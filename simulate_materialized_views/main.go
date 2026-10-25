package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Materialized views precompute an expensive query and store the result, so reads
// become cheap lookups. The trade-off is staleness: results only change on refresh.
//
// This program shows:
//   - raw aggregate vs materialized view read latency
//   - staleness until REFRESH
//   - plain REFRESH (blocks readers) vs REFRESH CONCURRENTLY (doesn't)
//   - periodic refresh with bounded staleness
//   - incremental refresh of a summary table with MERGE + a watermark

const rawQuery = `
	SELECT region, sum(amount) AS revenue
	FROM sales
	WHERE sold_at >= current_date - 30
	GROUP BY region ORDER BY region`

const mvQuery = `
	SELECT region, sum(revenue) AS revenue
	FROM daily_sales_mv
	WHERE day >= current_date - 30
	GROUP BY region ORDER BY region`

type App struct{ db *pgxpool.Pool }

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5505/shop?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()
	app := &App{db: db}
	app.resetDemo(ctx)
	fmt.Println("✅ Connected to shop (5505) — 1,000,000 rows in sales")

	banner("🧊 MATERIALIZED VIEWS SIMULATION")

	section("⚡ READ COST — raw aggregate vs materialized view")
	raw := app.timeQuery(ctx, rawQuery, 5)
	mv := app.timeQuery(ctx, mvQuery, 5)
	fmt.Printf("   raw GROUP BY over sales (1M rows):   %8.2f ms\n", ms(raw))
	fmt.Printf("   same answer from daily_sales_mv:     %8.2f ms\n", ms(mv))
	fmt.Printf("   → %.0fx faster; the MV holds ~%d pre-aggregated rows\n",
		float64(raw)/float64(mv), app.count(ctx, "daily_sales_mv"))

	section("🕰️  STALENESS — new sales are invisible until REFRESH")
	before := app.todayRevenue(ctx)
	app.insertSales(ctx, 1000)
	fmt.Printf("   inserted 1,000 sales for today\n")
	fmt.Printf("   today's revenue in MV: %s (unchanged — stale)\n", app.todayRevenue(ctx))
	took := app.refresh(ctx, false)
	fmt.Printf("   REFRESH MATERIALIZED VIEW took %.0f ms\n", ms(took))
	fmt.Printf("   today's revenue in MV: %s → %s\n", before, app.todayRevenue(ctx))

	section("🔒 REFRESH vs REFRESH CONCURRENTLY — do readers block?")
	app.readerDuringRefresh(ctx, false)
	app.readerDuringRefresh(ctx, true)

	section("⏱️  PERIODIC REFRESH — bounded staleness")
	app.periodicRefresh(ctx, 1*time.Second, 4*time.Second)

	section("➕ INCREMENTAL REFRESH — only process new rows (MERGE + watermark)")
	took = app.incrementalRefresh(ctx)
	fmt.Printf("   initial incremental load (all rows):  %6.0f ms\n", ms(took))
	app.insertSales(ctx, 1000)
	took = app.incrementalRefresh(ctx)
	fmt.Printf("   incremental refresh of 1,000 new rows: %6.0f ms\n", ms(took))
	took = app.refresh(ctx, true)
	fmt.Printf("   full REFRESH CONCURRENTLY for comparison: %6.0f ms\n", ms(took))
	app.verifyIncremental(ctx)

	section("📊 SUMMARY")
	fmt.Println("   Materialized view: one CREATE, reads become index lookups.")
	fmt.Println("   Refresh is a full recompute; CONCURRENTLY keeps readers unblocked.")
	fmt.Println("   For big tables, maintain the summary incrementally instead.")
}

// resetDemo removes rows added by earlier runs so timings and counts are stable.
func (a *App) resetDemo(ctx context.Context) {
	must(a.db.Exec(ctx, "DELETE FROM sales WHERE product = 'demo'"))
	must(a.db.Exec(ctx, "TRUNCATE daily_sales_agg"))
	must(a.db.Exec(ctx, "UPDATE agg_watermark SET last_id = 0"))
	must(a.db.Exec(ctx, "REFRESH MATERIALIZED VIEW daily_sales_mv"))
}

// timeQuery runs a query n times and returns the best (warm-cache) duration.
func (a *App) timeQuery(ctx context.Context, sql string, n int) time.Duration {
	best := time.Hour
	for range n {
		start := time.Now()
		rows, err := a.db.Query(ctx, sql)
		if err != nil {
			log.Fatalf("query: %v", err)
		}
		for rows.Next() {
		}
		rows.Close()
		best = min(best, time.Since(start))
	}
	return best
}

func (a *App) insertSales(ctx context.Context, n int) {
	must(a.db.Exec(ctx, `
		INSERT INTO sales (region, product, amount, sold_at)
		SELECT (ARRAY['north','south','east','west','central'])[1 + (g % 5)], 'demo', 100, now()
		FROM generate_series(1, $1) AS g`, n))
}

func (a *App) todayRevenue(ctx context.Context) string {
	var rev string
	if err := a.db.QueryRow(ctx,
		"SELECT coalesce(sum(revenue), 0)::text FROM daily_sales_mv WHERE day = current_date").Scan(&rev); err != nil {
		log.Fatal(err)
	}
	return rev
}

func (a *App) refresh(ctx context.Context, concurrently bool) time.Duration {
	sql := "REFRESH MATERIALIZED VIEW daily_sales_mv"
	if concurrently {
		sql = "REFRESH MATERIALIZED VIEW CONCURRENTLY daily_sales_mv"
	}
	start := time.Now()
	must(a.db.Exec(ctx, sql))
	return time.Since(start)
}

// readerDuringRefresh holds a refresh open inside a transaction (as a long refresh
// would be) and tries to read the view from another connection.
//   - plain REFRESH takes an ACCESS EXCLUSIVE lock → the reader waits.
//   - CONCURRENTLY takes an EXCLUSIVE lock → plain SELECTs proceed; it builds the new
//     result separately, then applies only the differences using the unique index.
func (a *App) readerDuringRefresh(ctx context.Context, concurrently bool) {
	label := "REFRESH"
	sql := "REFRESH MATERIALIZED VIEW daily_sales_mv"
	if concurrently {
		label = "REFRESH CONCURRENTLY"
		sql = "REFRESH MATERIALIZED VIEW CONCURRENTLY daily_sales_mv"
	}
	tx, err := a.db.Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, sql); err != nil {
		log.Fatalf("%s: %v", label, err)
	}

	// Reader with a lock_timeout so a blocked read fails fast instead of hanging.
	err = pgx.BeginFunc(ctx, a.db, func(r pgx.Tx) error {
		if _, err := r.Exec(ctx, "SET LOCAL lock_timeout = '500ms'"); err != nil {
			return err
		}
		var n int
		return r.QueryRow(ctx, "SELECT count(*) FROM daily_sales_mv").Scan(&n)
	})
	pgErr, isPgErr := errors.AsType[*pgconn.PgError](err)
	switch {
	case err == nil:
		fmt.Printf("   %-22s in progress → reader: ✅ served immediately\n", label)
	case isPgErr && pgErr.Code == "55P03": // lock_not_available
		fmt.Printf("   %-22s in progress → reader: ❌ BLOCKED (lock timeout after 500ms)\n", label)
	default:
		log.Fatalf("reader: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatal(err)
	}
}

// periodicRefresh mimics a scheduler (cron, pg_cron, a worker) refreshing every
// interval while writes keep arriving. Readers are never blocked, and the data they
// see is at most ~interval (+ refresh time) old.
func (a *App) periodicRefresh(ctx context.Context, interval, total time.Duration) {
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	var refreshes, writes, reads atomic.Int64
	var wg sync.WaitGroup

	wg.Go(func() { // refresher
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := a.db.Exec(ctx, "REFRESH MATERIALIZED VIEW CONCURRENTLY daily_sales_mv"); err == nil {
					refreshes.Add(1)
				}
			}
		}
	})
	wg.Go(func() { // writer
		for ctx.Err() == nil {
			if _, err := a.db.Exec(ctx, `INSERT INTO sales (region, product, amount, sold_at)
				VALUES ('north', 'demo', 100, now())`); err == nil {
				writes.Add(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	wg.Go(func() { // reader
		for ctx.Err() == nil {
			var region, revenue string
			if err := a.db.QueryRow(ctx, mvQuery+" LIMIT 1").Scan(&region, &revenue); err == nil {
				reads.Add(1)
			}
		}
	})
	wg.Wait()
	fmt.Printf("   %v run, refresh every %v: %d refreshes, %d writes, %d reads served\n",
		total, interval, refreshes.Load(), writes.Load(), reads.Load())
	fmt.Println("   readers never waited; any write shows up within ~1 refresh interval")
}

// incrementalRefresh folds only the sales rows added since the last run into
// daily_sales_agg. MERGE (PG15+) upserts each (region, day) group: add to existing
// totals or insert a new group. The watermark moves in the same transaction, so a
// crash can't double-count or skip rows.
func (a *App) incrementalRefresh(ctx context.Context) time.Duration {
	start := time.Now()
	err := pgx.BeginFunc(ctx, a.db, func(tx pgx.Tx) error {
		var from, to int64
		if err := tx.QueryRow(ctx,
			"SELECT last_id FROM agg_watermark WHERE name = 'daily_sales_agg' FOR UPDATE").Scan(&from); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, "SELECT coalesce(max(id), 0) FROM sales").Scan(&to); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			MERGE INTO daily_sales_agg AS t
			USING (
				SELECT region, sold_at::date AS day, count(*) AS orders, sum(amount) AS revenue
				FROM sales WHERE id > $1 AND id <= $2
				GROUP BY region, sold_at::date
			) AS s
			ON t.region = s.region AND t.day = s.day
			WHEN MATCHED THEN
				UPDATE SET orders = t.orders + s.orders, revenue = t.revenue + s.revenue
			WHEN NOT MATCHED THEN
				INSERT (region, day, orders, revenue) VALUES (s.region, s.day, s.orders, s.revenue)`,
			from, to); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE agg_watermark SET last_id = $1 WHERE name = 'daily_sales_agg'", to)
		return err
	})
	if err != nil {
		log.Fatalf("incremental refresh: %v", err)
	}
	return time.Since(start)
}

// verifyIncremental checks that the incrementally-maintained table and the fully
// refreshed materialized view agree exactly.
func (a *App) verifyIncremental(ctx context.Context) {
	var diff int
	if err := a.db.QueryRow(ctx, `
		SELECT count(*) FROM (
			(SELECT region, day, orders, revenue FROM daily_sales_mv
			 EXCEPT SELECT region, day, orders, revenue FROM daily_sales_agg)
			UNION ALL
			(SELECT region, day, orders, revenue FROM daily_sales_agg
			 EXCEPT SELECT region, day, orders, revenue FROM daily_sales_mv)
		) d`).Scan(&diff); err != nil {
		log.Fatal(err)
	}
	if diff == 0 {
		fmt.Println("   ✅ daily_sales_agg (incremental) == daily_sales_mv (full refresh)")
	} else {
		fmt.Printf("   ❌ %d differing rows between incremental and full results\n", diff)
	}
}

func (a *App) count(ctx context.Context, table string) int {
	var n int
	a.db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	return n
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func must[T any](_ T, err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

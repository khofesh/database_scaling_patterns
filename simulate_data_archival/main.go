package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Data archival moves rows that are no longer part of day-to-day work (old, closed
// orders) out of the hot table. The hot table — and its indexes, cache footprint,
// vacuum and backup time — shrinks back to the active working set, while the old
// data stays retrievable:
//
//   hot table  →  archive table (same DB, cold-tuned storage)  →  compressed files
//
// This program shows a batched, crash-safe archiver; storage parameters that make the
// archive table smaller; three retrieval strategies; and export to "object storage".

const exportDir = "/var/lib/postgresql/exports"

type App struct{ db *pgxpool.Pool }

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5531/shop?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()
	app := &App{db: db}
	fmt.Println("✅ Connected to shop (5531) — re-seeding 400k orders over 3 years...")
	app.exec(ctx, "CALL seed_orders()")
	app.exec(ctx, "VACUUM ANALYZE orders")

	banner("🗄️  DATA ARCHIVAL SIMULATION")

	section("🔥 BEFORE — everything lives in the hot table")
	app.printTables(ctx)
	scanBefore := app.timeQuery(ctx, "SELECT count(*) FROM orders WHERE total > 0")
	var old int
	must(db.QueryRow(ctx, "SELECT count(*) FROM orders WHERE created_at < now() - interval '1 year'").Scan(&old))
	fmt.Printf("   %d of 400000 orders (%.0f%%) are older than a year and closed\n", old, float64(old)/4000)

	section("📦 ARCHIVE — move delivered orders older than 1 year, in batches")
	app.archive(ctx, "1 year", 25000)

	section("🗜️  COMPACT — reclaim the hot table's space (pg_repack in production)")
	app.exec(ctx, "VACUUM FULL orders")
	app.exec(ctx, "ANALYZE orders, orders_archive")
	app.printTables(ctx)
	scanAfter := app.timeQuery(ctx, "SELECT count(*) FROM orders WHERE total > 0")
	fmt.Printf("   full-table scan (e.g. a nightly report): %.1f ms → %.1f ms\n", ms(scanBefore), ms(scanAfter))
	app.bytesPerRow(ctx)

	section("🔎 RETRIEVAL 1 — transparent: the orders_all view (hot UNION ALL archive)")
	rows, err := db.Query(ctx, `
		SELECT archived, count(*), min(created_at)::date::text, max(created_at)::date::text
		FROM orders_all WHERE customer_id = 42 GROUP BY archived ORDER BY archived`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var archived bool
		var n int
		var from, to string
		rows.Scan(&archived, &n, &from, &to)
		where := "hot    "
		if archived {
			where = "archive"
		}
		fmt.Printf("   customer 42, %s: %3d orders  (%s … %s)\n", where, n, from, to)
	}
	rows.Close()
	fmt.Println("   simple for the app; every query now spans two tables")

	section("🔎 RETRIEVAL 2 — hot first, archive on miss (lookup by id)")
	for _, id := range []int64{399990, 1234} {
		start := time.Now()
		where := app.findOrder(ctx, id)
		fmt.Printf("   order %-6d found in %-7s in %v\n", id, where, time.Since(start).Round(time.Microsecond))
	}

	section("♻️  RETRIEVAL 3 — restore an archived order (e.g. a late refund dispute)")
	app.restore(ctx, 1234)
	fmt.Printf("   order 1234 now in: %s\n", app.findOrder(ctx, 1234))

	section("🧊 COLD EXPORT — archive older than 2 years → gzip CSV, out of the DB")
	file := app.export(ctx, "2 years")

	section("🔎 RETRIEVAL 4 — query the cold file on demand")
	// Temp tables are per-session, so pin one connection for the whole sequence.
	conn, err := db.Acquire(ctx)
	if err != nil {
		log.Fatal(err)
	}
	start := time.Now()
	for _, sql := range []string{
		"CREATE TEMP TABLE cold_orders (LIKE orders_archive)",
		fmt.Sprintf("COPY cold_orders FROM PROGRAM 'gunzip -c %s' WITH (FORMAT csv)", file),
	} {
		if _, err := conn.Exec(ctx, sql); err != nil {
			log.Fatalf("%s: %v", sql, err)
		}
	}
	var n int
	var total string
	must(conn.QueryRow(ctx, "SELECT count(*), coalesce(sum(total), 0)::text FROM cold_orders WHERE customer_id = 42").Scan(&n, &total))
	conn.Release()
	fmt.Printf("   loaded file into a temp table and queried in %v: customer 42 has %d cold orders (total %s)\n",
		time.Since(start).Round(time.Millisecond), n, total)
	fmt.Println("   slow and manual — acceptable for audits and legal requests, not for the app")

	section("📊 SUMMARY")
	fmt.Println("   Hot table shrinks to the working set: faster scans, smaller indexes,")
	fmt.Println("   quicker vacuum and backups. Archive table is tuned for cold, append-only")
	fmt.Println("   data; files are cheapest of all. Retrieval gets slower at each tier.")
}

// archive moves qualifying rows in batches. Each batch is ONE statement:
//
//	WITH moved AS (DELETE … RETURNING *) INSERT INTO orders_archive SELECT … FROM moved
//
// so a row is either in the hot table or in the archive, never both or neither, even
// if the process dies mid-run. Small batches keep locks and WAL bursts short, and
// SKIP LOCKED lets the archiver run alongside normal traffic without waiting on rows
// that are being updated.
func (a *App) archive(ctx context.Context, age string, batch int) {
	start := time.Now()
	total := 0
	for i := 1; ; i++ {
		tag, err := a.db.Exec(ctx, fmt.Sprintf(`
			WITH batch AS (
				SELECT id FROM orders
				WHERE status = 'delivered' AND created_at < now() - interval '%s'
				ORDER BY id LIMIT %d
				FOR UPDATE SKIP LOCKED
			), moved AS (
				DELETE FROM orders o USING batch b WHERE o.id = b.id
				RETURNING o.*
			)
			INSERT INTO orders_archive (id, customer_id, status, total, created_at, details)
			SELECT id, customer_id, status, total, created_at, details FROM moved`, age, batch))
		if err != nil {
			log.Fatalf("archive batch: %v", err)
		}
		if tag.RowsAffected() == 0 {
			break
		}
		total += int(tag.RowsAffected())
		fmt.Printf("   batch %2d: moved %6d rows\n", i, tag.RowsAffected())
	}
	fmt.Printf("   archived %d rows in %v\n", total, time.Since(start).Round(time.Millisecond))
}

// findOrder checks the hot table first (where nearly all lookups hit) and only
// falls back to the archive on a miss.
func (a *App) findOrder(ctx context.Context, id int64) string {
	var x int64
	if err := a.db.QueryRow(ctx, "SELECT id FROM orders WHERE id = $1", id).Scan(&x); err == nil {
		return "hot"
	}
	if err := a.db.QueryRow(ctx, "SELECT id FROM orders_archive WHERE id = $1", id).Scan(&x); err == nil {
		return "archive"
	}
	return "nowhere"
}

// restore moves one order back to the hot table, atomically.
func (a *App) restore(ctx context.Context, id int64) {
	err := pgx.BeginFunc(ctx, a.db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			WITH back AS (DELETE FROM orders_archive WHERE id = $1 RETURNING *)
			INSERT INTO orders (id, customer_id, status, total, created_at, details)
			SELECT id, customer_id, 'disputed', total, created_at, details FROM back`, id)
		return err
	})
	if err != nil {
		log.Fatalf("restore: %v", err)
	}
	fmt.Printf("   order %d moved back to the hot table with status 'disputed'\n", id)
}

// export writes the oldest archive rows to a gzip-compressed CSV with server-side
// COPY … TO PROGRAM, then deletes them — in one transaction, so a failed export
// deletes nothing. The file stands in for an object-storage upload.
func (a *App) export(ctx context.Context, age string) string {
	file := fmt.Sprintf("%s/orders_before_%s.csv.gz", exportDir, time.Now().AddDate(-2, 0, 0).Format("2006_01"))
	var rows int64
	var tableBytes int64
	err := pgx.BeginFunc(ctx, a.db, func(tx pgx.Tx) error {
		cond := fmt.Sprintf("created_at < now() - interval '%s'", age)
		if err := tx.QueryRow(ctx, "SELECT count(*), coalesce(sum(pg_column_size(t.*)), 0) FROM orders_archive t WHERE "+cond).
			Scan(&rows, &tableBytes); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"COPY (SELECT * FROM orders_archive WHERE %s ORDER BY id) TO PROGRAM 'gzip > %s' WITH (FORMAT csv)",
			cond, file)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM orders_archive WHERE "+cond)
		return err
	})
	if err != nil {
		log.Fatalf("export: %v", err)
	}
	var fileBytes int64
	must(a.db.QueryRow(ctx, "SELECT size FROM pg_stat_file($1)", file).Scan(&fileBytes))
	fmt.Printf("   exported %d rows → %s\n", rows, file)
	fmt.Printf("   %.1f MB as rows → %.1f MB gzip file (%.0fx smaller), deleted from the DB\n",
		float64(tableBytes)/1e6, float64(fileBytes)/1e6, float64(tableBytes)/float64(fileBytes))
	return file
}

func (a *App) printTables(ctx context.Context) {
	rows, err := a.db.Query(ctx, `
		SELECT c.relname, c.reltuples::bigint,
		       pg_size_pretty(pg_table_size(c.oid)), pg_size_pretty(pg_indexes_size(c.oid))
		FROM pg_class c WHERE c.relname IN ('orders', 'orders_archive') ORDER BY c.relname`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, table, idx string
		var n int64
		rows.Scan(&name, &n, &table, &idx)
		fmt.Printf("   %-15s ~%7d rows   table %8s   indexes %8s\n", name, max(n, 0), table, idx)
	}
}

// bytesPerRow compares on-disk bytes per row for the table and its indexes: the
// archive packs pages full (fillfactor 100) and carries only two indexes.
func (a *App) bytesPerRow(ctx context.Context) {
	for _, t := range []string{"orders", "orders_archive"} {
		var heap, idx float64
		must(a.db.QueryRow(ctx, fmt.Sprintf(`
			SELECT pg_table_size('%[1]s')::float8 / greatest(count(*), 1),
			       pg_indexes_size('%[1]s')::float8 / greatest(count(*), 1)
			FROM %[1]s`, t)).Scan(&heap, &idx))
		fmt.Printf("   %-15s %4.0f bytes/row table + %3.0f bytes/row indexes\n", t, heap, idx)
	}
	fmt.Println("   archive: full pages (fillfactor 100) and 2 indexes instead of 4 — fine,")
	fmt.Println("   because archived rows are never updated and rarely searched")
}

func (a *App) timeQuery(ctx context.Context, sql string) time.Duration {
	best := time.Hour
	for range 3 {
		start := time.Now()
		var n int
		must(a.db.QueryRow(ctx, sql).Scan(&n))
		best = min(best, time.Since(start))
	}
	return best
}

func (a *App) exec(ctx context.Context, sql string) {
	if _, err := a.db.Exec(ctx, sql); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func must(err error) {
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

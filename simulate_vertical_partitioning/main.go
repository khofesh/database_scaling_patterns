package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Vertical partitioning splits a table by COLUMNS: hot columns (read on every
// request) in a narrow table, cold text and BLOBs in 1:1 side tables. The same 30k
// users are stored both ways, so each experiment runs against both layouts:
//
//   users_wide                         — everything in one row
//   users_core / users_profile / users_avatar — split vertically
//
// What we measure: table size, pages read by hot queries, bytes shipped by SELECT *,
// the cost of reassembling a full row, and WAL written when a hot column changes.

type App struct{ db *pgxpool.Pool }

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5520/app?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()
	app := &App{db: db}
	fmt.Println("✅ Connected to app (5520) — 30,000 users stored in both layouts")

	banner("🪓 VERTICAL PARTITIONING SIMULATION")

	section("📦 STORAGE — heap vs TOAST, rows per 8 KB page")
	app.printSizes(ctx)
	fmt.Println("   bio/preferences sit INLINE in users_wide (below the ~2 KB TOAST")
	fmt.Println("   threshold), so a page holds only a handful of rows; users_core ~100.")

	section("🔥 HOT QUERY — 'active users seen in the last 7 days'")
	hot := `SELECT count(*) FROM %s WHERE status = 'active' AND last_login > now() - interval '7 days'`
	app.compare(ctx, hot, "users_wide", "users_core")

	section("📋 LISTING — 50 most recently active users (id, name, email)")
	list := `SELECT id, name, email FROM %s ORDER BY last_login DESC LIMIT 50`
	app.compare(ctx, list, "users_wide", "users_core")

	section("🧳 SELECT * HABIT — 2,000 rows shipped to the app")
	app.selectStar(ctx, "users_wide")
	app.selectStar(ctx, "users_core")
	fmt.Println("   SELECT * on the wide table de-TOASTs every avatar and ships it")

	section("👤 PROFILE PAGE — reassembling one full user via the users_full view")
	app.timeOne(ctx, "users_wide WHERE id = 4242", "SELECT * FROM users_wide WHERE id = 4242")
	app.timeOne(ctx, "users_full (3-way PK join)", "SELECT * FROM users_full WHERE id = 4242")
	fmt.Println("   a 1:1 join on primary keys is cheap when you need it — rarely")

	section("✍️  HOT UPDATES — bump last_login for 10,000 users")
	app.walFor(ctx, "users_wide")
	app.walFor(ctx, "users_core")
	fmt.Println("   an UPDATE writes a whole new row version: wide rows mean more WAL,")
	fmt.Println("   more replication traffic, and faster bloat")

	section("📊 SUMMARY")
	fmt.Println("   Hot table stays small → more of it fits in RAM, scans read fewer pages.")
	fmt.Println("   Cold text and BLOBs are paid for only by the code paths that need them.")
	fmt.Println("   Cost: extra tables, 1:1 joins when you want everything, multi-table writes.")
}

func (a *App) printSizes(ctx context.Context) {
	rows, err := a.db.Query(ctx, `
		SELECT relname,
		       pg_size_pretty(pg_relation_size(oid)),
		       pg_size_pretty(pg_table_size(oid) - pg_relation_size(oid)),
		       round(reltuples / greatest(relpages, 1))::int
		FROM pg_class
		WHERE relname IN ('users_wide', 'users_core', 'users_profile', 'users_avatar')
		ORDER BY array_position(ARRAY['users_wide','users_core','users_profile','users_avatar'], relname::text)`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	fmt.Printf("   %-14s %10s %12s %10s\n", "table", "heap", "toast+fsm", "rows/page")
	for rows.Next() {
		var name, heap, toast string
		var perPage int
		rows.Scan(&name, &heap, &toast, &perPage)
		fmt.Printf("   %-14s %10s %12s %10d\n", name, heap, toast, perPage)
	}
}

// explain runs EXPLAIN (ANALYZE, BUFFERS) and returns pages touched and run time.
func (a *App) explain(ctx context.Context, sql string) (pages int, ms float64) {
	var raw []byte
	if err := a.db.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql).Scan(&raw); err != nil {
		log.Fatalf("explain: %v", err)
	}
	var out []struct {
		Plan struct {
			SharedHit  int `json:"Shared Hit Blocks"`
			SharedRead int `json:"Shared Read Blocks"`
		} `json:"Plan"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatal(err)
	}
	return out[0].Plan.SharedHit + out[0].Plan.SharedRead, out[0].ExecutionTime
}

func (a *App) compare(ctx context.Context, tmpl, wide, narrow string) {
	a.explain(ctx, fmt.Sprintf(tmpl, wide)) // warm the cache for a fair comparison
	a.explain(ctx, fmt.Sprintf(tmpl, narrow))
	wp, wt := a.explain(ctx, fmt.Sprintf(tmpl, wide))
	np, nt := a.explain(ctx, fmt.Sprintf(tmpl, narrow))
	fmt.Printf("   %-11s %6d pages (%5.1f MB)  %7.2f ms\n", wide, wp, mb(wp), wt)
	fmt.Printf("   %-11s %6d pages (%5.1f MB)  %7.2f ms\n", narrow, np, mb(np), nt)
	fmt.Printf("   → %.0fx fewer pages, %.1fx faster\n", float64(wp)/float64(max(np, 1)), wt/nt)
}

// selectStar fetches rows the way an ORM with SELECT * would and totals the bytes
// that actually crossed the wire.
func (a *App) selectStar(ctx context.Context, table string) {
	start := time.Now()
	rows, err := a.db.Query(ctx, fmt.Sprintf("SELECT * FROM %s ORDER BY id LIMIT 2000", table))
	if err != nil {
		log.Fatal(err)
	}
	var bytes int
	for rows.Next() {
		for _, v := range rows.RawValues() {
			bytes += len(v)
		}
	}
	rows.Close()
	fmt.Printf("   SELECT * FROM %-11s %6.2f MB received in %6.1f ms\n",
		table, float64(bytes)/1e6, float64(time.Since(start).Microseconds())/1000)
}

func (a *App) timeOne(ctx context.Context, label, sql string) {
	a.explain(ctx, sql)
	pages, ms := a.explain(ctx, sql)
	fmt.Printf("   %-28s %3d pages  %6.3f ms\n", label, pages, ms)
}

// walFor measures how many bytes of WAL an UPDATE of one small column produces.
func (a *App) walFor(ctx context.Context, table string) {
	var before string
	must(a.db.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&before))
	if _, err := a.db.Exec(ctx, fmt.Sprintf(
		"UPDATE %s SET last_login = now() WHERE id <= 10000", table)); err != nil {
		log.Fatal(err)
	}
	var wal int64
	must(a.db.QueryRow(ctx, "SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)::bigint", before).Scan(&wal))
	fmt.Printf("   UPDATE %-11s → %6.1f MB of WAL %s\n", table, float64(wal)/1e6, strings.Repeat("█", int(wal/1_000_000)))
	// Reclaim the dead row versions so re-runs start from a similar state.
	if _, err := a.db.Exec(ctx, "VACUUM "+table); err != nil {
		log.Fatal(err)
	}
}

func mb(pages int) float64 { return float64(pages) * 8192 / 1e6 }

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

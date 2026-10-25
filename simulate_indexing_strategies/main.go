package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Indexes are the first scaling tool: before adding servers, make each query read
// less. But each index type fits specific operators and data shapes, and every
// index costs disk, memory, write speed and maintenance. This program creates each
// kind on a 1M-row table and shows the plan and timing before and after:
//
//   B-tree      equality + ranges + sorting (the default)
//   Hash        equality only
//   Expression  index a computed value, e.g. lower(email)
//   Partial     index only the rows queries care about
//   Covering    INCLUDE extra columns → index-only scans
//   GIN         "contains" queries on jsonb / arrays / full-text
//   GiST        ranges & geometry: overlap, nearest-neighbour, exclusion constraints
//   BRIN        tiny index for huge, naturally ordered tables
//   + write overhead, bloat & REINDEX CONCURRENTLY, finding unused indexes

// Every index the demo creates, dropped at start so runs are repeatable.
var demoIndexes = []string{
	"idx_orders_customer", "idx_orders_email_btree", "idx_orders_email_hash",
	"idx_orders_email_lower", "idx_orders_status_created", "idx_orders_pending",
	"idx_orders_customer_cover", "idx_orders_attrs", "idx_orders_tags",
	"idx_orders_created_btree", "idx_orders_created_brin", "idx_orders_amount",
}

type App struct{ db *pgxpool.Pool }

type plan struct {
	node    string
	index   string
	ms      float64
	buffers int
	heap    int // heap fetches (index-only scans)
}

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5530/app?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer db.Close()
	app := &App{db: db}
	app.reset(ctx)
	fmt.Println("✅ Connected (5530) — orders: 1M rows, reservations: 100k rows, no secondary indexes")

	banner("🗂️  DATABASE INDEXING STRATEGIES")

	insertsBefore := app.timeInserts(ctx, 20000)

	section("🌳 B-TREE — equality on customer_id")
	q := "SELECT * FROM orders WHERE customer_id = 4242"
	app.before(ctx, q)
	app.create(ctx, "CREATE INDEX idx_orders_customer ON orders (customer_id)")
	app.after(ctx, q)

	section("#️⃣  HASH vs B-TREE — equality lookups on email")
	app.create(ctx, "CREATE INDEX idx_orders_email_btree ON orders (email)")
	app.create(ctx, "CREATE INDEX idx_orders_email_hash ON orders USING hash (email)")
	app.exec(ctx, "DROP INDEX idx_orders_email_btree")
	app.after(ctx, "SELECT * FROM orders WHERE email = 'User4242@Example.com'")
	app.after(ctx, "SELECT * FROM orders WHERE email > 'User9' ORDER BY email LIMIT 5")
	fmt.Println("   hash serves '=' only — range/ORDER BY fall back to a seq scan — and it isn't")
	fmt.Println("   even smaller here. B-tree is the safe default; hash pays off for long keys")

	section("🔡 EXPRESSION — case-insensitive lookup with lower(email)")
	q = "SELECT * FROM orders WHERE lower(email) = 'user4242@example.com'"
	app.before(ctx, q)
	fmt.Println("   (the index on email can't help: the query filters on lower(email))")
	app.create(ctx, "CREATE INDEX idx_orders_email_lower ON orders (lower(email))")
	app.after(ctx, q)

	section("🎯 PARTIAL — 'oldest 20 pending orders' (pending = 1% of rows)")
	q = "SELECT id, created_at FROM orders WHERE status = 'pending' ORDER BY created_at LIMIT 20"
	app.before(ctx, q)
	app.create(ctx, "CREATE INDEX idx_orders_status_created ON orders (status, created_at)")
	app.after(ctx, q)
	app.exec(ctx, "DROP INDEX idx_orders_status_created")
	app.create(ctx, "CREATE INDEX idx_orders_pending ON orders (created_at) WHERE status = 'pending'")
	app.after(ctx, q)
	fmt.Println("   same speed, a fraction of the size — and 'done' orders never touch it on write")

	section("🧾 COVERING — INCLUDE columns for an index-only scan")
	q = "SELECT customer_id, sum(amount) FROM orders WHERE customer_id BETWEEN 1000 AND 1100 GROUP BY customer_id"
	app.after(ctx, q)
	app.create(ctx, "CREATE INDEX idx_orders_customer_cover ON orders (customer_id) INCLUDE (amount)")
	app.exec(ctx, "VACUUM orders") // sets the visibility map so the heap can be skipped
	app.after(ctx, q)
	fmt.Println("   all needed columns are in the index → no heap visits (Heap Fetches: 0)")

	section("🧬 GIN — containment on jsonb and arrays")
	q = `SELECT count(*) FROM orders WHERE attrs @> '{"channel": "pos", "priority": true}'`
	app.before(ctx, q)
	app.create(ctx, "CREATE INDEX idx_orders_attrs ON orders USING gin (attrs jsonb_path_ops)")
	app.after(ctx, q)
	q = "SELECT count(*) FROM orders WHERE tags @> ARRAY['tag7', 'tag53']"
	app.before(ctx, q)
	app.create(ctx, "CREATE INDEX idx_orders_tags ON orders USING gin (tags)")
	app.after(ctx, q)

	section("📐 GiST — overlapping time ranges + exclusion constraint")
	q = "SELECT count(*) FROM reservations WHERE during && tstzrange('2025-06-05 10:00+00', '2025-06-05 12:00+00')"
	app.before(ctx, q)
	start := time.Now()
	app.exec(ctx, `ALTER TABLE reservations ADD CONSTRAINT no_double_booking
		EXCLUDE USING gist (room WITH =, during WITH &&)`)
	fmt.Printf("   + EXCLUDE USING gist (room WITH =, during WITH &&)  built in %v\n", time.Since(start).Round(time.Millisecond))
	app.after(ctx, q)
	_, err = db.Exec(ctx, `INSERT INTO reservations (room, during)
		VALUES (7, tstzrange('2025-06-01 01:00+00', '2025-06-01 03:00+00'))`)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		fmt.Printf("   double-book room 7 → ❌ %s (%s)\n", pgErr.Message, pgErr.Code)
	}

	section("🧱 BRIN vs B-TREE — time range on a naturally ordered 1M-row table")
	q = "SELECT count(*) FROM orders WHERE created_at >= '2025-06-01' AND created_at < '2025-06-02'"
	app.before(ctx, q)
	app.create(ctx, "CREATE INDEX idx_orders_created_btree ON orders (created_at)")
	app.after(ctx, q)
	app.exec(ctx, "DROP INDEX idx_orders_created_btree")
	app.create(ctx, "CREATE INDEX idx_orders_created_brin ON orders USING brin (created_at)")
	app.after(ctx, q)
	fmt.Println("   BRIN keeps only min/max per block range: ~1000x smaller than the B-tree,")
	fmt.Println("   still ~100x faster than a seq scan — as long as rows are stored in key order")

	section("✍️  WRITE OVERHEAD — every index must be updated on INSERT")
	// Someone adds an index "just in case". No query ever uses it — but every
	// write pays for it (it shows up again in the unused-index check below).
	app.create(ctx, "CREATE INDEX idx_orders_amount ON orders (amount)")
	insertsAfter := app.timeInserts(ctx, 20000)
	fmt.Printf("   20,000 inserts with only the PK:        %6.0f ms\n", ms(insertsBefore))
	fmt.Printf("   20,000 inserts with %2d indexes:         %6.0f ms  (%.1fx slower)\n",
		app.indexCount(ctx), ms(insertsAfter), float64(insertsAfter)/float64(insertsBefore))

	section("🫧 BLOAT — updates leave dead index entries; REINDEX CONCURRENTLY")
	app.printIndex(ctx, "idx_orders_customer", "fresh")
	app.exec(ctx, "UPDATE orders SET customer_id = customer_id + 1 WHERE id % 3 = 0")
	app.exec(ctx, "UPDATE orders SET customer_id = customer_id - 1 WHERE id % 3 = 0")
	app.exec(ctx, "VACUUM orders")
	app.printIndex(ctx, "idx_orders_customer", "after 2×333k updates + VACUUM")
	app.exec(ctx, "REINDEX INDEX CONCURRENTLY idx_orders_customer")
	app.printIndex(ctx, "idx_orders_customer", "after REINDEX CONCURRENTLY")
	fmt.Println("   VACUUM makes space reusable but doesn't shrink; REINDEX CONCURRENTLY")
	fmt.Println("   rebuilds without blocking reads or writes")

	section("🕵️  UNUSED INDEXES — pg_stat_user_indexes.idx_scan")
	app.printUsage(ctx)

	section("📊 SUMMARY")
	fmt.Println("   Pick the index type by operator: =/range/sort → B-tree, contains → GIN,")
	fmt.Println("   overlap/nearest → GiST, huge ordered tables → BRIN. Narrow with partial,")
	fmt.Println("   cover hot queries with INCLUDE, and drop indexes nobody uses.")
}

// explain runs EXPLAIN (ANALYZE, BUFFERS) and summarises the top-most interesting
// node: scan type, index used, time, pages touched and heap fetches.
func (a *App) explain(ctx context.Context, sql string) plan {
	var raw []byte
	if err := a.db.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql).Scan(&raw); err != nil {
		log.Fatalf("explain %s: %v", sql, err)
	}
	type node struct {
		NodeType    string `json:"Node Type"`
		IndexName   string `json:"Index Name"`
		HeapFetches int    `json:"Heap Fetches"`
		SharedHit   int    `json:"Shared Hit Blocks"`
		SharedRead  int    `json:"Shared Read Blocks"`
		Plans       []node `json:"Plans"`
	}
	var out []struct {
		Plan          node    `json:"Plan"`
		ExecutionTime float64 `json:"Execution Time"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Fatal(err)
	}
	p := plan{ms: out[0].ExecutionTime, buffers: out[0].Plan.SharedHit + out[0].Plan.SharedRead}
	// Walk down to the first scan node — that's where the index choice shows.
	var walk func(n node) bool
	walk = func(n node) bool {
		if strings.Contains(n.NodeType, "Scan") {
			p.node, p.index, p.heap = n.NodeType, n.IndexName, n.HeapFetches
			// A Bitmap Heap Scan names its index on the Bitmap Index Scan below it.
			if n.NodeType == "Bitmap Heap Scan" && len(n.Plans) > 0 {
				p.index = n.Plans[0].IndexName
			}
			return true
		}
		for _, c := range n.Plans {
			if walk(c) {
				return true
			}
		}
		return false
	}
	walk(out[0].Plan)
	return p
}

func (a *App) report(label string, p plan) {
	scan := p.node
	if p.index != "" {
		scan += " using " + p.index
	}
	extra := ""
	if p.node == "Index Only Scan" {
		extra = fmt.Sprintf(", Heap Fetches: %d", p.heap)
	}
	fmt.Printf("   %-6s %-55s %9.3f ms %7d pages%s\n", label, scan, p.ms, p.buffers, extra)
}

func (a *App) before(ctx context.Context, sql string) {
	a.explain(ctx, sql) // warm cache
	a.report("before", a.explain(ctx, sql))
}

func (a *App) after(ctx context.Context, sql string) {
	a.explain(ctx, sql)
	a.report("after", a.explain(ctx, sql))
}

// create builds an index and prints its build time and size.
func (a *App) create(ctx context.Context, ddl string) {
	start := time.Now()
	a.exec(ctx, ddl)
	took := time.Since(start)
	name := strings.Fields(ddl)[2]
	var size string
	if err := a.db.QueryRow(ctx, "SELECT pg_size_pretty(pg_relation_size($1::regclass))", name).Scan(&size); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   + %-28s %8s  built in %v\n", name, size, took.Round(time.Millisecond))
}

func (a *App) printIndex(ctx context.Context, name, label string) {
	var size string
	var density float64
	if err := a.db.QueryRow(ctx, `
		SELECT pg_size_pretty(pg_relation_size($1::regclass)), avg_leaf_density
		FROM pgstatindex($1)`, name).Scan(&size, &density); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("   %-30s %8s   leaf density %5.1f%%\n", label+":", size, density)
}

func (a *App) printUsage(ctx context.Context) {
	rows, err := a.db.Query(ctx, `
		SELECT s.indexrelname, s.idx_scan, pg_size_pretty(pg_relation_size(s.indexrelid))
		FROM pg_stat_user_indexes s
		JOIN pg_index i ON i.indexrelid = s.indexrelid
		WHERE s.relname = 'orders'
		  AND NOT i.indisunique  -- unique/PK indexes enforce constraints even if never scanned
		ORDER BY s.idx_scan, s.indexrelname`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, size string
		var scans int64
		rows.Scan(&name, &scans, &size)
		flag := ""
		if scans == 0 {
			flag = "← never used: drop candidate"
		}
		fmt.Printf("   %-28s scans=%-6d %8s %s\n", name, scans, size, flag)
	}
}

// timeInserts inserts n rows in one statement and removes them again.
func (a *App) timeInserts(ctx context.Context, n int) time.Duration {
	start := time.Now()
	a.exec(ctx, fmt.Sprintf(`
		INSERT INTO orders (customer_id, email, status, amount, created_at, attrs, tags)
		SELECT g %% 100000, 'Bench' || g || '@Example.com', 'done', 1, now(),
		       '{"channel": "web"}', ARRAY['tag1']
		FROM generate_series(1, %d) g`, n))
	took := time.Since(start)
	a.exec(ctx, "DELETE FROM orders WHERE email LIKE 'Bench%'")
	return took
}

func (a *App) indexCount(ctx context.Context) int {
	var n int
	a.db.QueryRow(ctx, "SELECT count(*) FROM pg_indexes WHERE tablename = 'orders'").Scan(&n)
	return n
}

func (a *App) reset(ctx context.Context) {
	for _, idx := range demoIndexes {
		a.exec(ctx, "DROP INDEX IF EXISTS "+idx)
	}
	a.exec(ctx, "ALTER TABLE reservations DROP CONSTRAINT IF EXISTS no_double_booking")
	// Rewrite the table in id (= created_at) order: undoes bloat from the previous
	// run's UPDATEs and restores the physical ordering BRIN relies on.
	a.exec(ctx, "CLUSTER orders USING orders_pkey")
	a.exec(ctx, "VACUUM ANALYZE orders")
	a.exec(ctx, "SELECT pg_stat_reset()")
}

func (a *App) exec(ctx context.Context, sql string) {
	if _, err := a.db.Exec(ctx, sql); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

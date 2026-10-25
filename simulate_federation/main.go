package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Federation (functional partitioning) splits the database by BUSINESS DOMAIN:
// users in one database, orders in another, analytics in a third. Each can be
// sized, tuned, scaled and owned separately, and a load spike in one domain no
// longer competes with the others for the same server.
//
// What you give up is the single database: no JOINs across domains, no foreign keys
// between them, no transaction spanning them. This program shows both sides:
//   - domain routing in the application
//   - cross-database "joins" done in the app (two round trips)
//   - cross-database SQL with postgres_fdw, and what it pushes down
//   - orphaned references once foreign keys are gone

type Federation struct {
	users     *pgxpool.Pool // accounts domain
	orders    *pgxpool.Pool // commerce domain
	analytics *pgxpool.Pool // reporting: federates the other two via postgres_fdw
}

type topCustomer struct {
	userID  int64
	name    string
	country string
	spend   float64
}

func main() {
	ctx := context.Background()
	f := &Federation{
		users:     open(ctx, "postgres://postgres:postgres@localhost:5522/users?sslmode=disable"),
		orders:    open(ctx, "postgres://postgres:postgres@localhost:5523/orders?sslmode=disable"),
		analytics: open(ctx, "postgres://postgres:postgres@localhost:5524/analytics?sslmode=disable"),
	}
	defer f.users.Close()
	defer f.orders.Close()
	defer f.analytics.Close()
	f.orders.Exec(ctx, "DELETE FROM orders WHERE user_id > 5000") // undo earlier runs
	fmt.Println("✅ Connected to users_db (5522), orders_db (5523), analytics_db (5524)")

	banner("🏛️  FEDERATION / FUNCTIONAL PARTITIONING SIMULATION")

	section("🧭 DOMAIN ROUTING — each request goes to its domain's database")
	f.printSizes(ctx)
	var name string
	must(f.users.QueryRow(ctx, "SELECT name FROM users WHERE id = 42").Scan(&name))
	fmt.Printf("   GET /users/42        → users_db:  %s\n", name)
	id := f.placeOrder(ctx, 42, 99.90)
	fmt.Printf("   POST /orders         → users_db (validate user) + orders_db (insert id=%d)\n", id)

	section("🧩 CROSS-DOMAIN QUERY, IN THE APP — 'top 5 customers by spend'")
	start := time.Now()
	top := f.topCustomersAppJoin(ctx, 5)
	appTook := time.Since(start)
	printTop(top)
	fmt.Printf("   2 round trips (orders_db aggregate → users_db lookup by id) in %v\n", appTook.Round(time.Microsecond))

	section("🔗 CROSS-DOMAIN QUERY, WITH postgres_fdw — same question, one SQL")
	fdwSQL := `
		SELECT o.user_id, u.name, u.country, o.spend
		FROM (SELECT user_id, sum(amount)::float8 AS spend
		      FROM orders_remote.orders GROUP BY user_id ORDER BY spend DESC LIMIT 5) o
		JOIN users_remote.users u ON u.id = o.user_id
		ORDER BY o.spend DESC`
	start = time.Now()
	top = f.query(ctx, fdwSQL)
	fdwTook := time.Since(start)
	printTop(top)
	fmt.Printf("   one query on analytics_db in %v. What it sent to the remotes:\n", fdwTook.Round(time.Microsecond))
	f.printRemoteSQL(ctx, fdwSQL)
	fmt.Println("   GROUP BY/ORDER BY/LIMIT ran on orders_db — only 5 rows came back")

	section("🌍 WHEN FEDERATED JOINS GET EXPENSIVE — 'revenue by country'")
	countrySQL := `
		SELECT u.country, sum(o.amount)::float8
		FROM orders_remote.orders o
		JOIN users_remote.users u ON u.id = o.user_id
		GROUP BY u.country ORDER BY 2 DESC`
	start = time.Now()
	rows, err := f.analytics.Query(ctx, countrySQL)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var country string
		var revenue float64
		rows.Scan(&country, &revenue)
		fmt.Printf("   %s  %12.2f\n", country, revenue)
	}
	rows.Close()
	fmt.Printf("   took %v — the join spans two servers, so rows are shipped to analytics:\n",
		time.Since(start).Round(time.Millisecond))
	f.printRemoteSQL(ctx, countrySQL)
	fmt.Println("   → for heavy reporting, replicate domain data INTO analytics (CDC/ETL)")

	section("💔 LOST GUARANTEE — no foreign keys across databases")
	// A buggy client (or a race with a user deletion) writes an order for a user
	// that doesn't exist. orders_db can't check: users live elsewhere.
	_, err = f.orders.Exec(ctx, "INSERT INTO orders (user_id, amount) VALUES (999999, 10)")
	fmt.Printf("   INSERT order for user 999999 → accepted (err=%v)\n", err)
	var orphans int
	must(f.analytics.QueryRow(ctx, `
		SELECT count(*) FROM orders_remote.orders o
		WHERE NOT EXISTS (SELECT 1 FROM users_remote.users u WHERE u.id = o.user_id)`).Scan(&orphans))
	fmt.Printf("   orphan check via fdw: %d order(s) reference a missing user ❌\n", orphans)
	fmt.Println("   integrity moves to the app: validate on write, reconcile periodically,")
	fmt.Println("   and use sagas/outbox events instead of cross-database transactions")

	section("📊 SUMMARY")
	fmt.Println("   + each domain scales, tunes and fails independently; clear ownership")
	fmt.Println("   + smaller databases: faster backups, simpler maintenance")
	fmt.Println("   − no cross-domain JOIN/FK/transaction; joins move to app, fdw or ETL")
	fmt.Printf("   − app join %v, fdw %v here: every cross-domain read pays network hops\n",
		appTook.Round(time.Microsecond), fdwTook.Round(time.Microsecond))
}

// placeOrder spans two domains: validate the user in users_db, then insert into
// orders_db. These are two separate transactions — nothing makes them atomic.
func (f *Federation) placeOrder(ctx context.Context, userID int64, amount float64) int64 {
	var exists bool
	must(f.users.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)", userID).Scan(&exists))
	if !exists {
		log.Fatalf("user %d not found", userID)
	}
	var id int64
	must(f.orders.QueryRow(ctx,
		"INSERT INTO orders (user_id, amount) VALUES ($1, $2) RETURNING id", userID, amount).Scan(&id))
	return id
}

// topCustomersAppJoin performs the "join" in Go: aggregate in orders_db, then fetch
// the matching users from users_db in ONE batched query (not N+1).
func (f *Federation) topCustomersAppJoin(ctx context.Context, n int) []topCustomer {
	rows, err := f.orders.Query(ctx, `
		SELECT user_id, sum(amount)::float8 FROM orders
		GROUP BY user_id ORDER BY 2 DESC LIMIT $1`, n)
	if err != nil {
		log.Fatal(err)
	}
	top, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (topCustomer, error) {
		var t topCustomer
		return t, r.Scan(&t.userID, &t.spend)
	})
	if err != nil {
		log.Fatal(err)
	}
	ids := make([]int64, len(top))
	for i, t := range top {
		ids[i] = t.userID
	}

	users := map[int64][2]string{}
	rows, err = f.users.Query(ctx, "SELECT id, name, country FROM users WHERE id = ANY($1)", ids)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var name, country string
		rows.Scan(&id, &name, &country)
		users[id] = [2]string{name, country}
	}
	rows.Close()
	for i := range top {
		top[i].name, top[i].country = users[top[i].userID][0], users[top[i].userID][1]
	}
	return top
}

func (f *Federation) query(ctx context.Context, sql string) []topCustomer {
	rows, err := f.analytics.Query(ctx, sql)
	if err != nil {
		log.Fatal(err)
	}
	top, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (topCustomer, error) {
		var t topCustomer
		return t, r.Scan(&t.userID, &t.name, &t.country, &t.spend)
	})
	if err != nil {
		log.Fatal(err)
	}
	return top
}

// printRemoteSQL shows the "Remote SQL" lines of EXPLAIN VERBOSE: exactly what
// postgres_fdw sends to each remote server. Pushed-down GROUP BY/LIMIT/WHERE means
// little data crosses the network; a bare "SELECT cols FROM table" means it all does.
func (f *Federation) printRemoteSQL(ctx context.Context, sql string) {
	rows, err := f.analytics.Query(ctx, "EXPLAIN (VERBOSE) "+sql)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		rows.Scan(&line)
		if i := strings.Index(line, "Remote SQL:"); i >= 0 {
			fmt.Printf("     ↳ %s\n", strings.TrimSpace(line[i:]))
		}
	}
}

func (f *Federation) printSizes(ctx context.Context) {
	for _, db := range []struct {
		name string
		pool *pgxpool.Pool
	}{{"users_db", f.users}, {"orders_db", f.orders}, {"analytics_db", f.analytics}} {
		var size string
		must(db.pool.QueryRow(ctx, "SELECT pg_size_pretty(pg_database_size(current_database()))").Scan(&size))
		fmt.Printf("   %-13s %8s  (separate server, own pool, own tuning)\n", db.name, size)
	}
}

func printTop(top []topCustomer) {
	for _, t := range top {
		fmt.Printf("   user %-5d %-10s %s  spend %9.2f\n", t.userID, t.name, t.country, t.spend)
	}
}

func open(ctx context.Context, url string) *pgxpool.Pool {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("ping %s: %v", url, err)
	}
	return db
}

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

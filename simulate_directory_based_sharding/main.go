package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Directory-based sharding routes each key (a tenant here) through a LOOKUP TABLE
// instead of a formula like hash(key) % N. Because placement is data, not math:
//   - a new tenant can be put on whichever shard is least loaded right now
//   - adding a shard moves nobody — existing mappings don't change
//   - one tenant (e.g. a hotspot) can be moved by copying its rows and flipping a row
// The price: an extra lookup per request (mitigated by caching), and the directory
// becomes critical infrastructure that must be highly available.

const directoryURL = "postgres://postgres:postgres@localhost:5510/directory?sslmode=disable"

type entry struct {
	shard   string
	version int
}

// Router is what each application instance embeds: directory access, a pool per
// shard, and a local cache of tenant → shard mappings.
type Router struct {
	name   string
	dir    *pgxpool.Pool
	pools  map[string]*pgxpool.Pool
	cache  map[string]entry
	hits   int
	misses int
}

func NewRouter(ctx context.Context, name string) *Router {
	return &Router{name: name, dir: open(ctx, directoryURL), pools: map[string]*pgxpool.Pool{}, cache: map[string]entry{}}
}

func (r *Router) Close() {
	for _, p := range r.pools {
		p.Close()
	}
	r.dir.Close()
}

// shard returns (lazily opening) the pool for a shard, using the DSN stored in the
// directory — new shards become reachable without redeploying the app.
func (r *Router) shard(ctx context.Context, name string) *pgxpool.Pool {
	if p, ok := r.pools[name]; ok {
		return p
	}
	var dsn string
	if err := r.dir.QueryRow(ctx, "SELECT dsn FROM shards WHERE name = $1", name).Scan(&dsn); err != nil {
		log.Fatalf("unknown shard %s: %v", name, err)
	}
	r.pools[name] = open(ctx, dsn)
	return r.pools[name]
}

// lookup resolves a tenant to its shard, from cache when possible.
func (r *Router) lookup(ctx context.Context, tenant string) entry {
	if e, ok := r.cache[tenant]; ok {
		r.hits++
		return e
	}
	r.misses++
	var e entry
	var status string
	if err := r.dir.QueryRow(ctx,
		"SELECT shard, version, status FROM tenant_directory WHERE tenant_id = $1", tenant).Scan(&e.shard, &e.version, &status); err != nil {
		log.Fatalf("lookup %s: %v", tenant, err)
	}
	if status == "active" { // never cache a mapping that's mid-move
		r.cache[tenant] = e
	}
	return e
}

// Onboard places a NEW tenant on the open shard holding the fewest rows right now —
// dynamic assignment that a hash function can't do.
func (r *Router) Onboard(ctx context.Context, tenant string) string {
	var target string
	var best int64 = -1
	rows, err := r.dir.Query(ctx, "SELECT name FROM shards WHERE status = 'open' ORDER BY name")
	if err != nil {
		log.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		log.Fatal(err)
	}
	for _, name := range names {
		var n int64
		r.shard(ctx, name).QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&n)
		if best < 0 || n < best {
			target, best = name, n
		}
	}
	must(r.shard(ctx, target).Exec(ctx, "INSERT INTO hosted_tenants VALUES ($1)", tenant))
	must(r.dir.Exec(ctx, "INSERT INTO tenant_directory (tenant_id, shard) VALUES ($1, $2)", tenant, target))
	return target
}

// InsertOrders writes n orders for a tenant on its shard. The INSERT only succeeds
// if the shard still hosts the tenant (FOR SHARE also makes a concurrent move wait
// for us). Zero rows inserted ⇒ our cached mapping is stale: drop it and retry.
func (r *Router) InsertOrders(ctx context.Context, tenant string, n int) (string, bool) {
	for attempt := range 2 {
		e := r.lookup(ctx, tenant)
		tag, err := r.shard(ctx, e.shard).Exec(ctx, `
			INSERT INTO orders (tenant_id, id, amount)
			SELECT $1, coalesce((SELECT max(id) FROM orders WHERE tenant_id = $1), 0) + g,
			       round((random() * 100 + 1)::numeric, 2)
			FROM generate_series(1, $2) AS g
			WHERE EXISTS (SELECT 1 FROM hosted_tenants WHERE tenant_id = $1 FOR SHARE)`, tenant, n)
		if err != nil {
			log.Fatalf("insert %s: %v", tenant, err)
		}
		if tag.RowsAffected() > 0 {
			return e.shard, attempt > 0
		}
		delete(r.cache, tenant) // fenced out: stale mapping
	}
	log.Fatalf("tenant %s is mid-move; caller should retry later", tenant)
	return "", false
}

func (r *Router) CountOrders(ctx context.Context, tenant string) int {
	e := r.lookup(ctx, tenant)
	var n int
	r.shard(ctx, e.shard).QueryRow(ctx, "SELECT count(*) FROM orders WHERE tenant_id = $1", tenant).Scan(&n)
	return n
}

// MoveTenant migrates one tenant to another shard while others keep running.
//  1. directory: mark 'moving' (new lookups won't be cached)
//  2. source: delete the hosted_tenants row inside an open tx — the fence goes up and
//     concurrent writers block on it, then fail and re-route
//  3. copy the tenant's rows to the target (COPY protocol) + host it there; commit
//  4. source: delete the rows; commit
//  5. directory: point at the target, bump the version, mark 'active'
func (r *Router) MoveTenant(ctx context.Context, tenant, target string) (int64, time.Duration) {
	start := time.Now()
	var source string
	if err := r.dir.QueryRow(ctx, `
		UPDATE tenant_directory SET status = 'moving', updated_at = now()
		WHERE tenant_id = $1 AND status = 'active' RETURNING shard`, tenant).Scan(&source); err != nil {
		log.Fatalf("mark moving: %v", err)
	}

	srcTx, err := r.shard(ctx, source).Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer srcTx.Rollback(ctx)
	must(srcTx.Exec(ctx, "DELETE FROM hosted_tenants WHERE tenant_id = $1", tenant))

	rows, err := srcTx.Query(ctx, "SELECT tenant_id, id, amount, created_at FROM orders WHERE tenant_id = $1", tenant)
	if err != nil {
		log.Fatal(err)
	}
	data, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([]any, error) { return row.Values() })
	if err != nil {
		log.Fatal(err)
	}

	err = pgx.BeginFunc(ctx, r.shard(ctx, target), func(tx pgx.Tx) error {
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"orders"},
			[]string{"tenant_id", "id", "amount", "created_at"}, pgx.CopyFromRows(data)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO hosted_tenants VALUES ($1)", tenant)
		return err
	})
	if err != nil {
		log.Fatalf("copy to %s: %v", target, err)
	}

	must(srcTx.Exec(ctx, "DELETE FROM orders WHERE tenant_id = $1", tenant))
	if err := srcTx.Commit(ctx); err != nil {
		log.Fatal(err)
	}

	must(r.dir.Exec(ctx, `
		UPDATE tenant_directory SET shard = $2, status = 'active', version = version + 1, updated_at = now()
		WHERE tenant_id = $1`, tenant, target))
	delete(r.cache, tenant)
	return int64(len(data)), time.Since(start)
}

func main() {
	ctx := context.Background()
	app := NewRouter(ctx, "app-1")
	defer app.Close()
	reset(ctx, app)
	fmt.Println("✅ Connected to directory (5510); shards discovered from it")

	banner("📒 DIRECTORY-BASED SHARDING SIMULATION")

	section("🆕 ONBOARD — each new tenant goes to the least-loaded shard")
	volumes := map[string]int{
		"acme": 300, "globex": 250, "initech": 200, "megacorp": 6000, "hooli": 150, "umbrella": 100,
	}
	for _, t := range []string{"acme", "globex", "initech", "megacorp", "hooli", "umbrella"} {
		shard := app.Onboard(ctx, t)
		app.InsertOrders(ctx, t, volumes[t])
		fmt.Printf("   %-9s → %s  (%d orders)\n", t, shard, volumes[t])
	}
	printDirectory(ctx, app)
	printShardLoad(ctx, app)
	fmt.Println("   ⚠️  megacorp alone makes its shard a HOTSPOT")

	section("🔁 CACHED ROUTING — the directory is consulted once per tenant")
	app.hits, app.misses = 0, 0
	clear(app.cache)
	for range 50 {
		for t := range volumes {
			app.CountOrders(ctx, t)
		}
	}
	fmt.Printf("   300 routed reads: %d directory lookups, %d cache hits\n", app.misses, app.hits)

	// A second app instance that cached megacorp's location BEFORE the move.
	stale := NewRouter(ctx, "app-2")
	defer stale.Close()
	stale.lookup(ctx, "megacorp")

	section("➕ ADD CAPACITY — register shard3; nobody is rehashed")
	must(app.dir.Exec(ctx, "INSERT INTO shards (name, dsn) VALUES ('shard3', 'postgres://postgres:postgres@localhost:5513/app?sslmode=disable')"))
	fmt.Println("   INSERT INTO shards ('shard3', …) — existing tenants untouched")

	section("🚚 MOVE A HOTSPOT — migrate megacorp to dedicated shard3")
	n, took := app.MoveTenant(ctx, "megacorp", "shard3")
	fmt.Printf("   moved %d rows in %v; only megacorp's rows moved, nobody else's\n", n, took.Round(time.Millisecond))
	must(app.dir.Exec(ctx, "UPDATE shards SET status = 'closed' WHERE name = 'shard3'"))
	fmt.Println("   shard3 marked 'closed' — dedicated to megacorp, no new tenants")
	printDirectory(ctx, app)
	printShardLoad(ctx, app)

	section("🧟 STALE CACHE — app-2 still thinks megacorp lives on its old shard")
	fmt.Printf("   app-2 cache: megacorp → %s (v%d)\n", stale.cache["megacorp"].shard, stale.cache["megacorp"].version)
	shard, retried := stale.InsertOrders(ctx, "megacorp", 10)
	fmt.Printf("   write fenced out by hosted_tenants on the old shard → refreshed → wrote to %s (retried=%v)\n", shard, retried)
	fmt.Printf("   app-2 cache now: megacorp → %s (v%d)\n", stale.cache["megacorp"].shard, stale.cache["megacorp"].version)

	section("🆕 ONBOARD AGAIN — placement adapts to current load")
	for _, t := range []string{"wayne", "stark"} {
		fmt.Printf("   %-9s → %s\n", t, app.Onboard(ctx, t))
		app.InsertOrders(ctx, t, 100)
	}

	section("🆚 VS HASH ROUTING — what adding a shard would have cost")
	compareWithHash(slices.Collect(maps.Keys(volumes)))

	section("📊 SUMMARY")
	fmt.Println("   + any tenant on any shard; move one by copying rows + flipping a row")
	fmt.Println("   + add shards without rehashing; place new tenants by live load")
	fmt.Println("   − extra lookup per request (cache it) and a directory that must be HA")
	fmt.Println("   − caches go stale on moves: shards must fence writes they don't own")
}

// compareWithHash shows how many tenants hash(tenant) % N would relocate when going
// from 2 to 3 shards — versus zero with a directory.
func compareWithHash(tenants []string) {
	slices.Sort(tenants)
	moved := 0
	for _, t := range tenants {
		h := fnv.New32a()
		h.Write([]byte(t))
		before, after := h.Sum32()%2, h.Sum32()%3
		mark := ""
		if before != after {
			moved++
			mark = "← moves"
		}
		fmt.Printf("   %-9s hash%%2 → shard%d   hash%%3 → shard%d  %s\n", t, before+1, after+1, mark)
	}
	fmt.Printf("   hash routing: %d/%d tenants must move. directory: 0 (we moved 1, by choice)\n", moved, len(tenants))
}

func printDirectory(ctx context.Context, r *Router) {
	rows, err := r.dir.Query(ctx, "SELECT tenant_id, shard, version FROM tenant_directory ORDER BY shard, tenant_id")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	fmt.Println("   tenant_directory:")
	for rows.Next() {
		var t, s string
		var v int
		rows.Scan(&t, &s, &v)
		fmt.Printf("     %-9s %s  v%d\n", t, s, v)
	}
}

func printShardLoad(ctx context.Context, r *Router) {
	rows, err := r.dir.Query(ctx, "SELECT name FROM shards ORDER BY name")
	if err != nil {
		log.Fatal(err)
	}
	names, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	for _, name := range names {
		var n int
		r.shard(ctx, name).QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&n)
		bar := ""
		for range n / 250 {
			bar += "█"
		}
		fmt.Printf("   %-7s %5d rows %s\n", name, n, bar)
	}
}

// reset returns all databases to their initial state so the demo is repeatable.
func reset(ctx context.Context, r *Router) {
	for _, s := range []string{"shard1", "shard2"} {
		must(r.shard(ctx, s).Exec(ctx, "TRUNCATE orders, hosted_tenants"))
	}
	shard3 := open(ctx, "postgres://postgres:postgres@localhost:5513/app?sslmode=disable")
	must(shard3.Exec(ctx, "TRUNCATE orders, hosted_tenants"))
	shard3.Close()
	must(r.dir.Exec(ctx, "TRUNCATE tenant_directory"))
	must(r.dir.Exec(ctx, "DELETE FROM shards WHERE name = 'shard3'"))
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

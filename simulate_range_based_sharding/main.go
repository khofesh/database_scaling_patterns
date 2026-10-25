package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Range-based sharding assigns each shard a contiguous RANGE of the shard key — here
// order date (created_at). Neighbouring keys live together, so range queries
// ("orders in March") touch one shard instead of all of them. The downside is that
// load follows the key distribution: with a time key, every new write lands in the
// newest range — a write HOTSPOT — and ranges grow unevenly, so they must be SPLIT
// and moved over time.

// Range maps [From, To) of created_at to one or more shards. To.IsZero() means
// "unbounded". With several shards, rows are spread inside the range by
// customer_id % len(Shards) (a "hash-prefixed range", see the last demo step).
type Range struct {
	From, To time.Time
	Shards   []string
}

func (r Range) contains(t time.Time) bool {
	return !t.Before(r.From) && (r.To.IsZero() || t.Before(r.To))
}

func (r Range) overlaps(from, to time.Time) bool {
	return (r.To.IsZero() || from.Before(r.To)) && to.After(r.From)
}

func (r Range) String() string {
	to := "∞"
	if !r.To.IsZero() {
		to = r.To.Format("2006-01-02")
	}
	return fmt.Sprintf("[%s, %-10s) → %s", r.From.Format("2006-01-02"), to, strings.Join(r.Shards, " + "))
}

type Cluster struct {
	pools  map[string]*pgxpool.Pool
	ranges []Range // sorted by From; the application-side range map
}

// route returns the shard for a row: find its range, then pick within the range.
func (c *Cluster) route(createdAt time.Time, customer int) string {
	for _, r := range c.ranges {
		if r.contains(createdAt) {
			return r.Shards[customer%len(r.Shards)]
		}
	}
	log.Fatalf("no range covers %v", createdAt)
	return ""
}

// shardsFor returns every shard whose range overlaps [from, to) — the only shards a
// range query needs to visit.
func (c *Cluster) shardsFor(from, to time.Time) []string {
	var out []string
	for _, r := range c.ranges {
		if r.overlaps(from, to) {
			for _, s := range r.Shards {
				if !slices.Contains(out, s) {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

type order struct {
	customer  int
	amount    float64
	createdAt time.Time
}

// insert routes each order to its shard and bulk-loads per shard with COPY. It
// returns how many rows each shard received.
func (c *Cluster) insert(ctx context.Context, orders []order) map[string]int {
	byShard := map[string][][]any{}
	for _, o := range orders {
		s := c.route(o.createdAt, o.customer)
		byShard[s] = append(byShard[s], []any{o.customer, o.amount, o.createdAt})
	}
	counts := map[string]int{}
	for s, rows := range byShard {
		if _, err := c.pools[s].CopyFrom(ctx, pgx.Identifier{"orders"},
			[]string{"customer_id", "amount", "created_at"}, pgx.CopyFromRows(rows)); err != nil {
			log.Fatalf("copy into %s: %v", s, err)
		}
		counts[s] = len(rows)
	}
	return counts
}

// splitRange cuts the range containing `at` in two and moves the upper half to a new
// shard. Only rows in [at, end of range) travel; every other range is untouched.
func (c *Cluster) splitRange(ctx context.Context, at time.Time, newShard string) int64 {
	i := slices.IndexFunc(c.ranges, func(r Range) bool { return r.contains(at) })
	old := c.ranges[i]
	src := old.Shards[0]

	tx, err := c.pools[src].Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback(ctx)
	// DELETE … RETURNING both removes and hands us the rows to copy, atomically with
	// the commit below (the copy commits first; if it fails we roll back the delete).
	rows, err := tx.Query(ctx, `
		DELETE FROM orders WHERE created_at >= $1
		RETURNING id, customer_id, amount, created_at`, at)
	if err != nil {
		log.Fatal(err)
	}
	moved, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([]any, error) { return r.Values() })
	if err != nil {
		log.Fatal(err)
	}
	if _, err := c.pools[newShard].CopyFrom(ctx, pgx.Identifier{"orders"},
		[]string{"id", "customer_id", "amount", "created_at"}, pgx.CopyFromRows(moved)); err != nil {
		log.Fatalf("copy to %s: %v", newShard, err)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatal(err)
	}

	lower := Range{From: old.From, To: at, Shards: old.Shards}
	upper := Range{From: at, To: old.To, Shards: []string{newShard}}
	c.ranges = slices.Replace(c.ranges, i, i+1, lower, upper)
	return int64(len(moved))
}

func main() {
	ctx := context.Background()
	c := &Cluster{pools: map[string]*pgxpool.Pool{}}
	for i, port := range []int{5514, 5515, 5516, 5517} {
		name := fmt.Sprintf("shard%d", i+1)
		c.pools[name] = open(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/app?sslmode=disable", port))
		defer c.pools[name].Close()
		must(c.pools[name].Exec(ctx, "TRUNCATE orders"))
	}
	fmt.Println("✅ Connected to shard1..shard4 (5514-5517)")

	now := time.Now().UTC()
	y := now.Year()
	jan1 := func(year int) time.Time { return time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC) }
	// One range per year; the newest one is open-ended. shard4 starts unused.
	c.ranges = []Range{
		{From: jan1(y - 2), To: jan1(y - 1), Shards: []string{"shard1"}},
		{From: jan1(y - 1), To: jan1(y), Shards: []string{"shard2"}},
		{From: jan1(y), Shards: []string{"shard3"}},
	}

	banner("📏 RANGE-BASED SHARDING SIMULATION (by order date)")

	section("🗺️  RANGE MAP — each shard owns a contiguous slice of time")
	printRanges(c)

	section("📥 LOAD HISTORY — orders routed by created_at")
	var history []order
	for t := jan1(y - 2); t.Before(now); t = t.Add(4 * time.Hour) {
		history = append(history, order{rand.IntN(1000), float64(rand.IntN(20000)) / 100, t})
	}
	c.insert(ctx, history)
	fmt.Printf("   %d orders loaded (one every 4h since %d-01-01)\n", len(history), y-2)
	printSizes(ctx, c)

	section("🔍 RANGE QUERY — 'revenue in March last year' touches ONE shard")
	from := time.Date(y-1, 3, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	targets := c.shardsFor(from, to)
	var total float64
	for _, s := range targets {
		var sum float64
		must0(c.pools[s].QueryRow(ctx,
			"SELECT coalesce(sum(amount), 0)::float8 FROM orders WHERE created_at >= $1 AND created_at < $2",
			from, to).Scan(&sum))
		total += sum
	}
	fmt.Printf("   shards visited: %v (of %d) → revenue %.2f\n", targets, len(c.pools), total)
	fmt.Println("   with hash sharding the same query must scatter to EVERY shard")

	section("🔥 WRITE HOTSPOT — new orders all have created_at = now()")
	printBurst(c.insert(ctx, burst(now, 1500)))
	fmt.Println("   every new write hits the newest range's shard; the others sit idle")

	split := jan1(y).Add(now.Sub(jan1(y)) / 2).Truncate(24 * time.Hour)
	section(fmt.Sprintf("✂️  SPLIT — shard3's range grew; split it at %s → shard4", split.Format("2006-01-02")))
	before := sizes(ctx, c)
	moved := c.splitRange(ctx, split, "shard4")
	fmt.Printf("   moved %d rows from shard3 to shard4 (shard1/shard2 untouched)\n", moved)
	printRanges(c)
	printSizesDelta(ctx, c, before)

	section("🔥 HOTSPOT AGAIN — splitting a time range doesn't fix APPEND load")
	printBurst(c.insert(ctx, burst(now.Add(time.Minute), 1500)))
	fmt.Println("   the newest range simply moved to shard4 — it takes 100% of writes")

	section("🧩 MITIGATION — spread the CURRENT range across shards by customer")
	cut := now.Add(2 * time.Minute)
	last := len(c.ranges) - 1
	c.ranges[last].To = cut
	c.ranges = append(c.ranges, Range{From: cut, Shards: []string{"shard3", "shard4"}})
	fmt.Println("   new open-ended range from now on: (created_at, customer_id % 2) → shard3 | shard4")
	printRanges(c)
	printBurst(c.insert(ctx, burst(cut.Add(time.Minute), 1500)))
	fmt.Printf("   trade-off: a query on recent orders must now visit %v\n",
		c.shardsFor(cut, cut.Add(time.Hour)))

	section("📊 SUMMARY")
	fmt.Println("   + range queries hit few shards; data locality by key order")
	fmt.Println("   + rebalance by SPLITTING one range — only its rows move")
	fmt.Println("   − monotonically increasing keys (time, serial ids) create write hotspots")
	fmt.Println("   − ranges grow unevenly → ongoing split/merge management")
}

// burst simulates n customers placing orders at time t.
func burst(t time.Time, n int) []order {
	out := make([]order, n)
	for i := range out {
		out[i] = order{rand.IntN(1000), float64(rand.IntN(20000)) / 100, t.Add(time.Duration(i) * time.Millisecond)}
	}
	return out
}

func sizes(ctx context.Context, c *Cluster) map[string]int {
	out := map[string]int{}
	for name, p := range c.pools {
		var n int
		must0(p.QueryRow(ctx, "SELECT count(*) FROM orders").Scan(&n))
		out[name] = n
	}
	return out
}

func printSizes(ctx context.Context, c *Cluster) {
	s := sizes(ctx, c)
	for _, name := range []string{"shard1", "shard2", "shard3", "shard4"} {
		fmt.Printf("   %-7s %5d rows %s\n", name, s[name], strings.Repeat("█", s[name]/150))
	}
}

func printSizesDelta(ctx context.Context, c *Cluster, before map[string]int) {
	s := sizes(ctx, c)
	for _, name := range []string{"shard1", "shard2", "shard3", "shard4"} {
		fmt.Printf("   %-7s %5d → %5d rows %s\n", name, before[name], s[name], strings.Repeat("█", s[name]/150))
	}
}

func printBurst(counts map[string]int) {
	total := 0
	for _, n := range counts {
		total += n
	}
	for _, name := range []string{"shard1", "shard2", "shard3", "shard4"} {
		n := counts[name]
		fmt.Printf("   %-7s %4d writes (%3d%%) %s\n", name, n, n*100/total, strings.Repeat("█", n/60))
	}
}

func printRanges(c *Cluster) {
	for _, r := range c.ranges {
		fmt.Printf("   %s\n", r)
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

func must[T any](_ T, err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must0(err error) {
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

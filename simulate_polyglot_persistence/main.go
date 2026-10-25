package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Polyglot persistence: use the right database for each access pattern instead of
// forcing one engine to do everything.
//
//   PostgreSQL     system of record: transactions, constraints, the truth
//   Redis          sub-millisecond cache + real-time leaderboard (sorted set)
//   Elasticsearch  full-text search: typo tolerance, relevance, facets
//
// The hard part is keeping them in sync. The app writes ONLY to PostgreSQL, in the
// same transaction as an outbox event; a relay applies events to Redis and
// Elasticsearch idempotently. Derived stores can lag, but never diverge for good —
// and can be rebuilt from PostgreSQL at any time.

const (
	pgURL = "postgres://postgres:postgres@localhost:5535/shop?sslmode=disable"
	esURL = "http://localhost:9200"
)

type Product struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Category    string  `json:"category"`
	Price       float64 `json:"price"`
	Stock       int     `json:"stock"`
	Version     int64   `json:"version"`
}

type App struct {
	pg  *pgxpool.Pool
	rdb *redis.Client
	es  *ES
}

func main() {
	ctx := context.Background()
	pg, err := pgxpool.New(ctx, pgURL)
	must(err)
	defer pg.Close()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6380"})
	defer rdb.Close()
	app := &App{pg: pg, rdb: rdb, es: &ES{base: esURL}}
	app.reset(ctx)
	fmt.Println("✅ Connected: PostgreSQL (5535), Redis (6380), Elasticsearch (9200)")

	banner("🧩 POLYGLOT PERSISTENCE SIMULATION")

	relay := NewRelay(app)

	section("📥 WRITE ONCE — products go to PostgreSQL + outbox, the relay fans out")
	start := time.Now()
	app.seedProducts(ctx, 3000)
	relay.Start(ctx)
	app.waitDrained(ctx)
	fmt.Printf("   3000 products: PostgreSQL ✓, relay → Elasticsearch ✓ in %v\n", time.Since(start).Round(time.Millisecond))
	fmt.Printf("   counts — PostgreSQL: %d   Elasticsearch: %d\n", app.pgCount(ctx), app.es.Count())

	section("🔍 SEARCH — 'wireles headphnes' (two typos)")
	q := "wireles headphnes"
	var ilike, fts int
	must(pg.QueryRow(ctx, "SELECT count(*) FROM products WHERE name ILIKE '%' || $1 || '%'", q).Scan(&ilike))
	must(pg.QueryRow(ctx, `SELECT count(*) FROM products
		WHERE to_tsvector('english', name || ' ' || description) @@ plainto_tsquery('english', $1)`, q).Scan(&fts))
	fmt.Printf("   PostgreSQL ILIKE: %d hits   PostgreSQL full-text: %d hits (no typo tolerance)\n", ilike, fts)
	res := app.es.Search(q)
	fmt.Printf("   Elasticsearch (fuzzy, relevance-ranked): %d hits in %dms, top 3:\n", res.Total, res.Took)
	for _, p := range res.Top {
		fmt.Printf("     %-44s %-12s $%7.2f\n", p.Name, p.Category, p.Price)
	}
	fmt.Print("   facets by category: ")
	for _, f := range res.Facets {
		fmt.Printf("%s(%d) ", f.Key, f.Count)
	}
	fmt.Println()

	section("⚡ PRODUCT PAGE — Redis cache-aside in front of PostgreSQL")
	for i := range 3 {
		start := time.Now()
		p, src := app.GetProduct(ctx, 42)
		fmt.Printf("   read %d: %-10s from %-10s in %v\n", i+1, p.Name[:10], src, time.Since(start).Round(time.Microsecond))
	}

	section("🏆 LEADERBOARD — orders → outbox → Redis sorted set")
	app.placeRandomOrders(ctx, 2000)
	app.waitDrained(ctx)
	start = time.Now()
	top, err := rdb.ZRevRangeWithScores(ctx, "bestsellers", 0, 4).Result()
	must(err)
	redisTook := time.Since(start)
	start = time.Now()
	rows, err := pg.Query(ctx, "SELECT product_id, sum(qty) FROM orders GROUP BY product_id ORDER BY 2 DESC LIMIT 5")
	must(err)
	rows.Close()
	pgTook := time.Since(start)
	for i, z := range top {
		fmt.Printf("   #%d product %-5s %4.0f units\n", i+1, z.Member, z.Score)
	}
	fmt.Printf("   ZREVRANGE: %v  vs  PostgreSQL GROUP BY over all orders: %v\n",
		redisTook.Round(time.Microsecond), pgTook.Round(time.Microsecond))
	fmt.Println("   the sorted set is maintained incrementally (O(log n) per order); the")
	fmt.Println("   GROUP BY re-aggregates every order on each read and grows with history")

	section("⏳ EVENTUAL CONSISTENCY — update a product, read every store immediately")
	app.UpdateProduct(ctx, 42, "Aurora Wireless Headphones PRO", 249.00)
	pgName := app.pgName(ctx, 42)
	esName := app.es.Get(42).Name
	fmt.Printf("   right after COMMIT → PostgreSQL: %q\n", pgName)
	fmt.Printf("                        Elasticsearch: %q %s\n", esName, staleMark(esName != pgName))
	start = time.Now()
	app.waitDrained(ctx)
	p, src := app.GetProduct(ctx, 42)
	fmt.Printf("   after relay (%v) → Elasticsearch: %q, Redis cache invalidated → %q (from %s)\n",
		time.Since(start).Round(time.Millisecond), app.es.Get(42).Name, p.Name, src)

	section("🛑 RELAY OUTAGE — writes keep working, events queue up")
	relay.Stop()
	for i := range 50 {
		app.UpdateProduct(ctx, int64(100+i), fmt.Sprintf("Clearance Item %d", i), 9.99)
	}
	var backlog int
	must(pg.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE processed_at IS NULL").Scan(&backlog))
	fmt.Printf("   relay down: 50 updates committed to PostgreSQL, outbox backlog = %d\n", backlog)
	relay.Start(ctx)
	start = time.Now()
	app.waitDrained(ctx)
	fmt.Printf("   relay back: backlog drained in %v; ES shows %q\n",
		time.Since(start).Round(time.Millisecond), app.es.Get(100).Name)

	section("♻️  REBUILD — derived stores are disposable: reindex Elasticsearch from PostgreSQL")
	app.es.Reset()
	fmt.Printf("   index dropped → Elasticsearch has %d docs\n", app.es.Count())
	start = time.Now()
	app.reindex(ctx)
	fmt.Printf("   reindexed %d products from PostgreSQL in %v\n", app.es.Count(), time.Since(start).Round(time.Millisecond))
	relay.Stop()

	section("📊 SUMMARY")
	fmt.Println("   Each store does what it's best at; PostgreSQL stays the source of truth.")
	fmt.Println("   Outbox + idempotent relay = no dual writes, no lost events, bounded lag.")
	fmt.Println("   Cost: three systems to run, secure, monitor — and eventual consistency.")
}

// ---------------------------------------------------------------------------
// Writes: PostgreSQL + outbox in one transaction. The app never writes to Redis or
// Elasticsearch directly, so there's no "PG succeeded but ES failed" dual-write bug.

func (a *App) seedProducts(ctx context.Context, n int) {
	adjectives := []string{"Aurora", "Nimbus", "Vertex", "Pulse", "Echo", "Zenith", "Nova", "Orbit", "Quantum", "Summit"}
	items := map[string][]string{
		"audio":     {"Wireless Headphones", "Bluetooth Speaker", "Noise Cancelling Earbuds", "Studio Monitor Headphones"},
		"computing": {"Mechanical Keyboard", "Wireless Mouse", "USB-C Hub", "27-inch Monitor"},
		"home":      {"Smart Lamp", "Air Purifier", "Robot Vacuum", "Coffee Grinder"},
		"outdoor":   {"Trail Backpack", "Camping Lantern", "Insulated Bottle", "Wireless Bike Light"},
	}
	cats := []string{"audio", "computing", "home", "outdoor"}
	must(pgx.BeginFunc(ctx, a.pg, func(tx pgx.Tx) error {
		for i := range n {
			cat := cats[i%len(cats)]
			item := items[cat][(i/len(cats))%len(items[cat])]
			name := fmt.Sprintf("%s %s %d", adjectives[rand.IntN(len(adjectives))], item, 100+i)
			desc := fmt.Sprintf("The %s %s — great for everyday use. Model %d.", strings.ToLower(cat), strings.ToLower(item), i)
			p := Product{Name: name, Description: desc, Category: cat, Price: float64(1000+rand.IntN(30000)) / 100, Stock: rand.IntN(500), Version: 1}
			if err := tx.QueryRow(ctx, `INSERT INTO products (name, description, category, price, stock)
				VALUES ($1, $2, $3, $4, $5) RETURNING id`, p.Name, p.Description, p.Category, p.Price, p.Stock).Scan(&p.ID); err != nil {
				return err
			}
			if err := emit(ctx, tx, "product_upserted", p); err != nil {
				return err
			}
		}
		return nil
	}))
}

func (a *App) UpdateProduct(ctx context.Context, id int64, name string, price float64) {
	must(pgx.BeginFunc(ctx, a.pg, func(tx pgx.Tx) error {
		var p Product
		if err := tx.QueryRow(ctx, `
			UPDATE products SET name = $2, price = $3, version = version + 1 WHERE id = $1
			RETURNING id, name, description, category, price::float8, stock, version`, id, name, price).
			Scan(&p.ID, &p.Name, &p.Description, &p.Category, &p.Price, &p.Stock, &p.Version); err != nil {
			return err
		}
		return emit(ctx, tx, "product_upserted", p)
	}))
}

func (a *App) placeRandomOrders(ctx context.Context, n int) {
	must(pgx.BeginFunc(ctx, a.pg, func(tx pgx.Tx) error {
		for range n {
			// Skewed popularity: low ids sell much more.
			pid := int64(1 + int(rand.ExpFloat64()*40)%3000)
			qty := 1 + rand.IntN(3)
			if _, err := tx.Exec(ctx, "INSERT INTO orders (product_id, qty) VALUES ($1, $2)", pid, qty); err != nil {
				return err
			}
			if err := emit(ctx, tx, "order_placed", map[string]any{"product_id": pid, "qty": qty}); err != nil {
				return err
			}
		}
		return nil
	}))
}

func emit(ctx context.Context, tx pgx.Tx, event string, payload any) error {
	_, err := tx.Exec(ctx, "INSERT INTO outbox (event, payload) VALUES ($1, $2)", event, payload)
	return err
}

// ---------------------------------------------------------------------------
// Reads.

// GetProduct is cache-aside: Redis first, PostgreSQL on a miss, then populate.
func (a *App) GetProduct(ctx context.Context, id int64) (Product, string) {
	key := fmt.Sprintf("product:%d", id)
	if b, err := a.rdb.Get(ctx, key).Bytes(); err == nil {
		var p Product
		must(json.Unmarshal(b, &p))
		return p, "Redis"
	}
	var p Product
	must(a.pg.QueryRow(ctx, `SELECT id, name, description, category, price::float8, stock, version
		FROM products WHERE id = $1`, id).Scan(&p.ID, &p.Name, &p.Description, &p.Category, &p.Price, &p.Stock, &p.Version))
	b, _ := json.Marshal(p)
	a.rdb.Set(ctx, key, b, 10*time.Minute)
	return p, "PostgreSQL"
}

func (a *App) pgName(ctx context.Context, id int64) string {
	var n string
	must(a.pg.QueryRow(ctx, "SELECT name FROM products WHERE id = $1", id).Scan(&n))
	return n
}

func (a *App) pgCount(ctx context.Context) int {
	var n int
	must(a.pg.QueryRow(ctx, "SELECT count(*) FROM products").Scan(&n))
	return n
}

// waitDrained blocks until the relay has processed every outbox event, then
// refreshes the search index so the result is visible to searches.
func (a *App) waitDrained(ctx context.Context) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var pending int
		must(a.pg.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE processed_at IS NULL").Scan(&pending))
		if pending == 0 {
			a.es.Refresh()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Fatal("outbox not drained")
}

// reindex rebuilds the search index from the source of truth with bulk requests.
func (a *App) reindex(ctx context.Context) {
	rows, err := a.pg.Query(ctx, `SELECT id, name, description, category, price::float8, stock, version
		FROM products ORDER BY id`)
	must(err)
	products, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Product])
	must(err)
	for chunk := range slices.Chunk(products, 1000) {
		must(a.es.BulkIndex(chunk))
	}
	a.es.Refresh()
}

func (a *App) reset(ctx context.Context) {
	must2(a.pg.Exec(ctx, "TRUNCATE products, orders, outbox RESTART IDENTITY"))
	must(a.rdb.FlushDB(ctx).Err())
	a.es.Reset()
}

// ---------------------------------------------------------------------------
// Relay: polls the outbox and applies events to the derived stores.

type Relay struct {
	app    *App
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func NewRelay(a *App) *Relay { return &Relay{app: a} }

func (r *Relay) Start(ctx context.Context) {
	ctx, r.cancel = context.WithCancel(ctx)
	r.wg.Go(func() {
		for ctx.Err() == nil {
			n, err := r.processBatch(ctx)
			if err != nil && ctx.Err() == nil {
				log.Printf("relay: %v (will retry)", err)
			}
			if n == 0 {
				select {
				case <-ctx.Done():
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
	})
}

func (r *Relay) Stop() {
	r.cancel()
	r.wg.Wait()
}

// incrOnce is atomic in Redis: increment the leaderboard only if this outbox event
// id hasn't been applied before. Makes order events safe to deliver more than once.
var incrOnce = redis.NewScript(`
if redis.call('SET', KEYS[1], 1, 'NX', 'EX', 86400) then
  return redis.call('ZINCRBY', KEYS[2], ARGV[1], ARGV[2])
end
return false`)

// processBatch claims up to 500 events (SKIP LOCKED lets several relays run), applies
// them, and marks them processed in the same transaction. If applying fails the
// transaction rolls back and the events are retried: AT-LEAST-ONCE delivery, made
// safe by idempotent appliers (ES external versions, Redis dedupe keys).
func (r *Relay) processBatch(ctx context.Context) (int, error) {
	a := r.app
	n := 0
	err := pgx.BeginFunc(ctx, a.pg, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, event, payload FROM outbox
			WHERE processed_at IS NULL ORDER BY id LIMIT 500 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			return err
		}
		type ev struct {
			id      int64
			event   string
			payload []byte
		}
		events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ev, error) {
			var e ev
			return e, row.Scan(&e.id, &e.event, &e.payload)
		})
		if err != nil || len(events) == 0 {
			return err
		}

		var docs []Product
		var ids []int64
		pipe := a.rdb.Pipeline()
		for _, e := range events {
			ids = append(ids, e.id)
			switch e.event {
			case "product_upserted":
				var p Product
				if err := json.Unmarshal(e.payload, &p); err != nil {
					return err
				}
				docs = append(docs, p)
				pipe.Del(ctx, fmt.Sprintf("product:%d", p.ID)) // invalidate the cache
			case "order_placed":
				var o struct {
					ProductID int64 `json:"product_id"`
					Qty       int   `json:"qty"`
				}
				if err := json.Unmarshal(e.payload, &o); err != nil {
					return err
				}
				incrOnce.Eval(ctx, pipe, []string{fmt.Sprintf("outbox:done:%d", e.id), "bestsellers"},
					o.Qty, fmt.Sprint(o.ProductID))
			}
		}
		if len(docs) > 0 {
			if err := a.es.BulkIndex(docs); err != nil {
				return err
			}
		}
		if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
			return err
		}
		n = len(events)
		_, err = tx.Exec(ctx, "UPDATE outbox SET processed_at = now() WHERE id = ANY($1)", ids)
		return err
	})
	return n, err
}

// ---------------------------------------------------------------------------
// Minimal Elasticsearch client over its REST API (net/http + encoding/json).

type ES struct{ base string }

type facet struct {
	Key   string `json:"key"`
	Count int    `json:"doc_count"`
}

type searchResult struct {
	Total  int
	Took   int
	Top    []Product
	Facets []facet
}

func (e *ES) do(method, path string, body any) ([]byte, int) {
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		r = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, e.base+path, r)
	must(err)
	if strings.HasSuffix(path, "_bulk") {
		req.Header.Set("Content-Type", "application/x-ndjson")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("elasticsearch %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode
}

// Reset drops and recreates the index with an explicit mapping: analyzed text for
// search, keyword for exact filters/facets. The standard analyzer (no stemming) is
// used because fuzzy matching runs on indexed terms: a stemmer would turn the typo
// "wireles" into "wirel", too far from "wireless" to match.
func (e *ES) Reset() {
	e.do(http.MethodDelete, "/products", nil)
	out, code := e.do(http.MethodPut, "/products", map[string]any{
		"mappings": map[string]any{"properties": map[string]any{
			"name":        map[string]any{"type": "text", "analyzer": "standard"},
			"description": map[string]any{"type": "text", "analyzer": "standard"},
			"category":    map[string]any{"type": "keyword"},
			"price":       map[string]any{"type": "float"},
			"stock":       map[string]any{"type": "integer"},
		}},
	})
	if code >= 300 {
		log.Fatalf("create index: %s", out)
	}
}

// BulkIndex upserts documents with EXTERNAL versioning: Elasticsearch keeps a doc
// only if the incoming version is higher than the stored one. Replays and
// out-of-order deliveries become harmless (409 version conflicts are expected).
func (e *ES) BulkIndex(docs []Product) error {
	var buf bytes.Buffer
	for _, d := range docs {
		meta, _ := json.Marshal(map[string]any{"index": map[string]any{
			"_index": "products", "_id": d.ID, "version": d.Version, "version_type": "external"}})
		src, _ := json.Marshal(d)
		buf.Write(meta)
		buf.WriteByte('\n')
		buf.Write(src)
		buf.WriteByte('\n')
	}
	out, code := e.do(http.MethodPost, "/_bulk", buf.Bytes())
	if code >= 300 {
		return fmt.Errorf("bulk: %s", out)
	}
	var res struct {
		Items []map[string]struct {
			Status int `json:"status"`
			Error  any `json:"error"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return err
	}
	for _, item := range res.Items {
		for _, r := range item {
			if r.Status >= 300 && r.Status != http.StatusConflict {
				return fmt.Errorf("bulk item failed: %v", r.Error)
			}
		}
	}
	return nil
}

func (e *ES) Refresh() { e.do(http.MethodPost, "/products/_refresh", nil) }

func (e *ES) Count() int {
	out, _ := e.do(http.MethodGet, "/products/_count", nil)
	var r struct{ Count int }
	json.Unmarshal(out, &r)
	return r.Count
}

func (e *ES) Get(id int64) Product {
	out, _ := e.do(http.MethodGet, fmt.Sprintf("/products/_doc/%d", id), nil)
	var r struct {
		Source Product `json:"_source"`
	}
	json.Unmarshal(out, &r)
	return r.Source
}

// Search runs a fuzzy multi-field match (name weighted 3x) plus a category facet.
func (e *ES) Search(q string) searchResult {
	out, code := e.do(http.MethodPost, "/products/_search", map[string]any{
		"size": 3,
		"query": map[string]any{"multi_match": map[string]any{
			"query": q, "fields": []string{"name^3", "description"}, "fuzziness": "AUTO", "operator": "and",
		}},
		"aggs": map[string]any{"by_category": map[string]any{"terms": map[string]any{"field": "category"}}},
	})
	if code >= 300 {
		log.Fatalf("search: %s", out)
	}
	var r struct {
		Took int `json:"took"`
		Hits struct {
			Total struct{ Value int } `json:"total"`
			Hits  []struct {
				Source Product `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
		Aggs struct {
			ByCategory struct {
				Buckets []facet `json:"buckets"`
			} `json:"by_category"`
		} `json:"aggregations"`
	}
	must(json.Unmarshal(out, &r))
	res := searchResult{Total: r.Hits.Total.Value, Took: r.Took, Facets: r.Aggs.ByCategory.Buckets}
	for _, h := range r.Hits.Hits {
		res.Top = append(res.Top, h.Source)
	}
	return res
}

// ---------------------------------------------------------------------------

func staleMark(stale bool) string {
	if stale {
		return "← stale (relay hasn't run yet)"
	}
	return "(relay already caught up)"
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must2[T any](_ T, err error) { must(err) }

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

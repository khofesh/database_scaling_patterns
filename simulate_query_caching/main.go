package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// Product mirrors a row in the products table.
type Product struct {
	ID          int     `json:"id"`
	Name        string  `json:"name"`
	Category    string  `json:"category"`
	Price       float64 `json:"price"`
	Description string  `json:"description"`
}

// Cache wraps the database and Redis and implements the cache-aside pattern.
type Cache struct {
	db    *pgxpool.Pool
	rdb   *redis.Client
	ttl   time.Duration
	hits  int
	misses int
}

func cacheKey(id int) string { return fmt.Sprintf("product:%d", id) }

// GetProduct implements CACHE-ASIDE (lazy loading):
//  1. look in Redis
//  2. on a miss, read the database
//  3. populate Redis with a TTL
//  4. return the value
func (c *Cache) GetProduct(ctx context.Context, id int) (*Product, time.Duration, error) {
	start := time.Now()

	// 1. Try the cache.
	if raw, err := c.rdb.Get(ctx, cacheKey(id)).Result(); err == nil {
		var p Product
		if jsonErr := json.Unmarshal([]byte(raw), &p); jsonErr == nil {
			c.hits++
			return &p, time.Since(start), nil
		}
	} else if !errors.Is(err, redis.Nil) {
		// Cache is down: degrade gracefully by falling through to the DB.
		log.Printf("redis error (falling back to DB): %v", err)
	}

	// 2. Cache miss -> read the system of record (deliberately slow).
	c.misses++
	var p Product
	err := c.db.QueryRow(ctx,
		"SELECT id, name, category, price, description FROM get_product_slow($1)", id).
		Scan(&p.ID, &p.Name, &p.Category, &p.Price, &p.Description)
	if err != nil {
		return nil, time.Since(start), err
	}

	// 3. Populate the cache for next time.
	if data, jsonErr := json.Marshal(p); jsonErr == nil {
		c.rdb.Set(ctx, cacheKey(id), data, c.ttl)
	}
	return &p, time.Since(start), nil
}

// UpdatePriceWriteThrough implements WRITE-THROUGH: update the database and the
// cache in the same operation so the cache never serves a stale price.
func (c *Cache) UpdatePriceWriteThrough(ctx context.Context, id int, price float64) error {
	var p Product
	err := c.db.QueryRow(ctx,
		"UPDATE products SET price = $1 WHERE id = $2 RETURNING id, name, category, price, description",
		price, id).Scan(&p.ID, &p.Name, &p.Category, &p.Price, &p.Description)
	if err != nil {
		return err
	}
	if data, jsonErr := json.Marshal(p); jsonErr == nil {
		c.rdb.Set(ctx, cacheKey(id), data, c.ttl)
	}
	return nil
}

// UpdatePriceInvalidate implements WRITE + INVALIDATE: update the database and
// delete the cache entry. The next read repopulates it (cache-aside again).
func (c *Cache) UpdatePriceInvalidate(ctx context.Context, id int, price float64) error {
	if _, err := c.db.Exec(ctx, "UPDATE products SET price = $1 WHERE id = $2", price, id); err != nil {
		return err
	}
	return c.rdb.Del(ctx, cacheKey(id)).Err()
}

func main() {
	ctx := context.Background()

	db, err := pgxpool.New(ctx, "postgres://postgres:postgres@localhost:5441/app_db?sslmode=disable")
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer db.Close()

	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("connect redis: %v", err)
	}
	// Start from a clean cache so the demo is reproducible.
	rdb.FlushDB(ctx)

	cache := &Cache{db: db, rdb: rdb, ttl: 30 * time.Second}
	fmt.Println("✅ Connected to PostgreSQL and Redis")

	banner("🗃️  QUERY CACHING SIMULATION (cache-aside)")

	section("❄️  COLD READ (cache miss) vs 🔥 WARM READ (cache hit)")
	demonstrateCacheAside(ctx, cache)

	section("✍️  WRITE-THROUGH: keep the cache fresh on update")
	demonstrateWriteThrough(ctx, cache)

	section("🗑️  WRITE + INVALIDATE: drop the entry on update")
	demonstrateInvalidation(ctx, cache)

	section("⏳ TTL EXPIRY")
	demonstrateTTL(ctx, cache)

	section("📊 CACHE STATISTICS")
	fmt.Printf("   Hits: %d   Misses: %d   Hit ratio: %.0f%%\n",
		cache.hits, cache.misses, ratio(cache.hits, cache.misses))
}

func demonstrateCacheAside(ctx context.Context, c *Cache) {
	// First read: cache miss, pays the ~50ms DB penalty.
	p, dCold, _ := c.GetProduct(ctx, 1)
	fmt.Printf("   Cold read product #%d (%s): %v  [MISS -> DB]\n", p.ID, p.Name, dCold.Round(time.Microsecond))

	// Second read: cache hit, served from Redis in microseconds.
	_, dWarm, _ := c.GetProduct(ctx, 1)
	fmt.Printf("   Warm read product #%d:          %v  [HIT  -> Redis]\n", p.ID, dWarm.Round(time.Microsecond))

	if dWarm > 0 {
		fmt.Printf("   ⚡ Cache hit was ~%.0fx faster\n", float64(dCold)/float64(dWarm))
	}
}

func demonstrateWriteThrough(ctx context.Context, c *Cache) {
	// Warm the cache first.
	c.GetProduct(ctx, 2)

	newPrice := 24.99
	if err := c.UpdatePriceWriteThrough(ctx, 2, newPrice); err != nil {
		log.Printf("update failed: %v", err)
		return
	}
	fmt.Printf("   Updated product #2 price to $%.2f (DB + cache in one step)\n", newPrice)

	// The very next read is a HIT and already shows the new price.
	p, d, _ := c.GetProduct(ctx, 2)
	fmt.Printf("   Next read: $%.2f in %v (served from cache, already fresh)\n", p.Price, d.Round(time.Microsecond))
}

func demonstrateInvalidation(ctx context.Context, c *Cache) {
	c.GetProduct(ctx, 3) // warm

	newPrice := 199.00
	if err := c.UpdatePriceInvalidate(ctx, 3, newPrice); err != nil {
		log.Printf("update failed: %v", err)
		return
	}
	fmt.Printf("   Updated product #3 to $%.2f and deleted its cache entry\n", newPrice)

	// Next read is a forced MISS -> reloads fresh data, then re-caches it.
	p, d, _ := c.GetProduct(ctx, 3)
	fmt.Printf("   Next read: $%.2f in %v (MISS -> DB, cache repopulated)\n", p.Price, d.Round(time.Microsecond))
}

func demonstrateTTL(ctx context.Context, c *Cache) {
	// Use a short-lived entry to show expiry without waiting 30s.
	key := cacheKey(4)
	c.GetProduct(ctx, 4) // populate (with the default 30s TTL)
	c.rdb.Expire(ctx, key, 1*time.Second)

	ttl, _ := c.rdb.TTL(ctx, key).Result()
	fmt.Printf("   product #4 cached with TTL set to %v\n", ttl)

	time.Sleep(1200 * time.Millisecond)
	exists, _ := c.rdb.Exists(ctx, key).Result()
	fmt.Printf("   After expiry, key present in Redis: %v (next read will MISS)\n", exists == 1)
}

func ratio(hits, misses int) float64 {
	total := hits + misses
	if total == 0 {
		return 0
	}
	return float64(hits) / float64(total) * 100
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

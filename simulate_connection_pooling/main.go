package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// The backend is started with max_connections=20, so direct clients are
	// scarce. PgBouncer multiplexes everyone onto DEFAULT_POOL_SIZE (5).
	directURL    = "postgres://postgres:postgres@localhost:5440/app_db?sslmode=disable"
	pooledURL    = "postgres://postgres:postgres@localhost:6432/app_db?sslmode=disable"
	adminConsole = "postgres://postgres:postgres@localhost:6432/pgbouncer?sslmode=disable"
)

func main() {
	ctx := context.Background()

	// A small, separate pool used only to observe the server (pg_stat_activity).
	admin, err := newPool(ctx, directURL, 3, false)
	if err != nil {
		log.Fatalf("connect (admin): %v", err)
	}
	defer admin.Close()
	fmt.Println("✅ Connected to PostgreSQL (max_connections=20) and PgBouncer")

	banner("🏊 CONNECTION POOLING SIMULATION")

	section("💥 DIRECT CONNECTIONS EXHAUST THE SERVER")
	demonstrateExhaustion(ctx, admin)

	section("♻️  PGBOUNCER MULTIPLEXES MANY CLIENTS ONTO FEW BACKENDS")
	demonstrateMultiplexing(ctx, admin)

	section("📊 PGBOUNCER POOL STATS (SHOW POOLS)")
	showPgBouncerPools(ctx)
}

// newPool builds a pgxpool with a fixed max size. When `pooled` is true the
// connection targets PgBouncer in transaction mode, so we switch pgx to the
// simple query protocol — server-side prepared statements are unsafe when a
// server connection is shared between transactions.
func newPool(ctx context.Context, url string, maxConns int32, pooled bool) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	if pooled {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// demonstrateExhaustion opens a direct pool larger than the server allows and
// fires concurrent slow queries. Some acquisitions fail because PostgreSQL runs
// out of connection slots — the classic problem a pooler solves.
func demonstrateExhaustion(ctx context.Context, admin *pgxpool.Pool) {
	const concurrency = 30
	pool, err := newPool(ctx, directURL, concurrency, false)
	if err != nil {
		log.Printf("direct pool: %v", err)
		return
	}
	defer pool.Close()

	fmt.Printf("   Firing %d concurrent direct queries at a 20-connection server...\n", concurrency)
	maxBackends := sampleBackends(ctx, admin, 400*time.Millisecond)
	var ok, failed atomic.Int64
	runLoad(concurrency, func() {
		// pg_sleep holds the connection so many are needed at once.
		_, err := pool.Exec(ctx, "SELECT pg_sleep(0.3)")
		if err != nil {
			failed.Add(1)
		} else {
			ok.Add(1)
		}
	})

	fmt.Printf("   Succeeded: %d   Failed (too many clients): %d\n", ok.Load(), failed.Load())
	fmt.Printf("   Peak client backends seen on the server: %d\n", <-maxBackends)
}

// demonstrateMultiplexing runs the same concurrency through PgBouncer. Every
// query succeeds even though the server still only allows 20 connections,
// because PgBouncer queues clients onto its 5 server connections.
func demonstrateMultiplexing(ctx context.Context, admin *pgxpool.Pool) {
	const concurrency = 100
	pool, err := newPool(ctx, pooledURL, concurrency, true)
	if err != nil {
		log.Printf("pooled pool: %v", err)
		return
	}
	defer pool.Close()

	fmt.Printf("   Firing %d concurrent queries through PgBouncer (pool size 5)...\n", concurrency)
	maxBackends := sampleBackends(ctx, admin, 800*time.Millisecond)
	var ok, failed atomic.Int64
	runLoad(concurrency, func() {
		_, err := pool.Exec(ctx, "SELECT pg_sleep(0.05)")
		if err != nil {
			failed.Add(1)
		} else {
			ok.Add(1)
		}
	})

	fmt.Printf("   Succeeded: %d   Failed: %d\n", ok.Load(), failed.Load())
	fmt.Printf("   Peak client backends seen on the server: %d (≈ pool size, not %d)\n",
		<-maxBackends, concurrency)
}

// showPgBouncerPools queries PgBouncer's virtual admin database for live pool
// metrics: how many clients are active/waiting and how many server connections
// are in use.
func showPgBouncerPools(ctx context.Context) {
	// The admin console only speaks the simple protocol.
	cfg, err := pgx.ParseConfig(adminConsole)
	if err != nil {
		log.Printf("parse admin url: %v", err)
		return
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		log.Printf("connect admin console: %v", err)
		return
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, "SHOW POOLS")
	if err != nil {
		log.Printf("SHOW POOLS: %v", err)
		return
	}
	defer rows.Close()

	descs := rows.FieldDescriptions()
	colIdx := func(name string) int {
		for i, d := range descs {
			if d.Name == name {
				return i
			}
		}
		return -1
	}
	dbI, poolI, activeI, waitI := colIdx("database"), colIdx("pool_mode"), colIdx("sv_active"), colIdx("cl_waiting")

	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			continue
		}
		get := func(i int) any {
			if i < 0 || i >= len(vals) {
				return "?"
			}
			return vals[i]
		}
		fmt.Printf("   db=%v mode=%v server_active=%v client_waiting=%v\n",
			get(dbI), get(poolI), get(activeI), get(waitI))
	}
}

// --- helpers --------------------------------------------------------------

// runLoad runs fn in `n` goroutines and waits for all of them.
func runLoad(n int, fn func()) {
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			fn()
		})
	}
	wg.Wait()
}

// sampleBackends polls pg_stat_activity for `dur` and returns (via channel) the
// peak number of client backends seen on app_db. Running in the background lets
// us measure concurrency while the load test is in flight.
func sampleBackends(ctx context.Context, admin *pgxpool.Pool, dur time.Duration) <-chan int {
	out := make(chan int, 1)
	go func() {
		deadline := time.Now().Add(dur)
		max := 0
		for time.Now().Before(deadline) {
			var n int
			err := admin.QueryRow(ctx, `
				SELECT COUNT(*) FROM pg_stat_activity
				WHERE datname = 'app_db' AND backend_type = 'client backend'`).Scan(&n)
			if err == nil && n > max {
				max = n
			}
			time.Sleep(15 * time.Millisecond)
		}
		out <- max
	}()
	return out
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

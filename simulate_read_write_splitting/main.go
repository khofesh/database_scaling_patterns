package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Router is an application-level read/write splitter. It automatically sends
// writes to the primary and balances reads across replicas — the same job a
// proxy like ProxySQL or pgpool does, implemented here in ~50 lines so the
// routing logic is explicit.
type Router struct {
	primary  *pgxpool.Pool
	replicas []*pgxpool.Pool
	next     atomic.Uint64

	writes           atomic.Int64
	readsFromReplica atomic.Int64
	readsFromPrimary atomic.Int64
}

func NewRouter(ctx context.Context, primaryURL string, replicaURLs []string) (*Router, error) {
	primary, err := pgxpool.New(ctx, primaryURL)
	if err != nil {
		return nil, fmt.Errorf("primary: %w", err)
	}
	r := &Router{primary: primary}
	for i, url := range replicaURLs {
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("replica %d: %w", i+1, err)
		}
		r.replicas = append(r.replicas, pool)
	}
	return r, nil
}

func (r *Router) Close() {
	if r.primary != nil {
		r.primary.Close()
	}
	for _, p := range r.replicas {
		p.Close()
	}
}

func (r *Router) pickReplica() *pgxpool.Pool {
	if len(r.replicas) == 0 {
		return r.primary
	}
	i := r.next.Add(1)
	return r.replicas[i%uint64(len(r.replicas))]
}

// Write runs a mutation on the primary and returns the WAL LSN at commit time.
// Callers can later use that LSN to read their own write consistently.
func (r *Router) Write(ctx context.Context, sql string, args ...any) (string, error) {
	if _, err := r.primary.Exec(ctx, sql, args...); err != nil {
		return "", err
	}
	r.writes.Add(1)
	var lsn string
	err := r.primary.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn)
	return lsn, err
}

// Read runs a query on a replica (round-robin). Use for data that tolerates a
// little staleness.
func (r *Router) Read(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.readsFromReplica.Add(1)
	return r.pickReplica().Query(ctx, sql, args...)
}

// ConsistentRead enforces read-your-writes. Given the LSN from a prior Write, it
// uses a replica only if that replica has already replayed up to (or past) the
// LSN; otherwise it falls back to the primary so the caller never sees stale data.
func (r *Router) ConsistentRead(ctx context.Context, afterLSN, sql string, args ...any) (pgx.Rows, string, error) {
	for _, replica := range r.replicas {
		var caughtUp bool
		err := replica.QueryRow(ctx,
			"SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn", afterLSN).Scan(&caughtUp)
		if err == nil && caughtUp {
			r.readsFromReplica.Add(1)
			rows, qErr := replica.Query(ctx, sql, args...)
			return rows, "replica", qErr
		}
	}
	// No replica has caught up yet -> read from the primary to stay consistent.
	r.readsFromPrimary.Add(1)
	rows, qErr := r.primary.Query(ctx, sql, args...)
	return rows, "primary", qErr
}

func main() {
	ctx := context.Background()

	primaryURL := "postgres://postgres:postgres@localhost:5442/app_db?sslmode=disable"
	replicaURLs := []string{
		"postgres://postgres:postgres@localhost:5443/app_db?sslmode=disable",
		"postgres://postgres:postgres@localhost:5444/app_db?sslmode=disable",
	}

	r, err := NewRouter(ctx, primaryURL, replicaURLs)
	if err != nil {
		log.Fatalf("router init: %v", err)
	}
	defer r.Close()
	fmt.Println("✅ Router connected: 1 primary + 2 replicas")

	banner("↔️  READ-WRITE SPLITTING SIMULATION")

	section("🧭 AUTOMATIC ROUTING (writes→primary, reads→replicas)")
	demonstrateRouting(ctx, r)

	section("🪞 READ-YOUR-WRITES via LSN tracking")
	demonstrateConsistentRead(ctx, r)

	section("📊 ROUTING STATISTICS")
	fmt.Printf("   Writes → primary:        %d\n", r.writes.Load())
	fmt.Printf("   Reads  → replicas:       %d\n", r.readsFromReplica.Load())
	fmt.Printf("   Reads  → primary (RYW):  %d\n", r.readsFromPrimary.Load())
}

func demonstrateRouting(ctx context.Context, r *Router) {
	// A write is automatically routed to the primary.
	_, err := r.Write(ctx,
		"INSERT INTO customers (name, email, tier) VALUES ($1, $2, $3)",
		"Eve Adams", "eve@example.com", "premium")
	if err != nil {
		log.Printf("write failed: %v", err)
	} else {
		fmt.Println("   INSERT customer → routed to PRIMARY")
	}

	// Reads are automatically routed across replicas.
	for i := 0; i < 4; i++ {
		rows, err := r.Read(ctx, "SELECT COUNT(*) FROM customers")
		if err != nil {
			log.Printf("read failed: %v", err)
			continue
		}
		for rows.Next() {
			var count int
			rows.Scan(&count)
			fmt.Printf("   SELECT customers → routed to REPLICA (%d rows)\n", count)
		}
		rows.Close()
	}
}

func demonstrateConsistentRead(ctx context.Context, r *Router) {
	email := fmt.Sprintf("ryw-%d@example.com", time.Now().UnixNano())

	// Write and capture the commit LSN.
	lsn, err := r.Write(ctx,
		"INSERT INTO customers (name, email, tier) VALUES ($1, $2, $3)",
		"New Signup", email, "standard")
	if err != nil {
		log.Printf("write failed: %v", err)
		return
	}
	fmt.Printf("   Inserted new customer; commit LSN = %s\n", lsn)

	// Immediately read it back with read-your-writes guarantees. Because the
	// replicas may not have replayed this LSN yet, the router transparently falls
	// back to the primary.
	rows, source, err := r.ConsistentRead(ctx,
		lsn, "SELECT name, tier FROM customers WHERE email = $1", email)
	if err != nil {
		log.Printf("consistent read failed: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name, tier string
		rows.Scan(&name, &tier)
		fmt.Printf("   Read-your-writes hit from %s: %s (%s) ✅ never stale\n", source, name, tier)
	}

	// After giving replication a moment, the same consistent read can be served
	// by a replica that has now caught up.
	time.Sleep(300 * time.Millisecond)
	rows2, source2, err := r.ConsistentRead(ctx,
		lsn, "SELECT name FROM customers WHERE email = $1", email)
	if err == nil {
		for rows2.Next() {
		}
		rows2.Close()
		fmt.Printf("   After 300ms, same read served from: %s (replicas caught up)\n", source2)
	}
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

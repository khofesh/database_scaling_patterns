package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ReplicaSet holds one writable primary and N read-only replicas. All writes go
// to the primary; reads are spread across the replicas to scale read throughput.
type ReplicaSet struct {
	primary  *pgxpool.Pool
	replicas []*pgxpool.Pool
	next     uint64 // round-robin counter for replica selection
}

// NewReplicaSet connects to the primary and every replica.
func NewReplicaSet(ctx context.Context, primaryURL string, replicaURLs []string) (*ReplicaSet, error) {
	primary, err := pgxpool.New(ctx, primaryURL)
	if err != nil {
		return nil, fmt.Errorf("connecting to primary: %w", err)
	}

	rs := &ReplicaSet{primary: primary}
	for i, url := range replicaURLs {
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			rs.Close()
			return nil, fmt.Errorf("connecting to replica %d: %w", i+1, err)
		}
		rs.replicas = append(rs.replicas, pool)
	}
	return rs, nil
}

// Close releases every connection pool.
func (rs *ReplicaSet) Close() {
	if rs.primary != nil {
		rs.primary.Close()
	}
	for _, r := range rs.replicas {
		r.Close()
	}
}

// Writer returns the primary pool. Every INSERT/UPDATE/DELETE must use this.
func (rs *ReplicaSet) Writer() *pgxpool.Pool {
	return rs.primary
}

// Reader returns the next replica using round-robin so read load is balanced.
// If no replicas are configured it falls back to the primary.
func (rs *ReplicaSet) Reader() *pgxpool.Pool {
	pool, _ := rs.readerWithIndex()
	return pool
}

// readerWithIndex is Reader plus the chosen replica index (or -1 for the primary
// fallback), used by the demo to show which replica served each read.
func (rs *ReplicaSet) readerWithIndex() (*pgxpool.Pool, int) {
	if len(rs.replicas) == 0 {
		return rs.primary, -1
	}
	i := atomic.AddUint64(&rs.next, 1)
	idx := int(i % uint64(len(rs.replicas)))
	return rs.replicas[idx], idx
}

func main() {
	ctx := context.Background()

	primaryURL := "postgres://postgres:postgres@localhost:5436/app_db?sslmode=disable"
	replicaURLs := []string{
		"postgres://postgres:postgres@localhost:5437/app_db?sslmode=disable",
		"postgres://postgres:postgres@localhost:5438/app_db?sslmode=disable",
	}

	rs, err := NewReplicaSet(ctx, primaryURL, replicaURLs)
	if err != nil {
		log.Fatalf("failed to initialise replica set: %v", err)
	}
	defer rs.Close()
	fmt.Println("✅ Connected to 1 primary and 2 replicas")

	banner("🔁 READ REPLICAS SIMULATION")

	section("✍️  WRITES GO TO THE PRIMARY")
	demonstrateWrites(ctx, rs)

	section("📖 READS ARE LOAD-BALANCED ACROSS REPLICAS")
	demonstrateBalancedReads(ctx, rs)

	section("⏱️  REPLICATION LAG")
	demonstrateReplicationLag(ctx, rs)

	section("🪞 READ-YOUR-WRITES CONSISTENCY")
	demonstrateReadYourWrites(ctx, rs)

	section("📊 REPLICATION STATUS (pg_stat_replication)")
	showReplicationStatus(ctx, rs)
}

// demonstrateWrites inserts rows on the primary. Replicas reject writes, which we
// also show here.
func demonstrateWrites(ctx context.Context, rs *ReplicaSet) {
	tag, err := rs.Writer().Exec(ctx,
		"INSERT INTO products (name, price, stock) VALUES ($1, $2, $3)",
		"Laptop Stand", 49.99, 80)
	if err != nil {
		log.Printf("primary write failed: %v", err)
	} else {
		fmt.Printf("   Primary accepted write (%d row inserted)\n", tag.RowsAffected())
	}

	// Replicas are read-only; an attempted write must fail.
	_, err = rs.replicas[0].Exec(ctx,
		"INSERT INTO products (name, price, stock) VALUES ($1, $2, $3)",
		"Should Fail", 1.00, 1)
	if err != nil {
		fmt.Printf("   Replica correctly rejected write: %s\n", shortErr(err))
	} else {
		fmt.Println("   ⚠️  Replica unexpectedly accepted a write!")
	}
}

// demonstrateBalancedReads issues several reads and prints which replica served
// each, proving the round-robin distribution.
func demonstrateBalancedReads(ctx context.Context, rs *ReplicaSet) {
	for i := 0; i < 6; i++ {
		reader, idx := rs.readerWithIndex()
		var count int
		if err := reader.QueryRow(ctx, "SELECT COUNT(*) FROM products").Scan(&count); err != nil {
			log.Printf("read failed: %v", err)
			continue
		}
		fmt.Printf("   Read #%d → replica %d (%d products)\n", i+1, idx+1, count)
	}
}

// demonstrateReplicationLag writes a row to the primary and immediately reads
// from a replica. Asynchronous replication means the row may not be there yet.
func demonstrateReplicationLag(ctx context.Context, rs *ReplicaSet) {
	marker := fmt.Sprintf("lag-probe-%d", time.Now().UnixNano())

	start := time.Now()
	_, err := rs.Writer().Exec(ctx,
		"INSERT INTO products (name, price, stock) VALUES ($1, $2, $3)", marker, 9.99, 1)
	if err != nil {
		log.Printf("write failed: %v", err)
		return
	}
	fmt.Printf("   Wrote %q to primary\n", marker)

	// Read it back from a replica with no delay.
	replica := rs.replicas[0]
	var found int
	replica.QueryRow(ctx, "SELECT COUNT(*) FROM products WHERE name = $1", marker).Scan(&found)
	fmt.Printf("   Immediate replica read (after %v): row visible = %v\n",
		time.Since(start).Round(time.Microsecond), found > 0)

	// Poll the replica until the row appears, measuring the observed lag.
	for found == 0 {
		if time.Since(start) > 5*time.Second {
			fmt.Println("   ⚠️  Row still not replicated after 5s")
			return
		}
		time.Sleep(10 * time.Millisecond)
		replica.QueryRow(ctx, "SELECT COUNT(*) FROM products WHERE name = $1", marker).Scan(&found)
	}
	fmt.Printf("   Row became visible on the replica after ~%v (observed lag)\n",
		time.Since(start).Round(time.Millisecond))
}

// demonstrateReadYourWrites shows the standard mitigation for replication lag:
// when a user must see their own write immediately, read it back from the
// primary instead of a replica.
func demonstrateReadYourWrites(ctx context.Context, rs *ReplicaSet) {
	name := fmt.Sprintf("ryw-%d", time.Now().UnixNano())
	var id int
	err := rs.Writer().QueryRow(ctx,
		"INSERT INTO products (name, price, stock) VALUES ($1, $2, $3) RETURNING id",
		name, 12.34, 5).Scan(&id)
	if err != nil {
		log.Printf("write failed: %v", err)
		return
	}
	fmt.Printf("   Inserted product id=%d on primary\n", id)

	// Strategy: route the follow-up read to the PRIMARY so the user always sees
	// their just-committed data, sidestepping replica lag.
	var price float64
	err = rs.Writer().QueryRow(ctx, "SELECT price FROM products WHERE id = $1", id).Scan(&price)
	if err != nil {
		log.Printf("read-back failed: %v", err)
		return
	}
	fmt.Printf("   Read-your-writes via primary: price=$%.2f (always consistent)\n", price)
	fmt.Println("   ↳ Use this for \"edit then view\" flows; use replicas for everything else.")
}

// showReplicationStatus queries pg_stat_replication on the primary to report each
// connected replica and its byte-level lag.
func showReplicationStatus(ctx context.Context, rs *ReplicaSet) {
	rows, err := rs.Writer().Query(ctx, `
		SELECT client_addr::text, state, sync_state,
		       pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS replay_lag_bytes
		FROM pg_stat_replication
		ORDER BY client_addr`)
	if err != nil {
		log.Printf("could not read pg_stat_replication: %v", err)
		return
	}
	defer rows.Close()

	any := false
	for rows.Next() {
		var clientAddr, state, syncState string
		var lagBytes int64
		if err := rows.Scan(&clientAddr, &state, &syncState, &lagBytes); err != nil {
			log.Printf("scan failed: %v", err)
			continue
		}
		fmt.Printf("   Replica %s: state=%s sync=%s replay_lag=%d bytes\n",
			clientAddr, state, syncState, lagBytes)
		any = true
	}
	if !any {
		fmt.Println("   No replicas currently streaming.")
	}
}

// --- small output helpers -------------------------------------------------

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

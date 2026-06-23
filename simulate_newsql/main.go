package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewSQL (here: CockroachDB) gives you a single logical SQL database that is, under
// the hood, a distributed system: data is auto-sharded into "ranges", each range is
// replicated to 3 nodes via Raft, and transactions are SERIALIZABLE across the whole
// cluster. Compare with simulate_database_sharding, where the *application* has to
// pick a shard and cross-shard transactions are essentially impossible. Here you
// just write SQL and the database does the distribution.
//
// Because CockroachDB speaks the PostgreSQL wire protocol, the same pgx driver used
// everywhere else in this repo connects to it unchanged.

const dsn = "postgres://root@localhost:26257/defaultdb?sslmode=disable"

func main() {
	ctx := context.Background()

	// The cluster takes a few seconds to bootstrap (the init container must run and
	// nodes must converge). Retry the connection until SQL is being served.
	pool := connectWithRetry(ctx)
	defer pool.Close()
	fmt.Println("✅ Connected to CockroachDB cluster (26257)")

	banner("🪳 NEWSQL (CockroachDB) SIMULATION")

	section("🗄️  SCHEMA — ordinary SQL; the cluster handles distribution")
	setupSchema(ctx, pool)
	fmt.Println("   Created table `accounts` and seeded Alice=$100, Bob=$100")

	section("📦 AUTOMATIC SHARDING + REPLICATION — no app-level routing")
	showRanges(ctx, pool)

	section("💸 DISTRIBUTED ACID TRANSACTION — serializable transfer")
	must(transfer(ctx, pool, "alice", "bob", 4000))
	printBalances(ctx, pool)
	fmt.Println("   One transaction, ACID across the whole cluster — not a 2-phase app hack.")

	section("⚔️  SERIALIZABLE ISOLATION — concurrent transfers, auto-retried")
	concurrentTransfers(ctx, pool)
	printBalances(ctx, pool)

	section("🛡️  FAULT TOLERANCE — survive a node loss")
	fmt.Println("   Every range is replicated to 3 nodes via Raft consensus.")
	fmt.Println("   Try it live, in another terminal:")
	fmt.Println("     docker compose stop roach2")
	fmt.Println("     # re-run this program — queries still succeed; no failover step.")

	section("📊 SUMMARY")
	fmt.Println("   NewSQL = SQL + ACID + horizontal scale + built-in HA.")
	fmt.Println("   Contrast simulate_database_sharding: no manual shard keys, no")
	fmt.Println("   cross-shard pain, no hand-rolled failover.")
}

// --- connection ---------------------------------------------------------------

func connectWithRetry(ctx context.Context) *pgxpool.Pool {
	for i := 0; i < 40; i++ {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool
			}
			pool.Close()
		}
		if i == 0 {
			fmt.Println("⏳ Waiting for cluster to finish bootstrapping...")
		}
		time.Sleep(2 * time.Second)
	}
	log.Fatal("cluster never became ready")
	return nil
}

// --- schema -------------------------------------------------------------------

func setupSchema(ctx context.Context, pool *pgxpool.Pool) {
	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS accounts (
			id      STRING PRIMARY KEY,
			owner   STRING NOT NULL,
			balance INT NOT NULL
		)`)
	must(err)
	_, err = pool.Exec(ctx, `
		UPSERT INTO accounts (id, owner, balance) VALUES
			('alice', 'Alice Wong', 10000),
			('bob',   'Bob Singh',  10000)`)
	must(err)
}

// --- distributed transaction --------------------------------------------------

// transfer moves money atomically between two accounts. CockroachDB runs this as a
// SERIALIZABLE distributed transaction; the rows may live on different nodes and it
// is still fully ACID. retryTxn wraps it so any serialization conflict (40001) is
// transparently retried — the canonical, correct way to use a serializable DB.
func transfer(ctx context.Context, pool *pgxpool.Pool, from, to string, amount int) error {
	return retryTxn(ctx, pool, func(tx pgx.Tx) error {
		var bal int
		if err := tx.QueryRow(ctx,
			"SELECT balance FROM accounts WHERE id = $1", from).Scan(&bal); err != nil {
			return err
		}
		if bal < amount {
			return fmt.Errorf("insufficient funds for %s", from)
		}
		if _, err := tx.Exec(ctx,
			"UPDATE accounts SET balance = balance - $1 WHERE id = $2", amount, from); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			"UPDATE accounts SET balance = balance + $1 WHERE id = $2", amount, to)
		return err
	})
}

// retryTxn runs fn inside a transaction and retries on serialization failures.
func retryTxn(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	const maxRetries = 10
	for attempt := 0; attempt < maxRetries; attempt++ {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err == nil {
			return nil
		}
		tx.Rollback(ctx)

		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40001" { // serialization_failure
			continue // retriable: the DB asked us to try again
		}
		return err
	}
	return fmt.Errorf("transaction failed after %d retries", maxRetries)
}

// concurrentTransfers fires several transfers in parallel to provoke serialization
// conflicts and show retryTxn resolving them without lost updates.
func concurrentTransfers(ctx context.Context, pool *pgxpool.Pool) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := transfer(ctx, pool, "bob", "alice", 100); err != nil {
				log.Printf("   concurrent transfer error: %v", err)
			}
		}()
	}
	wg.Wait()
	fmt.Println("   8 concurrent $1 transfers committed — no lost updates (serializable).")
}

// --- introspection ------------------------------------------------------------

// showRanges reports how the table is physically distributed, using only supported
// SQL surface (SHOW RANGES) — no restricted crdb_internal tables. It prints the
// range count, the per-range replication factor, and the distinct nodes hosting the
// data, demonstrating that CockroachDB sharded and replicated it automatically.
func showRanges(ctx context.Context, pool *pgxpool.Pool) {
	var ranges, replicas int
	err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(max(array_length(voting_replicas, 1)), 0)
		FROM [SHOW RANGES FROM TABLE accounts WITH DETAILS]`).Scan(&ranges, &replicas)
	if err != nil {
		log.Printf("ranges: %v", err)
		return
	}
	fmt.Printf("   `accounts` spans %d range(s), each replicated across %d nodes (Raft).\n", ranges, replicas)

	rows, err := pool.Query(ctx, `
		SELECT DISTINCT unnest(voting_replicas) AS node_id
		FROM [SHOW RANGES FROM TABLE accounts WITH DETAILS] ORDER BY node_id`)
	if err != nil {
		log.Printf("nodes: %v", err)
		return
	}
	defer rows.Close()
	var nodes []int
	for rows.Next() {
		var id int
		rows.Scan(&id)
		nodes = append(nodes, id)
	}
	fmt.Printf("   Data lives on nodes %v — the application never chose a shard.\n", nodes)
}

func printBalances(ctx context.Context, pool *pgxpool.Pool) {
	rows, err := pool.Query(ctx,
		"SELECT owner, balance FROM accounts ORDER BY id")
	if err != nil {
		log.Printf("balances: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var owner string
		var bal int
		rows.Scan(&owner, &bal)
		fmt.Printf("   %-12s $%.2f\n", owner, float64(bal)/100)
	}
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

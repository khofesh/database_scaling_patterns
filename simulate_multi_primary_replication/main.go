package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Multi-primary replication: every node accepts writes and ships them to every other
// node. Here two PostgreSQL 18 nodes replicate to each other with native logical
// replication (each publishes, each subscribes to the peer). The hard part is not
// moving data — it's what to do when two nodes change the same row at the same time.
//
// This program shows:
//   - bidirectional replication without loops (subscription option origin = none)
//   - conflict AVOIDANCE: disjoint id sequences per node for INSERTs
//   - an UNRESOLVED conflict: concurrent UPDATEs → the nodes silently diverge
//   - conflict RESOLUTION: last-writer-wins via a replica-only trigger → convergence
//   - PostgreSQL 18 conflict statistics (pg_stat_subscription_stats)

type Node struct {
	name string // cluster_name, also used as the docker hostname
	db   *pgxpool.Pool
	peer string // the other node's hostname on the docker network
}

func main() {
	ctx := context.Background()

	a := connect(ctx, "node_a", "node_b", 5500)
	b := connect(ctx, "node_b", "node_a", 5501)
	defer a.db.Close()
	defer b.db.Close()
	fmt.Println("✅ Connected to node_a (5500) and node_b (5501) — both writable")

	banner("🔀 MULTI-PRIMARY REPLICATION SIMULATION")

	section("🔗 SETUP — each node subscribes to the other (origin = none)")
	// Tear down any previous run so the demo is repeatable, then start clean.
	for _, n := range []*Node{a, b} {
		n.exec(ctx, "DROP SUBSCRIPTION IF EXISTS sub_from_peer")
	}
	for _, n := range []*Node{a, b} {
		n.exec(ctx, "TRUNCATE orders, accounts")
		n.exec(ctx, "ALTER TABLE accounts DISABLE TRIGGER accounts_lww")
		n.exec(ctx, "SELECT pg_stat_reset_subscription_stats(NULL)")
	}
	for _, n := range []*Node{a, b} {
		n.subscribe(ctx)
	}
	waitFor(ctx, "subscriptions streaming", func() bool {
		return a.streaming(ctx) && b.streaming(ctx)
	})
	fmt.Println("   node_a ⇄ node_b streaming. origin = none stops a change that arrived")
	fmt.Println("   FROM the peer being sent BACK to it — no infinite ping-pong.")

	section("✍️  WRITE ANYWHERE — concurrent INSERTs on both nodes")
	var wg sync.WaitGroup
	for _, n := range []*Node{a, b} {
		wg.Go(func() {
			for i := range 3 {
				n.exec(ctx, "INSERT INTO orders (item) VALUES ($1)", fmt.Sprintf("%s-item-%d", n.name, i+1))
			}
		})
	}
	wg.Wait()
	waitFor(ctx, "orders replicated", func() bool {
		return a.count(ctx, "orders") == 6 && b.count(ctx, "orders") == 6
	})
	a.printOrders(ctx)
	fmt.Println("   Both nodes hold all 6 rows. ids never collide: node_a uses 1,11,21…")
	fmt.Println("   and node_b uses 2,12,22… — conflict AVOIDANCE by key design.")

	section("💥 CONFLICT — same row updated on both nodes during a partition")
	a.exec(ctx, "INSERT INTO accounts (id, owner, balance) VALUES (1, 'alice', 100)")
	waitFor(ctx, "account replicated", func() bool { return b.count(ctx, "accounts") == 1 })
	fmt.Println("   accounts[1].balance = 100 on both nodes")
	concurrentUpdates(ctx, a, b)
	balA, balB := a.balance(ctx), b.balance(ctx)
	fmt.Printf("   after reconnect → node_a: %s   node_b: %s\n", balA, balB)
	if balA != balB {
		fmt.Println("   ❌ DIVERGED. Each node applied the OTHER node's update last.")
		fmt.Println("   No error was raised — the default policy is 'apply remote change'.")
	}
	a.printConflictStats(ctx)
	b.printConflictStats(ctx)
	fmt.Println("   PostgreSQL DETECTED the conflict (see also 'conflict detected' in the")
	fmt.Println("   server log) but its built-in resolution is always 'apply remote'.")

	section("🛡️  RESOLUTION — last-writer-wins (LWW) with a replica trigger")
	// Re-sync the row, then enable the guard. ENABLE REPLICA TRIGGER means it fires
	// only inside the logical replication apply worker, never for local writes.
	a.exec(ctx, "UPDATE accounts SET balance = 100, updated_at = clock_timestamp(), updated_by = 'node_a' WHERE id = 1")
	waitFor(ctx, "re-sync", func() bool { return a.balance(ctx) == "100.00" && b.balance(ctx) == "100.00" })
	for _, n := range []*Node{a, b} {
		n.exec(ctx, "ALTER TABLE accounts ENABLE REPLICA TRIGGER accounts_lww")
	}
	fmt.Println("   balance re-synced to 100; accounts_lww enabled as REPLICA trigger")
	concurrentUpdates(ctx, a, b)
	balA, balB = a.balance(ctx), b.balance(ctx)
	fmt.Printf("   after reconnect → node_a: %s   node_b: %s\n", balA, balB)
	if balA == balB {
		fmt.Println("   ✅ CONVERGED on the newer write (node_b's). node_b rejected node_a's")
		fmt.Println("   older update; node_a accepted node_b's newer one.")
	}
	fmt.Println("   ⚠️  LWW still LOSES node_a's write — fine for 'set profile name',")
	fmt.Println("   wrong for 'add $50'. Commutative ops (counters/CRDTs) or routing each")
	fmt.Println("   key to one home node avoid that.")

	section("📊 SUMMARY")
	fmt.Println("   Writes accepted on every node; replication is asynchronous.")
	fmt.Println("   INSERT conflicts: avoid with per-node key ranges / UUIDs.")
	fmt.Println("   UPDATE conflicts: detected (PG18 stats) but resolved only by policy —")
	fmt.Println("   without one, nodes diverge silently.")
}

// concurrentUpdates simulates a network partition: both subscriptions are disabled
// (changes queue up in each node's replication slot), each node updates the same
// row, then the link is restored and the queued changes cross over.
func concurrentUpdates(ctx context.Context, a, b *Node) {
	for _, n := range []*Node{a, b} {
		n.exec(ctx, "ALTER SUBSCRIPTION sub_from_peer DISABLE")
	}
	waitFor(ctx, "apply workers stopped", func() bool { return !a.streaming(ctx) && !b.streaming(ctx) })
	fmt.Println("   🔌 partition: subscriptions disabled")

	upd := "UPDATE accounts SET balance = $1, updated_at = clock_timestamp(), updated_by = current_setting('cluster_name') WHERE id = 1"
	a.exec(ctx, upd, 150) // alice deposits on node_a…
	time.Sleep(50 * time.Millisecond)
	b.exec(ctx, upd, 80) // …a moment later, a withdrawal lands on node_b
	fmt.Println("   node_a: balance = 150   (t1)")
	fmt.Println("   node_b: balance =  80   (t2, slightly later)")

	for _, n := range []*Node{a, b} {
		n.exec(ctx, "ALTER SUBSCRIPTION sub_from_peer ENABLE")
	}
	fmt.Println("   🔌 link restored: subscriptions enabled, queued changes cross over")
	// Wait until each node's apply worker has caught up with the peer's WAL.
	waitFor(ctx, "catch-up", func() bool { return a.caughtUp(ctx, b) && b.caughtUp(ctx, a) })
}

func connect(ctx context.Context, name, peer string, port int) *Node {
	db, err := pgxpool.New(ctx, fmt.Sprintf("postgres://postgres:postgres@localhost:%d/app?sslmode=disable", port))
	if err != nil {
		log.Fatalf("connect %s: %v", name, err)
	}
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("ping %s: %v", name, err)
	}
	return &Node{name: name, db: db, peer: peer}
}

// subscribe creates this node's subscription to its peer.
//   - origin = none: only replicate changes that ORIGINATED on the peer, so a row
//     we sent to the peer doesn't come straight back (loop prevention, PG16+).
//   - copy_data = false: both nodes start empty; no initial table sync needed.
func (n *Node) subscribe(ctx context.Context) {
	n.exec(ctx, fmt.Sprintf(`CREATE SUBSCRIPTION sub_from_peer
		CONNECTION 'host=%s port=5432 dbname=app user=postgres password=postgres'
		PUBLICATION pub_app
		WITH (origin = none, copy_data = false, slot_name = 'from_%s_to_%s')`,
		n.peer, n.peer, n.name))
	fmt.Printf("   %s: CREATE SUBSCRIPTION sub_from_peer → %s\n", n.name, n.peer)
}

// streaming reports whether this node's apply worker is running.
func (n *Node) streaming(ctx context.Context) bool {
	var pid *int32
	err := n.db.QueryRow(ctx,
		"SELECT pid FROM pg_stat_subscription WHERE subname = 'sub_from_peer' AND relid IS NULL").Scan(&pid)
	return err == nil && pid != nil
}

// caughtUp reports whether this node has received and applied everything the peer
// has written so far (subscriber's latest end LSN >= publisher's current WAL LSN).
func (n *Node) caughtUp(ctx context.Context, peer *Node) bool {
	var peerLSN string
	if err := peer.db.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&peerLSN); err != nil {
		return false
	}
	var ok bool
	err := n.db.QueryRow(ctx, `
		SELECT coalesce(latest_end_lsn >= $1::pg_lsn, false)
		FROM pg_stat_subscription WHERE subname = 'sub_from_peer' AND relid IS NULL`, peerLSN).Scan(&ok)
	return err == nil && ok
}

func (n *Node) printOrders(ctx context.Context) {
	rows, err := n.db.Query(ctx, "SELECT id, item, written_on FROM orders ORDER BY id")
	if err != nil {
		log.Fatalf("orders: %v", err)
	}
	defer rows.Close()
	fmt.Printf("   orders as seen on %s:\n", n.name)
	for rows.Next() {
		var id int64
		var item, on string
		rows.Scan(&id, &item, &on)
		fmt.Printf("     id=%-3d %-16s written on %s\n", id, item, on)
	}
}

// printConflictStats reads the PostgreSQL 18 per-subscription conflict counters.
// update_origin_differs = "the row I'm updating was last changed by a different
// origin" — exactly the concurrent-update case. It requires track_commit_timestamp.
// Cumulative stats are flushed by the apply worker about once a second, so poll
// briefly instead of reading a stale zero.
func (n *Node) printConflictStats(ctx context.Context) {
	var originDiffers, insertExists int64
	for range 25 {
		err := n.db.QueryRow(ctx, `
			SELECT confl_update_origin_differs, confl_insert_exists
			FROM pg_stat_subscription_stats WHERE subname = 'sub_from_peer'`).Scan(&originDiffers, &insertExists)
		if err != nil {
			log.Printf("conflict stats on %s: %v", n.name, err)
			return
		}
		if originDiffers > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("   %s pg_stat_subscription_stats: update_origin_differs=%d insert_exists=%d\n",
		n.name, originDiffers, insertExists)
}

func (n *Node) balance(ctx context.Context) string {
	var bal string
	if err := n.db.QueryRow(ctx, "SELECT balance::text FROM accounts WHERE id = 1").Scan(&bal); err != nil {
		return "?"
	}
	return bal
}

func (n *Node) count(ctx context.Context, table string) int {
	var c int
	if err := n.db.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&c); err != nil {
		return -1
	}
	return c
}

func (n *Node) exec(ctx context.Context, sql string, args ...any) {
	if _, err := n.db.Exec(ctx, sql, args...); err != nil {
		log.Fatalf("%s: %s: %v", n.name, sql, err)
	}
}

// waitFor polls cond until it holds or 20s pass. Replication is asynchronous, so
// the demo must wait for convergence rather than assume it.
func waitFor(ctx context.Context, what string, cond func() bool) {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		select {
		case <-ctx.Done():
			log.Fatal(ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	log.Fatalf("timed out waiting for: %s", what)
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Synchronous vs asynchronous replication is a dial between durability/consistency
// and write latency/availability. In PostgreSQL two settings control it:
//
//   synchronous_standby_names  WHICH standbys a commit waits for ('' = none = async,
//                              'FIRST 1 (s1)' = priority, 'ANY 1 (s1, s2)' = quorum)
//   synchronous_commit         HOW FAR the commit waits: local, remote_write, on
//                              (= remote flush), remote_apply (visible on standby)
//
// This program flips those settings at runtime and shows latency, stale reads,
// blocked commits when a sync standby stalls, and how quorum commit tolerates that.

const (
	primaryURL = "postgres://postgres:postgres@localhost:5502/app_db?sslmode=disable"
	s1URL      = "postgres://postgres:postgres@localhost:5503/app_db?sslmode=disable"
	s2URL      = "postgres://postgres:postgres@localhost:5504/app_db?sslmode=disable"
)

type Cluster struct {
	primary *pgxpool.Pool
	s1, s2  *pgxpool.Pool
}

func main() {
	ctx := context.Background()

	c := &Cluster{primary: open(ctx, primaryURL), s1: open(ctx, s1URL), s2: open(ctx, s2URL)}
	defer c.primary.Close()
	defer c.s1.Close()
	defer c.s2.Close()
	// Always leave the cluster asynchronous and replaying, even if a step fails.
	defer c.reset(ctx)
	c.reset(ctx)
	fmt.Println("✅ Connected: primary (5502), standby s1 (5503), standby s2 (5504)")

	banner("⚖️  SYNCHRONOUS vs ASYNCHRONOUS REPLICATION")

	section("1️⃣  ASYNC — commit returns after the LOCAL flush")
	c.setStandbyNames(ctx, "")
	c.printReplication(ctx)
	c.benchmark(ctx, "async (synchronous_commit=on, no sync standby)", "on")

	section("2️⃣  ASYNC → STALE READS on a standby")
	c.pauseReplay(ctx, c.s1)
	id := c.insert(ctx, "on", "written while s1 is behind")
	fmt.Printf("   primary committed id=%d\n", id)
	fmt.Printf("   s1 sees id=%d? %v  ← stale read: commit didn't wait for s1\n", id, c.has(ctx, c.s1, id))
	c.resumeReplay(ctx, c.s1)

	section("3️⃣  SYNC (FIRST 1 (s1, s2)) — what each synchronous_commit level waits for")
	c.setStandbyNames(ctx, "FIRST 1 (s1, s2)")
	c.printReplication(ctx)
	for _, level := range []string{"local", "remote_write", "on", "remote_apply"} {
		c.benchmark(ctx, "sync, synchronous_commit="+level, level)
	}
	fmt.Println("   local        → don't wait for any standby (even though one is sync)")
	fmt.Println("   remote_write → standby received it (in OS cache)")
	fmt.Println("   on           → standby flushed it to disk (survives standby crash)")
	fmt.Println("   remote_apply → standby replayed it (visible to standby readers)")

	section("4️⃣  remote_apply → READ-YOUR-WRITES on the sync standby")
	id = c.insert(ctx, "remote_apply", "read-your-writes")
	fmt.Printf("   committed id=%d with remote_apply; s1 sees it immediately? %v\n", id, c.has(ctx, c.s1, id))

	section("5️⃣  THE PRICE — sync standby stalls ⇒ commits BLOCK")
	c.pauseReplay(ctx, c.s1)
	c.blockedCommit(ctx, "remote_apply")
	c.resumeReplay(ctx, c.s1)

	section("6️⃣  QUORUM — ANY 1 (s1, s2): one slow standby no longer blocks")
	c.setStandbyNames(ctx, "ANY 1 (s1, s2)")
	c.printReplication(ctx)
	c.pauseReplay(ctx, c.s1)
	start := time.Now()
	id = c.insert(ctx, "remote_apply", "quorum commit with s1 paused")
	fmt.Printf("   s1 paused, yet commit id=%d returned in %v (s2 acknowledged)\n", id, time.Since(start).Round(time.Millisecond))
	fmt.Printf("   on s2: %v   on s1: %v\n", c.has(ctx, c.s2, id), c.has(ctx, c.s1, id))
	c.pauseReplay(ctx, c.s2)
	fmt.Println("   now pausing s2 too → quorum can't be met:")
	c.blockedCommit(ctx, "remote_apply")
	c.resumeReplay(ctx, c.s1)
	c.resumeReplay(ctx, c.s2)

	section("📊 SUMMARY")
	fmt.Println("   async          fastest, never blocks; a primary crash can lose")
	fmt.Println("                  recent commits and standbys can serve stale reads")
	fmt.Println("   sync FIRST n   zero data loss for acknowledged commits; latency +1 RTT;")
	fmt.Println("                  a stalled sync standby stalls ALL writes")
	fmt.Println("   quorum ANY n   zero loss with tolerance for slow/failed standbys")
}

// setStandbyNames changes synchronous_standby_names without a restart and waits
// until pg_stat_replication reflects the new mode.
func (c *Cluster) setStandbyNames(ctx context.Context, value string) {
	exec(ctx, c.primary, fmt.Sprintf("ALTER SYSTEM SET synchronous_standby_names = '%s'", value))
	exec(ctx, c.primary, "SELECT pg_reload_conf()")
	want := "async"
	switch {
	case strings.HasPrefix(value, "FIRST"):
		want = "sync"
	case strings.HasPrefix(value, "ANY"):
		want = "quorum"
	}
	waitFor(ctx, "sync_state="+want, func() bool {
		var n int
		c.primary.QueryRow(ctx,
			"SELECT count(*) FROM pg_stat_replication WHERE sync_state = $1", want).Scan(&n)
		return n > 0
	})
	shown := value
	if shown == "" {
		shown = "'' (empty → asynchronous)"
	}
	fmt.Printf("   synchronous_standby_names = %s\n", shown)
}

// printReplication shows the primary's view of its standbys. sync_state is one of
// async / potential / sync / quorum.
func (c *Cluster) printReplication(ctx context.Context) {
	rows, err := c.primary.Query(ctx, `
		SELECT application_name, state, sync_state, sync_priority
		FROM pg_stat_replication ORDER BY application_name`)
	if err != nil {
		log.Fatalf("pg_stat_replication: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, state, syncState string
		var prio int
		rows.Scan(&name, &state, &syncState, &prio)
		fmt.Printf("   standby %-3s %-10s sync_state=%-9s priority=%d\n", name, state, syncState, prio)
	}
}

// benchmark runs n single-row autocommit inserts at the given synchronous_commit
// level on one pinned connection and reports the average commit latency.
func (c *Cluster) benchmark(ctx context.Context, label, level string) {
	const n = 200
	conn, err := c.primary.Acquire(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET synchronous_commit = "+level); err != nil {
		log.Fatal(err)
	}
	start := time.Now()
	for range n {
		if _, err := conn.Exec(ctx, "INSERT INTO ledger (note) VALUES ('bench')"); err != nil {
			log.Fatalf("bench insert: %v", err)
		}
	}
	avg := time.Since(start) / n
	fmt.Printf("   %-48s avg commit %6.3f ms\n", label, float64(avg.Microseconds())/1000)
}

// insert writes one row at the given synchronous_commit level, in its own
// transaction (SET LOCAL scopes the level to just this commit).
func (c *Cluster) insert(ctx context.Context, level, note string) int64 {
	var id int64
	err := pgx.BeginFunc(ctx, c.primary, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL synchronous_commit = "+level); err != nil {
			return err
		}
		return tx.QueryRow(ctx, "INSERT INTO ledger (note) VALUES ($1) RETURNING id", note).Scan(&id)
	})
	if err != nil {
		log.Fatalf("insert: %v", err)
	}
	return id
}

// blockedCommit shows what happens when the commit's sync requirement can't be met:
// the COMMIT waits indefinitely (statement_timeout does NOT cover the commit wait).
// After 2s we cancel the backend, as an impatient client or operator would. That
// cancels the WAIT, not the commit — PostgreSQL warns the transaction has ALREADY
// committed locally.
func (c *Cluster) blockedCommit(ctx context.Context, level string) {
	cfg, err := pgx.ParseConfig(primaryURL)
	if err != nil {
		log.Fatal(err)
	}
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		notices = append(notices, n.Message+" "+n.Detail)
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)
	exec(ctx, conn, "SET synchronous_commit = "+level)

	pid := conn.PgConn().PID()
	go func() {
		time.Sleep(2 * time.Second)
		c.primary.Exec(ctx, "SELECT pg_cancel_backend($1)", pid)
	}()

	start := time.Now()
	var id int64
	err = conn.QueryRow(ctx, "INSERT INTO ledger (note) VALUES ('blocked commit') RETURNING id").Scan(&id)
	fmt.Printf("   INSERT … COMMIT hung %v waiting for the sync standby until cancelled\n",
		time.Since(start).Round(100*time.Millisecond))
	if err != nil {
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
			fmt.Printf("   error: %s\n", pgErr.Message)
		} else {
			fmt.Printf("   error: %v\n", err)
		}
	}
	for _, n := range notices {
		fmt.Printf("   ⚠️  server: %s\n", n)
	}
	if id > 0 {
		fmt.Printf("   …and id=%d IS visible on the primary: the client gave up waiting,\n", id)
		fmt.Println("   but the row exists. Sync replication can't 'un-commit'.")
	}
}

func (c *Cluster) has(ctx context.Context, db *pgxpool.Pool, id int64) bool {
	var ok bool
	db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM ledger WHERE id = $1)", id).Scan(&ok)
	return ok
}

// pauseReplay stops WAL *replay* on a standby. It keeps receiving and flushing WAL,
// so 'remote_write' and 'on' still succeed, but 'remote_apply' can't — a handy,
// deterministic stand-in for a slow or overloaded standby.
func (c *Cluster) pauseReplay(ctx context.Context, db *pgxpool.Pool) {
	exec(ctx, db, "SELECT pg_wal_replay_pause()")
	fmt.Printf("   ⏸️  WAL replay paused on %s\n", name(ctx, db))
}

func (c *Cluster) resumeReplay(ctx context.Context, db *pgxpool.Pool) {
	exec(ctx, db, "SELECT pg_wal_replay_resume()")
	fmt.Printf("   ▶️  WAL replay resumed on %s\n", name(ctx, db))
}

func (c *Cluster) reset(ctx context.Context) {
	c.primary.Exec(ctx, "ALTER SYSTEM RESET synchronous_standby_names")
	c.primary.Exec(ctx, "SELECT pg_reload_conf()")
	c.s1.Exec(ctx, "SELECT pg_wal_replay_resume()")
	c.s2.Exec(ctx, "SELECT pg_wal_replay_resume()")
}

func name(ctx context.Context, db *pgxpool.Pool) string {
	var n string
	db.QueryRow(ctx, "SELECT current_setting('cluster_name')").Scan(&n)
	return n
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func exec(ctx context.Context, db execer, sql string) {
	if _, err := db.Exec(ctx, sql); err != nil {
		log.Fatalf("%s: %v", sql, err)
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

func waitFor(ctx context.Context, what string, cond func() bool) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
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

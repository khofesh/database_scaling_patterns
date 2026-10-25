package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A standby is a copy of the primary kept up to date by replaying its WAL. How it
// receives WAL and whether it can be queried defines its flavour:
//
//   warm standby  WAL shipped as ARCHIVED 16 MB segment files; no queries allowed;
//                 only useful after promotion; loses whatever wasn't archived yet
//   hot standby   WAL STREAMED continuously; serves read-only queries; near-zero lag
//
// And because the primary archives every WAL segment, a base backup + the archive
// can rebuild the database at ANY point in time (PITR) — the one thing standbys
// can't do, since they faithfully replicate mistakes too.

const (
	primaryURL = "postgres://postgres:postgres@localhost:5525/bank?sslmode=disable"
	hotURL     = "postgres://postgres:postgres@localhost:5526/bank?sslmode=disable"
	warmURL    = "postgres://postgres:postgres@localhost:5527/bank?sslmode=disable"
	pitrURL    = "postgres://postgres:postgres@localhost:5528/bank?sslmode=disable"
)

func main() {
	ctx := context.Background()
	banner("🔥 HOT STANDBY vs WARM STANDBY + POINT-IN-TIME RECOVERY")

	section("🧹 PREPARE — fresh warm standby, empty table")
	// The warm standby and PITR instances are promoted by this demo, so they're
	// recreated from scratch every run (-V = fresh anonymous data volume).
	compose("rm", "-sf", "pitr")
	compose("up", "-d", "-V", "--force-recreate", "standby_warm")
	primary := open(ctx, primaryURL)
	defer primary.Close()
	hot := open(ctx, hotURL)
	defer hot.Close()
	exec1(ctx, primary, "TRUNCATE accounts RESTART IDENTITY")
	waitWarmInRecovery()
	fmt.Println("   primary (5525), hot standby (5526), warm standby (5527) running")

	section("🔌 CONNECT TO EACH STANDBY")
	var inRecovery bool
	must(hot.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery))
	fmt.Printf("   hot  standby: ✅ accepts read-only queries (pg_is_in_recovery = %v)\n", inRecovery)
	if _, err := pgx.Connect(ctx, warmURL); err != nil {
		fmt.Printf("   warm standby: ❌ %s\n", firstLine(err))
	}

	section("📡 STREAMING — the hot standby sees commits almost immediately")
	insertBatch(ctx, primary, "batch-1", 100)
	start := time.Now()
	waitCount(ctx, hot, 100)
	fmt.Printf("   100 rows committed on primary → visible on hot standby after %v\n", time.Since(start).Round(time.Millisecond))
	printReplication(ctx, primary)

	section("📦 LOG SHIPPING — the warm standby only gets COMPLETED WAL segments")
	exec1(ctx, primary, "SELECT pg_switch_wal()") // close the current segment → archive it
	waitArchived(ctx, primary)
	insertBatch(ctx, primary, "batch-2", 50) // still in the current, un-archived segment
	waitCount(ctx, hot, 150)
	fmt.Println("   batch-1 (100 rows) → segment switched + archived")
	fmt.Println("   batch-2 ( 50 rows) → still in the current segment, NOT archived")
	printArchiver(ctx, primary)

	section("🚨 PRIMARY LOST — promote the warm standby")
	compose("exec", "-u", "postgres", "standby_warm", "pg_ctl", "promote", "-w", "-D", "/var/lib/postgresql/data")
	warm := waitPromoted(ctx, warmURL)
	defer warm.Close()
	fmt.Printf("   hot  standby has %d rows\n", count(ctx, hot))
	fmt.Printf("   warm standby (now primary) has %d rows ← batch-2 lost: RPO = un-archived WAL\n", count(ctx, warm))
	fmt.Println("   (archive_timeout bounds that window; streaming avoids it)")

	section("💾 BASE BACKUP — physical copy for point-in-time recovery")
	start = time.Now()
	compose("exec", "-u", "postgres", "pg_primary", "sh", "-c",
		"rm -rf /backups/base && pg_basebackup -D /backups/base -Fp -Xs -c fast")
	fmt.Printf("   pg_basebackup → /backups/base in %v\n", time.Since(start).Round(time.Millisecond))
	insertBatch(ctx, primary, "after-backup", 25)
	fmt.Println("   25 more rows written after the backup (they live only in WAL)")

	section("💥 DISASTER — someone runs DELETE without a WHERE clause")
	var lsn string
	must(primary.QueryRow(ctx, "SELECT pg_create_restore_point('before_disaster')::text").Scan(&lsn))
	fmt.Printf("   restore point 'before_disaster' created at LSN %s\n", lsn)
	exec1(ctx, primary, "DELETE FROM accounts")
	exec1(ctx, primary, "SELECT pg_switch_wal()")
	waitArchived(ctx, primary)
	waitCount(ctx, hot, 0)
	fmt.Printf("   primary: %d rows   hot standby: %d rows\n", count(ctx, primary), count(ctx, hot))
	fmt.Println("   ❌ the hot standby replicated the DELETE — replicas are NOT backups")

	section("⏪ POINT-IN-TIME RECOVERY — base backup + archived WAL up to the restore point")
	start = time.Now()
	compose("--profile", "pitr", "up", "-d", "-V", "--force-recreate", "pitr")
	pitr := waitPromoted(ctx, pitrURL)
	defer pitr.Close()
	fmt.Printf("   recovered instance up in %v with %d rows ✅\n", time.Since(start).Round(time.Millisecond), count(ctx, pitr))
	printBatches(ctx, pitr)
	fmt.Println("   everything up to 'before_disaster' — including the 25 rows written after")
	fmt.Println("   the base backup, replayed from the WAL archive")

	section("📊 SUMMARY")
	fmt.Println("   warm standby  cheap, simple; no reads; loses un-archived WAL on failover")
	fmt.Println("   hot standby   reads + near-zero lag; replicates mistakes instantly")
	fmt.Println("   PITR          base backup + WAL archive: rewind to any moment")
}

func insertBatch(ctx context.Context, db *pgxpool.Pool, batch string, n int) {
	if _, err := db.Exec(ctx, `
		INSERT INTO accounts (owner, balance, batch)
		SELECT 'owner-' || g, 100 + g, $1 FROM generate_series(1, $2) g`, batch, n); err != nil {
		log.Fatal(err)
	}
}

func printReplication(ctx context.Context, primary *pgxpool.Pool) {
	rows, err := primary.Query(ctx, `
		SELECT client_addr::text, state, coalesce(replay_lag::text, '0')
		FROM pg_stat_replication`)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var addr, state, lag string
		rows.Scan(&addr, &state, &lag)
		fmt.Printf("   pg_stat_replication: %s %s replay_lag=%s (only the HOT standby streams)\n", addr, state, lag)
	}
}

func printArchiver(ctx context.Context, primary *pgxpool.Pool) {
	var archived int
	var last string
	must(primary.QueryRow(ctx,
		"SELECT archived_count, coalesce(last_archived_wal, '-') FROM pg_stat_archiver").Scan(&archived, &last))
	fmt.Printf("   pg_stat_archiver: %d segments archived, last = %s\n", archived, last)
}

func printBatches(ctx context.Context, db *pgxpool.Pool) {
	rows, err := db.Query(ctx, "SELECT batch, count(*) FROM accounts GROUP BY batch ORDER BY min(id)")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var batch string
		var n int
		rows.Scan(&batch, &n)
		fmt.Printf("     %-13s %3d rows\n", batch, n)
	}
}

// waitArchived waits until the archiver has caught up with every completed segment.
func waitArchived(ctx context.Context, primary *pgxpool.Pool) {
	waitFor("WAL archived", func() bool {
		var ok bool
		err := primary.QueryRow(ctx, `
			SELECT last_archived_wal IS NOT NULL
			   AND last_archived_wal >= pg_walfile_name(pg_current_wal_lsn() - 1)
			FROM pg_stat_archiver`).Scan(&ok)
		return err == nil && ok
	})
}

// waitWarmInRecovery waits until the warm standby is up and REJECTING connections,
// which is exactly how a healthy warm standby looks from the outside.
func waitWarmInRecovery() {
	waitFor("warm standby in recovery", func() bool {
		out, _ := exec.Command("docker", "exec", "hotstandby_warm", "pg_isready", "-U", "postgres").CombinedOutput()
		return strings.Contains(string(out), "rejecting connections")
	})
}

// waitPromoted waits until a promoted/recovered instance accepts connections and has
// left recovery (it is now an independent primary).
func waitPromoted(ctx context.Context, url string) *pgxpool.Pool {
	var db *pgxpool.Pool
	waitFor("instance promoted", func() bool {
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			return false
		}
		defer conn.Close(ctx)
		var inRecovery bool
		if conn.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery) != nil || inRecovery {
			return false
		}
		db = open(ctx, url)
		return true
	})
	return db
}

func waitCount(ctx context.Context, db *pgxpool.Pool, want int) {
	waitFor(fmt.Sprintf("%d rows", want), func() bool { return count(ctx, db) == want })
}

func count(ctx context.Context, db *pgxpool.Pool) int {
	var n int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM accounts").Scan(&n); err != nil {
		return -1
	}
	return n
}

func waitFor(what string, cond func() bool) {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	log.Fatalf("timed out waiting for: %s", what)
}

// compose runs a docker compose subcommand for this project (from its directory).
func compose(args ...string) {
	out, err := exec.Command("docker", append([]string{"compose"}, args...)...).CombinedOutput()
	if err != nil {
		log.Fatalf("docker compose %v: %v\n%s", args, err, out)
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

func exec1(ctx context.Context, db *pgxpool.Pool, sql string) {
	if _, err := db.Exec(ctx, sql); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

func firstLine(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, "FATAL: "); i >= 0 {
		s = s[i:]
	}
	return strings.SplitN(s, "\n", 2)[0]
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

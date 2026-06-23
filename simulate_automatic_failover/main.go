package main

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FailoverManager is a minimal version of what Patroni/repmgr do: it watches the
// primary, and when the primary stops answering it promotes the standby to become
// the new primary and redirects writes there. Real tools add distributed leader
// election and fencing; this keeps the core control loop explicit.
type FailoverManager struct {
	primaryURL string
	standbyURL string

	current     *pgxpool.Pool // pool currently treated as the writable primary
	standby     *pgxpool.Pool // the standby pool (becomes current after promotion)
	promoted    bool
	failThresh  int           // consecutive failures before we fail over
	probeEvery  time.Duration // how often we health-check
	probeTimout time.Duration // per-probe timeout
}

func NewFailoverManager(ctx context.Context, primaryURL, standbyURL string) (*FailoverManager, error) {
	primary, err := pgxpool.New(ctx, primaryURL)
	if err != nil {
		return nil, err
	}
	standby, err := pgxpool.New(ctx, standbyURL)
	if err != nil {
		primary.Close()
		return nil, err
	}
	return &FailoverManager{
		primaryURL:  primaryURL,
		standbyURL:  standbyURL,
		current:     primary,
		standby:     standby,
		failThresh:  3,
		probeEvery:  1 * time.Second,
		probeTimout: 2 * time.Second,
	}, nil
}

func (m *FailoverManager) Close() {
	m.current.Close()
	if !m.promoted { // after promotion, current == standby; don't double-close
		m.standby.Close()
	}
}

// healthy returns true if the current primary answers a trivial query in time.
func (m *FailoverManager) healthy(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, m.probeTimout)
	defer cancel()
	var one int
	return m.current.QueryRow(cctx, "SELECT 1").Scan(&one) == nil
}

// WatchAndFailover runs the control loop until either the primary fails (and the
// standby is promoted) or maxProbes elapse. Returns true if a failover happened.
func (m *FailoverManager) WatchAndFailover(ctx context.Context, maxProbes int) bool {
	failures := 0
	for i := 0; i < maxProbes; i++ {
		if m.healthy(ctx) {
			failures = 0
			fmt.Printf("   probe %2d: primary OK ✅\n", i+1)
		} else {
			failures++
			fmt.Printf("   probe %2d: primary UNREACHABLE ❌ (%d/%d)\n", i+1, failures, m.failThresh)
			if failures >= m.failThresh {
				fmt.Printf("   → failure threshold reached; promoting standby\n")
				if err := m.promote(ctx); err != nil {
					log.Printf("promotion failed: %v", err)
					return false
				}
				return true
			}
		}
		time.Sleep(m.probeEvery)
	}
	return false
}

// promote turns the standby into a primary. pg_promote() tells PostgreSQL to exit
// recovery and start accepting writes; we then wait until it confirms it is no
// longer in recovery before routing writes to it.
func (m *FailoverManager) promote(ctx context.Context) error {
	if _, err := m.standby.Exec(ctx, "SELECT pg_promote(wait := true)"); err != nil {
		return fmt.Errorf("pg_promote: %w", err)
	}
	// Wait for the node to leave recovery (becomes writable).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var inRecovery bool
		if err := m.standby.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery); err == nil && !inRecovery {
			// Swap roles: the standby is now the writable primary.
			m.current.Close()
			m.current = m.standby
			m.promoted = true
			fmt.Println("   ✅ standby promoted; it now accepts writes")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("standby did not leave recovery in time")
}

func (m *FailoverManager) Write(ctx context.Context, sql string, args ...any) error {
	_, err := m.current.Exec(ctx, sql, args...)
	return err
}

func (m *FailoverManager) CountCustomers(ctx context.Context) (int, error) {
	var n int
	err := m.current.QueryRow(ctx, "SELECT COUNT(*) FROM customers").Scan(&n)
	return n, err
}

// simulatePrimaryCrash stops the primary container so the manager observes a real
// outage. If docker isn't reachable, the demo prints how to do it manually.
func simulatePrimaryCrash() {
	fmt.Println("   💥 stopping primary container (docker stop failover_primary)...")
	out, err := exec.Command("docker", "stop", "failover_primary").CombinedOutput()
	if err != nil {
		fmt.Printf("   (couldn't stop container automatically: %v)\n", err)
		fmt.Println("   Run manually in another terminal: docker stop failover_primary")
		return
	}
	fmt.Printf("   stopped: %s", out)
}

func main() {
	ctx := context.Background()

	primaryURL := "postgres://postgres:postgres@localhost:5470/app_db?sslmode=disable"
	standbyURL := "postgres://postgres:postgres@localhost:5471/app_db?sslmode=disable"

	m, err := NewFailoverManager(ctx, primaryURL, standbyURL)
	if err != nil {
		log.Fatalf("manager init: %v", err)
	}
	defer m.Close()
	fmt.Println("✅ Failover manager up: primary (5470) + standby (5471)")

	banner("🔁 AUTOMATIC FAILOVER SIMULATION")

	section("✍️  WRITE to primary, confirm it replicates")
	if err := m.Write(ctx,
		"INSERT INTO customers (name, email, tier) VALUES ($1, $2, $3)",
		"Pre-Failover User", "pre@example.com", "standard"); err != nil {
		log.Fatalf("write: %v", err)
	}
	n, _ := m.CountCustomers(ctx)
	fmt.Printf("   wrote 1 row; primary now has %d customers\n", n)
	time.Sleep(1 * time.Second) // let WAL stream to the standby
	var standbyCount int
	if err := m.standby.QueryRow(ctx, "SELECT COUNT(*) FROM customers").Scan(&standbyCount); err == nil {
		fmt.Printf("   standby replicated: %d customers (read-only hot standby)\n", standbyCount)
	}

	section("💥 SIMULATE PRIMARY CRASH")
	simulatePrimaryCrash()

	section("🩺 HEALTH-CHECK LOOP → automatic promotion")
	failedOver := m.WatchAndFailover(ctx, 20)
	if !failedOver {
		log.Fatal("expected a failover but none happened")
	}

	section("✍️  WRITE after failover (now served by promoted node)")
	if err := m.Write(ctx,
		"INSERT INTO customers (name, email, tier) VALUES ($1, $2, $3)",
		"Post-Failover User", "post@example.com", "premium"); err != nil {
		log.Fatalf("post-failover write: %v", err)
	}
	n, _ = m.CountCustomers(ctx)
	fmt.Printf("   write succeeded on the new primary; %d customers total\n", n)
	fmt.Println("   (includes both the pre- and post-failover rows → no data lost)")

	section("📊 SUMMARY")
	fmt.Println("   Detection: consecutive failed health probes.")
	fmt.Println("   Action:    pg_promote() the standby, wait until writable, reroute writes.")
	fmt.Println("   Restore the old node with: docker compose up -d postgres_primary")
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

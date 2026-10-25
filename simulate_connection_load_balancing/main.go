package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connection load balancing puts a proxy (HAProxy here; pgpool-II or a cloud load
// balancer elsewhere) in front of several PostgreSQL servers. Clients connect to ONE
// address; the proxy picks a healthy backend for each new connection and stops
// sending traffic to backends that fail their health checks.
//
// This program shows:
//   - separate write (:5506 → primary) and read (:5507 → replicas) endpoints
//   - round-robin spreading of NEW connections across replicas
//   - why a connection pool "pins" its queries to the backends it connected to
//   - a replica failing health checks and being taken out of rotation
//   - the primary acting as a backup for reads when all replicas are down
//   - recovery: replicas rejoin automatically once healthy

const (
	writeURL = "postgres://postgres:postgres@localhost:5506/app_db?sslmode=disable"
	readURL  = "postgres://postgres:postgres@localhost:5507/app_db?sslmode=disable"
	statsURL = "http://localhost:8404/stats;csv"
)

func main() {
	ctx := context.Background()
	banner("⚖️  CONNECTION LOAD BALANCING SIMULATION (HAProxy)")

	// Make sure every backend is up, even if a previous run was interrupted.
	exec.Command("docker", "start", "lb_replica1", "lb_replica2").Run()
	waitServers(ctx, "pg_read", map[string]string{"replica1": "UP", "replica2": "UP"})

	section("🧭 ENDPOINTS — one address for writes, one for reads")
	fmt.Printf("   :5506 (write) → %s\n", whoAmI(ctx, writeURL))
	fmt.Printf("   :5507 (read)  → %s\n", whoAmI(ctx, readURL))
	printStats()

	section("🔄 ROUND-ROBIN — 10 new connections to the read endpoint")
	printDistribution(distribute(ctx, 10))

	section("📌 POOLED CONNECTIONS — balancing happens per CONNECTION, not per query")
	pinnedPool(ctx)

	section("💥 FAILURE — stop replica1, HAProxy health check marks it DOWN")
	dockerCmd("stop", "lb_replica1")
	start := time.Now()
	waitServers(ctx, "pg_read", map[string]string{"replica1": "DOWN"})
	fmt.Printf("   replica1 marked DOWN after %v (inter 1s × fall 2)\n", time.Since(start).Round(100*time.Millisecond))
	printStats()
	printDistribution(distribute(ctx, 6))
	fmt.Println("   no client config changed — the dead replica simply stopped receiving connections")

	section("🛟 ALL REPLICAS DOWN — the primary 'backup' server takes reads")
	dockerCmd("stop", "lb_replica2")
	waitServers(ctx, "pg_read", map[string]string{"replica2": "DOWN"})
	printStats()
	printDistribution(distribute(ctx, 3))

	section("♻️  RECOVERY — replicas restarted, rise 2 → back in rotation")
	dockerCmd("start", "lb_replica1", "lb_replica2")
	start = time.Now()
	waitServers(ctx, "pg_read", map[string]string{"replica1": "UP", "replica2": "UP"})
	fmt.Printf("   both replicas UP again after %v\n", time.Since(start).Round(100*time.Millisecond))
	printStats()
	printDistribution(distribute(ctx, 6))

	section("📊 SUMMARY")
	fmt.Println("   Clients see two stable addresses; HAProxy hides topology and failures.")
	fmt.Println("   Balancing is per connection — long-lived pools stay put until reconnect.")
	fmt.Println("   pgsql-check proves 'PostgreSQL answers', not 'this node is the primary':")
	fmt.Println("   with failover, use Patroni's REST checks (httpchk GET /primary, /replica).")
}

// whoAmI opens a fresh connection and asks the backend for its name and role.
func whoAmI(ctx context.Context, url string) string {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	defer conn.Close(ctx)
	var name string
	var replica bool
	if err := conn.QueryRow(ctx,
		"SELECT current_setting('cluster_name'), pg_is_in_recovery()").Scan(&name, &replica); err != nil {
		return "ERROR: " + err.Error()
	}
	role := "primary (read-write)"
	if replica {
		role = "replica (read-only)"
	}
	return fmt.Sprintf("%s — %s", name, role)
}

// distribute opens n separate connections to the read endpoint, runs one query on
// each, and counts which backend served it.
func distribute(ctx context.Context, n int) map[string]int {
	counts := map[string]int{}
	for range n {
		conn, err := pgx.Connect(ctx, readURL)
		if err != nil {
			counts["connect error"]++
			continue
		}
		var name string
		if err := conn.QueryRow(ctx, "SELECT current_setting('cluster_name')").Scan(&name); err != nil {
			name = "query error"
		}
		counts[name]++
		conn.Close(ctx)
	}
	return counts
}

// pinnedPool shows the classic surprise: a pool of 2 connections running 20 queries
// only ever talks to the 2 backends chosen when those connections were opened.
func pinnedPool(ctx context.Context) {
	cfg, err := pgxpool.ParseConfig(readURL)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.MinConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// 4 workers × 5 queries compete for the 2 pooled connections.
	var mu sync.Mutex
	var wg sync.WaitGroup
	counts := map[string]int{}
	for range 4 {
		wg.Go(func() {
			for range 5 {
				var name string
				var pid int
				if err := pool.QueryRow(ctx,
					"SELECT current_setting('cluster_name'), pg_backend_pid() FROM pg_sleep(0.02)").Scan(&name, &pid); err != nil {
					log.Fatal(err)
				}
				mu.Lock()
				counts[fmt.Sprintf("%s (pid %d)", name, pid)]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	fmt.Println("   pool MaxConns=2, 4 workers × 5 queries:")
	printDistribution(counts)
	fmt.Println("   → all 20 queries reused the two connections opened at startup. To")
	fmt.Println("     rebalance pools, cap connection lifetime (pgxpool MaxConnLifetime)")
	fmt.Println("     or put PgBouncer behind/in front of the balancer.")
}

// serverStatus reads HAProxy's stats CSV and returns server → status for a proxy.
func serverStatus(proxy string) map[string]string {
	resp, err := http.Get(statsURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	r := csv.NewReader(resp.Body)
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil || len(records) == 0 {
		return nil
	}
	// Header starts with "# pxname,svname,...": locate the status column by name.
	header := records[0]
	header[0] = strings.TrimPrefix(header[0], "# ")
	statusCol := slices.Index(header, "status")
	out := map[string]string{}
	for _, rec := range records[1:] {
		if rec[0] != proxy || rec[1] == "FRONTEND" || rec[1] == "BACKEND" {
			continue
		}
		// Statuses may carry transition suffixes like "DOWN 1/2"; keep the word.
		out[rec[1]] = strings.Fields(rec[statusCol])[0]
	}
	return out
}

func printStats() {
	for _, proxy := range []string{"pg_write", "pg_read"} {
		st := serverStatus(proxy)
		parts := make([]string, 0, len(st))
		for _, name := range slices.Sorted(maps.Keys(st)) {
			icon := "🟢"
			if st[name] != "UP" {
				icon = "🔴"
			}
			parts = append(parts, fmt.Sprintf("%s %s %s", icon, name, st[name]))
		}
		fmt.Printf("   HAProxy %-8s %s\n", proxy+":", strings.Join(parts, "   "))
	}
}

// waitServers polls the stats endpoint until every listed server has the wanted
// status — HAProxy reacts on its own schedule (inter/fall/rise), not instantly.
func waitServers(ctx context.Context, proxy string, want map[string]string) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st := serverStatus(proxy)
		ok := st != nil
		for name, status := range want {
			if st[name] != status {
				ok = false
			}
		}
		if ok {
			return
		}
		select {
		case <-ctx.Done():
			log.Fatal(ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
	log.Fatalf("timed out waiting for %s servers %v (is `docker compose up -d` running?)", proxy, want)
}

func printDistribution(counts map[string]int) {
	for _, name := range slices.Sorted(maps.Keys(counts)) {
		fmt.Printf("   %-22s %s %d\n", name, strings.Repeat("█", counts[name]), counts[name])
	}
}

// dockerCmd stops/starts containers to simulate failures, as the failover demo does.
func dockerCmd(args ...string) {
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		log.Fatalf("docker %v: %v\n%s", args, err, out)
	}
	fmt.Printf("   🐳 docker %s\n", strings.Join(args, " "))
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

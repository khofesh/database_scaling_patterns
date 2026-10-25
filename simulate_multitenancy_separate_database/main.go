package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Separate database per tenant: every tenant gets its own PostgreSQL DATABASE,
// cloned from a template, possibly on a different server. It's the strongest
// isolation short of a server per tenant — no shared tables, no shared catalog,
// per-database limits and settings — at the cost of a connection pool per tenant,
// migrations × N databases, and more operational surface.

var servers = map[string]string{"server_a": "localhost:5533", "server_b": "localhost:5534"}

func adminDSN(server, db string) string {
	return fmt.Sprintf("postgres://postgres:postgres@%s/%s?sslmode=disable", servers[server], db)
}

// Each tenant database has its own login role (tenant_x) that owns it. Superusers
// ignore CONNECTION LIMIT, so tenants must NOT connect as postgres. (Passwords are
// derived here for the demo; in production they'd come from a secrets manager.)
func tenantDSN(t tenant) string {
	return fmt.Sprintf("postgres://%s:pw_%s@%s/%s?sslmode=disable", t.dbname, t.slug, servers[t.server], t.dbname)
}

var containers = map[string]string{"server_a": "mt_db_server_a", "server_b": "mt_db_server_b"}

var validSlug = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// Plans map to per-database resource settings.
var plans = map[string]struct {
	connLimit int
	timeout   string
}{
	"standard": {connLimit: 3, timeout: "1s"},
	"premium":  {connLimit: 20, timeout: "10s"},
}

type tenant struct{ slug, server, dbname, plan string }

// Router resolves tenant → (server, database) from the control catalogue and keeps
// one small pool per tenant database.
type Router struct {
	control *pgxpool.Pool
	mu      sync.Mutex
	pools   map[string]*pgxpool.Pool
}

func (r *Router) lookup(ctx context.Context, slug string) tenant {
	t := tenant{slug: slug}
	if err := r.control.QueryRow(ctx,
		"SELECT server, dbname, plan FROM tenants WHERE slug = $1", slug).Scan(&t.server, &t.dbname, &t.plan); err != nil {
		log.Fatalf("lookup %s: %v", slug, err)
	}
	return t
}

// Pool returns (lazily creating) the tenant's own pool.
func (r *Router) Pool(ctx context.Context, slug string) (*pgxpool.Pool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pools[slug]; ok {
		return p, nil
	}
	t := r.lookup(ctx, slug)
	cfg, err := pgxpool.ParseConfig(tenantDSN(t))
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 2
	cfg.MinConns = 2 // keep warm connections, as a real per-tenant pool would
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	r.pools[slug] = p
	return p, nil
}

func (r *Router) Drop(slug string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.pools[slug]; ok {
		p.Close()
		delete(r.pools, slug)
	}
}

func main() {
	ctx := context.Background()
	admin := map[string]*pgxpool.Pool{}
	for name := range servers {
		admin[name] = open(ctx, adminDSN(name, "postgres"))
		defer admin[name].Close()
	}
	r := &Router{control: open(ctx, adminDSN("server_a", "control")), pools: map[string]*pgxpool.Pool{}}
	defer r.control.Close()
	reset(ctx, r, admin)
	fmt.Println("✅ Connected: server_a (5533, shared + control plane), server_b (5534, dedicated)")

	banner("🏦 MULTI-TENANCY: SEPARATE DATABASE PER TENANT")

	section("🆕 PROVISION — CREATE DATABASE … TEMPLATE tenant_template")
	for _, t := range []tenant{
		{"acme", "server_a", "", "standard"},
		{"globex", "server_a", "", "standard"},
		{"initech", "server_a", "", "standard"},
		{"bigbank", "server_b", "", "premium"},
	} {
		took := provision(ctx, r, admin, t.slug, t.server, t.plan)
		fmt.Printf("   %-8s → %s/tenant_%-8s plan=%-8s (%v)\n", t.slug, t.server, t.slug, t.plan, took.Round(time.Millisecond))
	}
	for _, slug := range []string{"acme", "globex", "initech", "bigbank"} {
		p, err := r.Pool(ctx, slug)
		must(err)
		must2(p.Exec(ctx, "INSERT INTO invoices (customer, amount) SELECT $1 || '-cust-' || g, g * 10 FROM generate_series(1, 50) g", slug))
	}

	section("🛡️  FULL ISOLATION — a tenant connection can't even name another database")
	acme, _ := r.Pool(ctx, "acme")
	var n int
	_, err := acme.Exec(ctx, "SELECT count(*) FROM tenant_globex.public.invoices")
	fmt.Printf("   acme: SELECT … FROM tenant_globex.public.invoices → ❌ %s\n", pgMessage(err))
	must(acme.QueryRow(ctx, "SELECT count(*) FROM invoices").Scan(&n))
	fmt.Printf("   acme: SELECT count(*) FROM invoices → %d (only its own database exists for it)\n", n)
	stolen := tenantDSN(tenant{slug: "acme", server: "server_a", dbname: "tenant_acme"})
	stolen = strings.Replace(stolen, "/tenant_acme?", "/tenant_globex?", 1)
	_, err = pgx.Connect(ctx, stolen)
	fmt.Printf("   acme's credentials → connect to tenant_globex → ❌ %s\n", pgMessage(err))

	section("🚦 PER-TENANT RESOURCE LIMITS — CONNECTION LIMIT and statement_timeout")
	noisyNeighbour(ctx, r, "acme", 6)
	for _, slug := range []string{"globex", "bigbank"} {
		p, _ := r.Pool(ctx, slug)
		start := time.Now()
		_, err := p.Exec(ctx, "SELECT pg_sleep(2)")
		result := "✅ completed"
		if err != nil {
			result = "❌ " + pgMessage(err)
		}
		fmt.Printf("   %-8s (%s) 2s report query → %s after %v\n", slug, r.lookup(ctx, slug).plan, result,
			time.Since(start).Round(100*time.Millisecond))
	}

	section("📊 PER-TENANT ACCOUNTING — size and activity per database")
	printAccounting(ctx, admin)

	section("🔧 MIGRATION — every tenant database, one at a time")
	start := time.Now()
	applied := migrate(ctx, r, 2, "ALTER TABLE invoices ADD COLUMN currency TEXT NOT NULL DEFAULT 'USD'")
	fmt.Printf("   v2 applied to %d databases in %v (connect + migrate each)\n", applied, time.Since(start).Round(time.Millisecond))

	section("🚚 MOVE A TENANT — globex outgrew the shared server → server_b")
	start = time.Now()
	moveTenant(ctx, r, admin, "globex", "server_b")
	p, _ := r.Pool(ctx, "globex")
	must(p.QueryRow(ctx, "SELECT count(*) FROM invoices").Scan(&n))
	fmt.Printf("   pg_dump | pg_restore + catalogue update in %v; globex on server_b has %d invoices\n",
		time.Since(start).Round(time.Millisecond), n)

	section("🗑️  OFFBOARD — DROP DATABASE … WITH (FORCE)")
	start = time.Now()
	offboard(ctx, r, admin, "initech")
	fmt.Printf("   tenant_initech dropped in %v — no trace left in shared tables\n", time.Since(start).Round(time.Millisecond))

	section("💥 CONNECTION EXPLOSION — a pool per tenant on a 60-connection server")
	explode(ctx, r, admin)

	section("📊 SUMMARY")
	fmt.Println("   + strongest isolation; per-tenant limits, settings, backup, restore, drop")
	fmt.Println("   + tenants can be placed on / moved to any server")
	fmt.Println("   − one pool per tenant: connections explode (use PgBouncer / lazy pools)")
	fmt.Println("   − migrations × N databases; more databases to monitor and back up")
}

// provision clones the template into a new database and applies the plan's limits.
// CREATE DATABASE can't run inside a transaction block, so this isn't atomic: on
// failure, clean up (DROP DATABASE IF EXISTS) before retrying.
func provision(ctx context.Context, r *Router, admin map[string]*pgxpool.Pool, slug, server, plan string) time.Duration {
	if !validSlug.MatchString(slug) {
		log.Fatalf("invalid slug %q", slug)
	}
	db := "tenant_" + slug
	limits := plans[plan]
	start := time.Now()
	createTenantRole(ctx, admin[server], db, slug)
	for _, sql := range []string{
		"CREATE DATABASE " + db + " OWNER " + db + " TEMPLATE tenant_template",
		fmt.Sprintf("ALTER DATABASE %s WITH IS_TEMPLATE false ALLOW_CONNECTIONS true CONNECTION LIMIT %d", db, limits.connLimit),
		fmt.Sprintf("ALTER DATABASE %s SET statement_timeout = '%s'", db, limits.timeout),
		// Every role may connect to every database by default (PUBLIC has CONNECT).
		// Revoke it so only this tenant's role (the owner) can.
		"REVOKE CONNECT ON DATABASE " + db + " FROM PUBLIC",
	} {
		if _, err := admin[server].Exec(ctx, sql); err != nil {
			log.Fatalf("%s: %v", sql, err)
		}
	}
	// Tables cloned from the template are owned by postgres; hand them to the tenant.
	conn, err := pgx.Connect(ctx, adminDSN(server, db))
	must(err)
	rows, err := conn.Query(ctx, "SELECT format('ALTER TABLE %I OWNER TO %I', tablename, $1::text) FROM pg_tables WHERE schemaname = 'public'", db)
	must(err)
	stmts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	must(err)
	for _, stmt := range stmts {
		must2(conn.Exec(ctx, stmt))
	}
	conn.Close(ctx)
	must2(r.control.Exec(ctx, "INSERT INTO tenants (slug, server, dbname, plan) VALUES ($1, $2, $3, $4)", slug, server, db, plan))
	return time.Since(start)
}

func createTenantRole(ctx context.Context, admin *pgxpool.Pool, role, slug string) {
	must2(admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD 'pw_%s'", role, slug)))
}

// noisyNeighbour opens more simultaneous connections than the tenant's CONNECTION
// LIMIT allows. The excess is refused for that tenant only.
func noisyNeighbour(ctx context.Context, r *Router, slug string, want int) {
	t := r.lookup(ctx, slug)
	var conns []*pgx.Conn
	refused := ""
	for range want {
		c, err := pgx.Connect(ctx, tenantDSN(t))
		if err != nil {
			refused = pgMessage(err)
			continue
		}
		conns = append(conns, c)
	}
	fmt.Printf("   %s (limit %d, its pool already holds 2) opens %d more → %d accepted, %d refused:\n",
		slug, plans[t.plan].connLimit, want, len(conns), want-len(conns))
	fmt.Printf("   ❌ %s\n", refused)
	for _, c := range conns {
		c.Close(ctx)
	}
	g, _ := r.Pool(ctx, "globex")
	var one int
	must(g.QueryRow(ctx, "SELECT 1").Scan(&one))
	fmt.Println("   globex is unaffected — acme's limit only caps acme")
}

func printAccounting(ctx context.Context, admin map[string]*pgxpool.Pool) {
	for _, server := range []string{"server_a", "server_b"} {
		rows, err := admin[server].Query(ctx, `
			SELECT d.datname, pg_size_pretty(pg_database_size(d.datname)), d.datconnlimit,
			       s.numbackends, s.xact_commit
			FROM pg_database d JOIN pg_stat_database s ON s.datid = d.oid
			WHERE d.datname LIKE 'tenant\_%' AND NOT d.datistemplate ORDER BY d.datname`)
		if err != nil {
			log.Fatal(err)
		}
		for rows.Next() {
			var name, size string
			var limit, backends int
			var commits int64
			rows.Scan(&name, &size, &limit, &backends, &commits)
			fmt.Printf("   %s %-16s %8s  conn limit %2d  connected %d  commits %d\n",
				server, name, size, limit, backends, commits)
		}
		rows.Close()
	}
}

// migrate applies a versioned change to every tenant database.
func migrate(ctx context.Context, r *Router, version int, ddl string) int {
	rows, err := r.control.Query(ctx, "SELECT slug FROM tenants ORDER BY slug")
	if err != nil {
		log.Fatal(err)
	}
	slugs, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	applied := 0
	for _, slug := range slugs {
		p, err := r.Pool(ctx, slug)
		must(err)
		must(pgx.BeginFunc(ctx, p, func(tx pgx.Tx) error {
			var has bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&has); err != nil || has {
				return err
			}
			if _, err := tx.Exec(ctx, ddl); err != nil {
				return err
			}
			applied++
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version)
			return err
		}))
	}
	return applied
}

// moveTenant copies a whole tenant database to another server with
// pg_dump -Fc | pg_restore -C, switches the catalogue, then drops the original.
// (Production: block the tenant's writes first, or use logical replication for a
// near-zero-downtime cutover.)
func moveTenant(ctx context.Context, r *Router, admin map[string]*pgxpool.Pool, slug, target string) {
	t := r.lookup(ctx, slug)
	r.Drop(slug)                                         // stop using the old location
	createTenantRole(ctx, admin[target], t.dbname, slug) // roles are per server, not in the dump
	dump := exec.Command("docker", "exec", containers[t.server], "pg_dump", "-U", "postgres", "-Fc", t.dbname)
	restore := exec.Command("docker", "exec", "-i", containers[target], "pg_restore", "-U", "postgres", "-C", "-d", "postgres")
	pipe, err := dump.StdoutPipe()
	must(err)
	restore.Stdin = pipe
	var stderr strings.Builder
	restore.Stderr = &stderr
	must(dump.Start())
	must(restore.Start())
	if err := dump.Wait(); err != nil {
		log.Fatalf("pg_dump: %v", err)
	}
	if err := restore.Wait(); err != nil {
		log.Fatalf("pg_restore: %v\n%s", err, stderr.String())
	}
	// Database-level settings aren't part of a plain dump of the database contents.
	limits := plans[t.plan]
	must2(admin[target].Exec(ctx, fmt.Sprintf("ALTER DATABASE %s CONNECTION LIMIT %d", t.dbname, limits.connLimit)))
	must2(admin[target].Exec(ctx, fmt.Sprintf("ALTER DATABASE %s SET statement_timeout = '%s'", t.dbname, limits.timeout)))
	must2(r.control.Exec(ctx, "UPDATE tenants SET server = $2 WHERE slug = $1", slug, target))
	must2(admin[t.server].Exec(ctx, "DROP DATABASE "+t.dbname+" WITH (FORCE)"))
	must2(admin[t.server].Exec(ctx, "DROP ROLE "+t.dbname))
}

// explode provisions many small tenants on server_a and opens a warm 2-connection
// pool for each — the naive "pool per tenant" design.
func explode(ctx context.Context, r *Router, admin map[string]*pgxpool.Pool) {
	var max, used int
	must(admin["server_a"].QueryRow(ctx,
		"SELECT current_setting('max_connections')::int, (SELECT count(*) FROM pg_stat_activity)").Scan(&max, &used))
	fmt.Printf("   server_a max_connections=%d, already in use=%d\n", max, used)
	opened := 0
	var failure string
	for i := range 40 {
		slug := fmt.Sprintf("small%02d", i)
		provision(ctx, r, admin, slug, "server_a", "standard")
		if _, err := r.Pool(ctx, slug); err != nil {
			failure = fmt.Sprintf("tenant %s: %s", slug, pgMessage(err))
			break
		}
		opened++
	}
	fmt.Printf("   opened warm pools for %d more tenants (2 connections each), then:\n", opened)
	fmt.Printf("   ❌ %s\n", failure)
	fmt.Println("   every tenant now fails to get new connections on server_a. Fixes: a")
	fmt.Println("   pooler (PgBouncer) multiplexing tenants, lazy/idle-closing pools, or")
	fmt.Println("   fewer tenants per server")
	for i := range 40 {
		r.Drop(fmt.Sprintf("small%02d", i))
	}
}

func offboard(ctx context.Context, r *Router, admin map[string]*pgxpool.Pool, slug string) {
	t := r.lookup(ctx, slug)
	r.Drop(slug)
	must2(admin[t.server].Exec(ctx, "DROP DATABASE IF EXISTS "+t.dbname+" WITH (FORCE)"))
	must2(admin[t.server].Exec(ctx, "DROP ROLE IF EXISTS "+t.dbname))
	must2(r.control.Exec(ctx, "DELETE FROM tenants WHERE slug = $1", slug))
}

// reset drops every tenant database on both servers and empties the catalogue.
func reset(ctx context.Context, r *Router, admin map[string]*pgxpool.Pool) {
	for _, server := range slices.Sorted(maps.Keys(servers)) {
		rows, err := admin[server].Query(ctx,
			`SELECT datname FROM pg_database WHERE datname LIKE 'tenant\_%' AND datname <> 'tenant_template'`)
		must(err)
		dbs, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, db := range dbs {
			must2(admin[server].Exec(ctx, "DROP DATABASE "+db+" WITH (FORCE)"))
		}
		rows, err = admin[server].Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname LIKE 'tenant\_%'`)
		must(err)
		roles, _ := pgx.CollectRows(rows, pgx.RowTo[string])
		for _, role := range roles {
			must2(admin[server].Exec(ctx, "DROP ROLE "+role))
		}
	}
	must2(r.control.Exec(ctx, "TRUNCATE tenants"))
}

func pgMessage(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Message
	}
	if err == nil {
		return "no error (!)"
	}
	s := err.Error()
	if i := strings.LastIndex(s, "FATAL: "); i >= 0 {
		return strings.SplitN(s[i+7:], " (SQLSTATE", 2)[0]
	}
	return s
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

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func must2[T any](_ T, err error) { must(err) }

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Shared database, separate schema: each tenant gets its own schema (namespace)
// holding its own copy of every table. Tenants are isolated by NAMESPACE (each
// request sets search_path to the tenant's schema) and, here, also by PRIVILEGE (each
// request switches to a role that can only touch that schema).
//
// This program shows provisioning, routing, isolation, the pooled-connection
// search_path pitfall, per-tenant migrations/customization/backup/offboarding, and
// what happens to the system catalog as tenant count grows.

const (
	adminURL = "postgres://postgres:postgres@localhost:5532/saas?sslmode=disable"
	appURL   = "postgres://app_user:app@localhost:5532/saas?sslmode=disable"
)

// Schema and role names are interpolated into DDL, so only allow a safe alphabet.
var validSlug = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

type App struct {
	admin *pgxpool.Pool // control plane: provisioning, migrations (superuser)
	app   *pgxpool.Pool // data plane: tenant requests (app_user, no own privileges)
}

func main() {
	ctx := context.Background()
	a := &App{admin: open(ctx, adminURL), app: open(ctx, appURL)}
	defer a.admin.Close()
	defer a.app.Close()
	a.dropAllTenants(ctx)
	fmt.Println("✅ Connected to saas (5532) as postgres (control plane) and app_user (requests)")

	banner("🏢 MULTI-TENANCY: SHARED DATABASE, SEPARATE SCHEMA")

	section("🆕 PROVISION — one schema + role per tenant, in one transaction")
	for _, t := range []string{"acme", "globex", "initech"} {
		start := time.Now()
		a.provision(ctx, t)
		fmt.Printf("   tenant %-8s → schema tenant_%-8s role tenant_%-8s (%v)\n",
			t, t, t, time.Since(start).Round(time.Millisecond))
	}
	seed := map[string]int{"acme": 5, "globex": 3, "initech": 8}
	for t, n := range seed {
		must(a.withTenant(ctx, t, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, "INSERT INTO projects (name) SELECT $1 || ' project ' || g FROM generate_series(1, $2) g", t, n)
			return err
		}))
	}

	section("🧭 ROUTING — same unqualified SQL, different schema per tenant")
	for _, t := range []string{"acme", "globex", "initech"} {
		var n int
		var path string
		must(a.withTenant(ctx, t, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*), current_setting('search_path') FROM projects").Scan(&n, &path)
		}))
		fmt.Printf("   tenant %-8s SELECT count(*) FROM projects → %d   (search_path=%s)\n", t, n, path)
	}

	section("🛡️  ISOLATION — acme tries to read globex's schema explicitly")
	err := a.withTenant(ctx, "acme", func(tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, "SELECT count(*) FROM tenant_globex.projects").Scan(&n)
	})
	fmt.Printf("   SELECT … FROM tenant_globex.projects as acme → ❌ %s\n", pgMessage(err))
	fmt.Println("   search_path alone is just a default; the per-tenant ROLE is the real wall")

	section("🧟 POOL PITFALL — session-level SET leaks to the next request")
	a.poolLeak(ctx)

	section("🔧 MIGRATION — roll a schema change out to every tenant schema")
	start := time.Now()
	n := a.migrate(ctx, 2, "ALTER TABLE projects ADD COLUMN archived BOOLEAN NOT NULL DEFAULT false")
	fmt.Printf("   v2 applied to %d schemas in %v (each tenant tracks its own version)\n", n, time.Since(start).Round(time.Millisecond))
	n = a.migrate(ctx, 2, "ALTER TABLE projects ADD COLUMN archived BOOLEAN NOT NULL DEFAULT false")
	fmt.Printf("   re-run: applied to %d schemas (idempotent via schema_migrations)\n", n)

	section("🎨 CUSTOMIZATION — a column only acme's schema has")
	a.execAdmin(ctx, "ALTER TABLE tenant_acme.projects ADD COLUMN sap_cost_center TEXT")
	fmt.Println("   ALTER TABLE tenant_acme.projects ADD COLUMN sap_cost_center TEXT")
	fmt.Println("   possible — but every per-tenant difference is a schema you must maintain")

	section("💾 PER-TENANT BACKUP & OFFBOARDING")
	out, err := exec.Command("docker", "exec", "mt_schema_pg",
		"pg_dump", "-U", "postgres", "-n", "tenant_acme", "saas").Output()
	if err != nil {
		log.Fatalf("pg_dump: %v", err)
	}
	fmt.Printf("   pg_dump -n tenant_acme → %d bytes: one tenant's data, nobody else's\n", len(out))
	start = time.Now()
	a.offboard(ctx, "initech")
	fmt.Printf("   DROP SCHEMA tenant_initech CASCADE + DROP ROLE → gone in %v\n", time.Since(start).Round(time.Millisecond))

	section("📈 SCALE — catalog growth with many tenants")
	before := a.catalogStats(ctx)
	start = time.Now()
	for i := range 300 {
		a.provision(ctx, fmt.Sprintf("bulk%03d", i))
	}
	took := time.Since(start)
	after := a.catalogStats(ctx)
	fmt.Printf("   provisioned 300 tenants in %v (%v each)\n", took.Round(time.Millisecond), (took / 300).Round(time.Microsecond))
	fmt.Printf("   pg_class:     %6d → %6d relations\n", before.relations, after.relations)
	fmt.Printf("   pg_attribute: %6d → %6d columns\n", before.columns, after.columns)
	start = time.Now()
	n = a.migrate(ctx, 3, "CREATE INDEX ON projects (created_at)")
	fmt.Printf("   one migration across %d schemas: %v — grows linearly with tenants\n", n, time.Since(start).Round(time.Millisecond))
	fmt.Println("   10k+ tenants ⇒ millions of catalog rows: slow pg_dump, planning, migrations")

	section("📊 SUMMARY")
	fmt.Println("   + strong logical isolation, per-tenant backup/restore/drop, customization")
	fmt.Println("   + still one database: one pool, cheap cross-tenant admin queries")
	fmt.Println("   − migrations × N schemas; catalog bloat beyond thousands of tenants")
	fmt.Println("   − search_path must be set per TRANSACTION with pooled connections")
}

// withTenant runs fn in a transaction scoped to one tenant. set_config(…, true) is
// SET LOCAL: both the role and search_path revert at COMMIT/ROLLBACK, so the pooled
// connection goes back to the pool clean.
func (a *App) withTenant(ctx context.Context, slug string, fn func(pgx.Tx) error) error {
	var schema string
	if err := a.app.QueryRow(ctx, "SELECT schema_name FROM tenants WHERE slug = $1", slug).Scan(&schema); err != nil {
		return fmt.Errorf("unknown tenant %q: %w", slug, err)
	}
	return pgx.BeginFunc(ctx, a.app, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			"SELECT set_config('role', $1, true), set_config('search_path', $1, true)", schema); err != nil {
			return err
		}
		return fn(tx)
	})
}

// provision creates the tenant's role, schema and tables atomically — PostgreSQL DDL
// is transactional, so a failure halfway leaves nothing behind.
func (a *App) provision(ctx context.Context, slug string) {
	if !validSlug.MatchString(slug) {
		log.Fatalf("invalid tenant slug %q", slug)
	}
	s := "tenant_" + slug
	must(pgx.BeginFunc(ctx, a.admin, func(tx pgx.Tx) error {
		for _, ddl := range []string{
			"CREATE ROLE " + s + " NOLOGIN",
			// PG16+: app_user may SET ROLE to it but does NOT inherit its privileges,
			// so outside an explicit SET ROLE app_user can't see any tenant's tables.
			"GRANT " + s + " TO app_user WITH INHERIT FALSE, SET TRUE",
			"CREATE SCHEMA " + s + " AUTHORIZATION " + s,
			// Create the tables AS the tenant role so it owns them.
			"SET LOCAL ROLE " + s,
			"SET LOCAL search_path = " + s,
			`CREATE TABLE projects (
				id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				name TEXT NOT NULL,
				created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
			`CREATE TABLE tasks (
				id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				project_id BIGINT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
				title TEXT NOT NULL,
				done BOOLEAN NOT NULL DEFAULT false)`,
			"CREATE TABLE schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())",
			"INSERT INTO schema_migrations (version) VALUES (1)",
			"RESET ROLE",
		} {
			if _, err := tx.Exec(ctx, ddl); err != nil {
				return fmt.Errorf("%s: %w", ddl, err)
			}
		}
		_, err := tx.Exec(ctx, "INSERT INTO public.tenants (slug, schema_name) VALUES ($1, $2)", slug, s)
		return err
	}))
}

// migrate applies one versioned change to every tenant schema that doesn't have it
// yet, one transaction per tenant (a failure stops at that tenant; others keep
// their consistent version). Returns how many schemas were changed.
func (a *App) migrate(ctx context.Context, version int, ddl string) int {
	rows, err := a.admin.Query(ctx, "SELECT schema_name FROM tenants ORDER BY slug")
	if err != nil {
		log.Fatal(err)
	}
	schemas, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		log.Fatal(err)
	}
	applied := 0
	for _, s := range schemas {
		must(pgx.BeginFunc(ctx, a.admin, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT set_config('search_path', $1, true)", s); err != nil {
				return err
			}
			var has bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&has); err != nil {
				return err
			}
			if has {
				return nil
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

// poolLeak shows why tenant context must be transaction-scoped. A request uses
// session-level SET (not SET LOCAL) and returns the connection to the pool; the next
// request on that connection forgets to set its tenant and inherits the old one.
func (a *App) poolLeak(ctx context.Context) {
	cfg, _ := pgxpool.ParseConfig(appURL)
	cfg.MaxConns = 1 // force both requests onto the same physical connection
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	must2(pool.Exec(ctx, "SET search_path = tenant_acme")) // ❌ session-level
	fmt.Println("   request 1 (acme):   SET search_path = tenant_acme   ← no LOCAL, no ROLE")
	var path string
	must(pool.QueryRow(ctx, "SHOW search_path").Scan(&path))
	fmt.Printf("   request 2 (globex): forgot to set tenant; connection still has search_path=%s\n", path)
	var n int
	err = pool.QueryRow(ctx, "SELECT count(*) FROM projects").Scan(&n)
	if err != nil {
		fmt.Printf("   → SELECT FROM projects: ❌ %s\n", pgMessage(err))
		fmt.Println("   the leaked search_path points at acme, but app_user (no SET ROLE) has no")
		fmt.Println("   USAGE on tenant_acme, so PostgreSQL skips it. With search_path-only")
		fmt.Println("   isolation this would have returned acme's rows to globex")
	} else {
		fmt.Printf("   → SELECT FROM projects returned %d acme rows to globex ❌\n", n)
	}
}

func (a *App) offboard(ctx context.Context, slug string) {
	s := "tenant_" + slug
	must(pgx.BeginFunc(ctx, a.admin, func(tx pgx.Tx) error {
		for _, sql := range []string{
			"DROP SCHEMA " + s + " CASCADE",
			"DROP ROLE " + s,
			"DELETE FROM tenants WHERE slug = '" + slug + "'",
		} {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return err
			}
		}
		return nil
	}))
}

type catalog struct{ relations, columns int }

func (a *App) catalogStats(ctx context.Context) catalog {
	var c catalog
	must(a.admin.QueryRow(ctx,
		"SELECT (SELECT count(*) FROM pg_class), (SELECT count(*) FROM pg_attribute)").Scan(&c.relations, &c.columns))
	return c
}

// dropAllTenants removes every tenant schema and role from earlier runs.
func (a *App) dropAllTenants(ctx context.Context) {
	rows, err := a.admin.Query(ctx, "SELECT slug FROM tenants")
	if err != nil {
		log.Fatal(err)
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range slugs {
		a.offboard(ctx, s)
	}
}

func (a *App) execAdmin(ctx context.Context, sql string) {
	if _, err := a.admin.Exec(ctx, sql); err != nil {
		log.Fatalf("%s: %v", sql, err)
	}
}

func pgMessage(err error) string {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Message
	}
	if err == nil {
		return "no error (!)"
	}
	return err.Error()
}

func open(ctx context.Context, url string) *pgxpool.Pool {
	db, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	if err := db.Ping(ctx); err != nil {
		log.Fatalf("ping: %v", err)
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

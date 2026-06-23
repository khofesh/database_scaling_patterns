package main

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantDB runs every operation in a transaction that first pins the current
// tenant via the session setting `app.current_tenant`. PostgreSQL's Row-Level
// Security policy then transparently filters every query to that tenant — the
// application can't read or write another tenant's rows even by mistake.
type TenantDB struct {
	pool *pgxpool.Pool // connected as the NON-superuser app_user, so RLS applies
}

// WithTenant opens a transaction, sets app.current_tenant LOCAL to that
// transaction, and runs fn. Using SET LOCAL (via set_config(..., is_local=true))
// scopes the tenant to this transaction only, which is safe with a shared pool
// where connections are reused across tenants.
func (t *TenantDB) WithTenant(ctx context.Context, tenantID int, fn func(tx pgx.Tx) error) error {
	tx, err := t.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.current_tenant', $1, true)", fmt.Sprint(tenantID)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func main() {
	ctx := context.Background()

	// Connect as app_user, NOT postgres. A superuser/table-owner would bypass RLS.
	pool, err := pgxpool.New(ctx,
		"postgres://app_user:app_pass@localhost:5480/saas_db?sslmode=disable")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	db := &TenantDB{pool: pool}
	fmt.Println("✅ Connected as app_user (subject to RLS) to shared saas_db")

	banner("🏢 MULTI-TENANCY (SHARED SCHEMA + RLS) SIMULATION")

	section("👀 EACH TENANT SEES ONLY ITS OWN ROWS")
	for _, tid := range []int{1, 2, 3} {
		listDocuments(ctx, db, tid)
	}

	section("🚫 CROSS-TENANT READ IS INVISIBLE (not an error — just empty)")
	// Tenant 1 (Acme) asks for document id=3, which belongs to tenant 2 (Globex).
	_ = db.WithTenant(ctx, 1, func(tx pgx.Tx) error {
		var title string
		err := tx.QueryRow(ctx, "SELECT title FROM documents WHERE id = 3").Scan(&title)
		if err == pgx.ErrNoRows {
			fmt.Println("   tenant 1 SELECT doc#3 (Globex's) → 0 rows: RLS hid it ✅")
		} else if err != nil {
			fmt.Printf("   unexpected error: %v\n", err)
		} else {
			fmt.Printf("   ❌ LEAK: tenant 1 saw '%s'\n", title)
		}
		return nil
	})

	section("✍️  INSERTS ARE AUTO-SCOPED; WRONG tenant_id IS REJECTED")
	// A correct insert: tenant 1 adds its own document.
	_ = db.WithTenant(ctx, 1, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			"INSERT INTO documents (tenant_id, title, body) VALUES (1, $1, $2)",
			"Acme Q3 plan", "more acme stuff")
		if err != nil {
			fmt.Printf("   valid insert failed: %v\n", err)
		} else {
			fmt.Println("   tenant 1 inserted its own document ✅")
		}
		return nil
	})
	// A malicious insert: tenant 1 tries to plant a row under tenant 2. WITH CHECK
	// rejects it.
	_ = db.WithTenant(ctx, 1, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			"INSERT INTO documents (tenant_id, title, body) VALUES (2, $1, $2)",
			"forged", "should be blocked")
		if err != nil {
			fmt.Println("   tenant 1 INSERT as tenant_id=2 → blocked by WITH CHECK ✅")
		} else {
			fmt.Println("   ❌ LEAK: cross-tenant insert succeeded")
		}
		return nil
	})

	section("🔒 FAIL-CLOSED: no tenant set → query errors, not leaks everything")
	// Bypass WithTenant: run directly on the pool with no app.current_tenant set.
	var n int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM documents").Scan(&n)
	if err != nil {
		fmt.Println("   SELECT with no tenant context → error (current_setting unset) ✅")
	} else {
		fmt.Printf("   ⚠️ returned %d rows with no tenant set — policy too loose\n", n)
	}

	section("📊 SUMMARY")
	fmt.Println("   One schema, one tenant_id column, RLS policy = transparent isolation.")
	fmt.Println("   The app sets the tenant once per transaction; the DB enforces the rest.")
}

func listDocuments(ctx context.Context, db *TenantDB, tenantID int) {
	_ = db.WithTenant(ctx, tenantID, func(tx pgx.Tx) error {
		var name string
		_ = tx.QueryRow(ctx, "SELECT name FROM tenants WHERE id = $1", tenantID).Scan(&name)
		rows, err := tx.Query(ctx, "SELECT title FROM documents ORDER BY id")
		if err != nil {
			return err
		}
		defer rows.Close()
		var titles []string
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				return err
			}
			titles = append(titles, t)
		}
		fmt.Printf("   tenant %d (%-9s) sees %d docs: %v\n", tenantID, name, len(titles), titles)
		return nil
	})
}

func banner(title string) {
	line := "============================================================"
	fmt.Printf("\n%s\n%s\n%s\n", line, title, line)
}

func section(title string) {
	fmt.Printf("\n%s\n--------------------------------------------------\n", title)
}

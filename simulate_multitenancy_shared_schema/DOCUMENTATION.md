# Multi-Tenancy (Shared Schema) Simulation - Line-by-Line Documentation

This document explains the `simulate_multitenancy_shared_schema` project: **what**
each piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Schema](#infrastructure--schema)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of Shared-Schema Multi-Tenancy](#summary-pros-and-cons-of-shared-schema-multi-tenancy)

---

## Project Overview

A SaaS app serving many customers ("tenants") has to keep their data apart. The
densest, cheapest way is to put everyone in the **same tables** with a `tenant_id`
column. The risk is obvious: isolation now depends on every single query
remembering `WHERE tenant_id = ...`. Forget once and you have a data leak.

**Row-Level Security (RLS)** moves that guarantee from application discipline into
the database. You declare a policy once; PostgreSQL then appends the tenant filter
to _every_ statement automatically. The app just declares "I am tenant 2" and the
database enforces the rest.

---

## Infrastructure & Schema

`docker-compose.yml` runs a single PostgreSQL (port 5480). All the interesting
parts are in `init/01_schema.sql`:

### Shared tables with a discriminator

```sql
CREATE TABLE documents (
    id INT, tenant_id INT REFERENCES tenants(id), title, body, created_at
);
CREATE INDEX idx_documents_tenant ON documents (tenant_id);
```

Every tenant-owned row carries its `tenant_id`. The index makes the tenant filter
cheap — important because _every_ query will include it.

### A non-superuser application role

```sql
CREATE ROLE app_user WITH LOGIN PASSWORD 'app_pass';
GRANT SELECT, INSERT, UPDATE, DELETE ON documents TO app_user;
```

**Why a separate role?** PostgreSQL superusers and a table's owner **bypass RLS by
default**. If the app connected as `postgres`, the policies would do nothing. The
app must use an ordinary role.

### Enabling and forcing RLS

```sql
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;
```

`ENABLE` turns policies on for normal roles. `FORCE` applies them even to the table
owner — defense in depth so no connection accidentally escapes isolation.

### The policy

```sql
CREATE POLICY tenant_isolation ON documents
    USING      (tenant_id = current_setting('app.current_tenant')::int)
    WITH CHECK (tenant_id = current_setting('app.current_tenant')::int);
```

| Clause       | Applies to               | Effect                                             |
| ------------ | ------------------------ | -------------------------------------------------- |
| `USING`      | SELECT / UPDATE / DELETE | Which existing rows are _visible_ to the statement |
| `WITH CHECK` | INSERT / UPDATE          | Which new/changed rows are _allowed_ to be written |

`current_setting('app.current_tenant')` reads a session variable the app sets per
transaction. If it's unset, `current_setting` raises an error — which means **no
tenant context fails closed** rather than exposing everything.

---

## Application Code (main.go)

### Connecting as the right role

```go
pgxpool.New(ctx, "postgres://app_user:app_pass@localhost:5480/saas_db?...")
```

Connecting as `app_user` (not `postgres`) is what makes RLS take effect.

### Pinning the tenant per transaction — `WithTenant`

```go
tx := t.pool.Begin(ctx)
tx.Exec(ctx, "SELECT set_config('app.current_tenant', $1, true)", fmt.Sprint(tenantID))
fn(tx)
tx.Commit(ctx)
```

| Step                    | What It Does                                   | Why We Do It                                       |
| ----------------------- | ---------------------------------------------- | -------------------------------------------------- |
| `Begin` a transaction   | Scopes the tenant setting to this unit of work | A pooled connection serves many tenants over time  |
| `set_config(..., true)` | Sets `app.current_tenant` **LOCAL** to the TX  | The third arg `true` = local; auto-reset at TX end |
| run `fn`, then `Commit` | Execute the tenant's queries                   | All of them inherit the RLS filter automatically   |

**Why `set_config(..., is_local := true)` instead of a plain `SET`?** With a
connection pool, the same physical connection is reused across tenants. A
session-level `SET` would leak the previous tenant's id into the next borrower.
Transaction-local config resets on commit/rollback, so each transaction starts
clean.

### What the demo proves

1. **Per-tenant visibility** — looping tenants 1/2/3, each `SELECT * FROM documents`
   (no `WHERE`!) returns only that tenant's rows.
2. **Cross-tenant read** — tenant 1 selecting Globex's `id=3` gets `0 rows`, not an
   error and not the row: RLS simply hides it.
3. **Forged insert** — tenant 1 inserting a row with `tenant_id = 2` is rejected by
   `WITH CHECK`.
4. **Fail-closed** — a query run straight on the pool (no `WithTenant`, so no tenant
   set) errors because `current_setting` is unset — it does **not** dump all rows.

---

## Summary: Pros and Cons of Shared-Schema Multi-Tenancy

### Pros

| Benefit                   | Explanation                                                       |
| ------------------------- | ----------------------------------------------------------------- |
| **Highest density**       | Thousands of tenants in one schema, one connection pool           |
| **Lowest ops cost**       | One database to back up, migrate, monitor, and tune               |
| **DB-enforced isolation** | RLS catches the query that forgot its tenant filter               |
| **Simple onboarding**     | A new tenant is just a new `tenant_id`, no schema/DB to provision |

### Cons

| Drawback                      | Explanation                                                   |
| ----------------------------- | ------------------------------------------------------------- |
| **Logical, not physical**     | A policy/role misconfig can still leak across tenants         |
| **Noisy neighbors**           | One tenant's heavy load affects everyone on the shared tables |
| **Limited per-tenant tuning** | Hard to give one tenant different indexes/storage             |
| **Big blast radius**          | A bad migration or corruption hits all tenants at once        |

### When to Use Shared-Schema Multi-Tenancy

**Good fit:** SaaS with many small/medium tenants where density and low operational
cost matter most, and per-tenant customization is minimal. **Move toward
separate-schema or separate-database** as individual tenants grow large, demand
isolation/compliance guarantees, or need independent tuning and backup/restore.

## Key Takeaways

1. One schema + a `tenant_id` column is the densest, cheapest tenancy model.
2. RLS enforces isolation in the DB, not in every hand-written query.
3. The app must connect as a non-superuser, non-owner role for RLS to apply.
4. Set the tenant **transaction-locally** so a pooled connection can't leak it.
5. Design it to **fail closed**: no tenant context ⇒ no rows, not all rows.

## go run output

```shell
$ go run main.go
✅ Connected as app_user (subject to RLS) to shared saas_db

============================================================
🏢 MULTI-TENANCY (SHARED SCHEMA + RLS) SIMULATION
============================================================

👀 EACH TENANT SEES ONLY ITS OWN ROWS
--------------------------------------------------
   tenant 1 (Acme Corp) sees 2 docs: [Acme roadmap Acme invoices]
   tenant 2 (Globex   ) sees 2 docs: [Globex strategy Globex contacts]
   tenant 3 (Initech  ) sees 1 docs: [Initech memo]

🚫 CROSS-TENANT READ IS INVISIBLE (not an error — just empty)
--------------------------------------------------
   tenant 1 SELECT doc#3 (Globex's) → 0 rows: RLS hid it ✅

✍️  INSERTS ARE AUTO-SCOPED; WRONG tenant_id IS REJECTED
--------------------------------------------------
   tenant 1 inserted its own document ✅
   tenant 1 INSERT as tenant_id=2 → blocked by WITH CHECK ✅

🔒 FAIL-CLOSED: no tenant set → query errors, not leaks everything
--------------------------------------------------
   SELECT with no tenant context → error (current_setting unset) ✅

📊 SUMMARY
--------------------------------------------------
   One schema, one tenant_id column, RLS policy = transparent isolation.
   The app sets the tenant once per transaction; the DB enforces the rest.
```

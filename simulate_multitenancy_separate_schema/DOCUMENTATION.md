# Multi-Tenancy (Separate Schema) Simulation - Line-by-Line Documentation

This document explains the `simulate_multitenancy_separate_schema` project: **what**
each piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Schema (init/01_schema.sql)](#schema-init01_schemasql)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

A PostgreSQL **schema** is a namespace inside a database. Giving each tenant a schema
keeps their tables physically separate while sharing one server, one database and one
connection pool. The two engineering problems are **routing** (making unqualified SQL
hit the right schema) and **isolation** (making sure it can't hit the wrong one).

---

## Schema (init/01_schema.sql)

```sql
CREATE TABLE tenants (slug TEXT PRIMARY KEY, schema_name TEXT NOT NULL UNIQUE, plan TEXT, created_at TIMESTAMPTZ);
CREATE ROLE app_user LOGIN PASSWORD 'app';
GRANT SELECT ON tenants TO app_user;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
```

- **`tenants`** — the control-plane catalogue in `public`: which schema belongs to which
  tenant. Tenant schemas are created at runtime, not in init.
- **`app_user`** — the role the application connects as. It's not a superuser and has
  no privileges on tenant data of its own; it can only read the catalogue.
- **`REVOKE CREATE ON SCHEMA public`** — PG15+ default, stated explicitly: nobody can
  sneak tables into the shared schema.

---

## Application Code (main.go)

### Two pools

- `admin` (postgres) — the control plane: provisioning, migrations, offboarding.
- `app` (app_user) — the data plane: every tenant request.

### `provision` — transactional onboarding

In one transaction:

```sql
CREATE ROLE tenant_acme NOLOGIN;
GRANT tenant_acme TO app_user WITH INHERIT FALSE, SET TRUE;
CREATE SCHEMA tenant_acme AUTHORIZATION tenant_acme;
SET LOCAL ROLE tenant_acme;
SET LOCAL search_path = tenant_acme;
CREATE TABLE projects (...); CREATE TABLE tasks (...); CREATE TABLE schema_migrations (...);
INSERT INTO schema_migrations VALUES (1);
RESET ROLE;
INSERT INTO public.tenants ...;
```

- PostgreSQL DDL is **transactional**: if any step fails, the role, schema and tables all
  disappear with the rollback. No half-provisioned tenants.
- **`WITH INHERIT FALSE, SET TRUE`** (PG16+ grant options): `app_user` may `SET ROLE
  tenant_acme`, but does *not* automatically inherit its privileges. Without this, the
  app role would inherit every tenant's privileges and the role layer would be useless.
- Tables are created **as the tenant role** so it owns them.
- Slugs are validated with a regex because identifiers are interpolated into DDL
  (parameters can't be used for identifiers).

### `withTenant` — routing per transaction

```go
tx.Exec(ctx, "SELECT set_config('role', $1, true), set_config('search_path', $1, true)", schema)
```

`set_config(name, value, true)` is the function form of `SET LOCAL`: the setting lasts
until the transaction ends. The schema name is passed as a *parameter*, which `SET`
can't do. When the transaction commits or rolls back, the connection returns to the
pool as plain `app_user` with the default `search_path` — nothing leaks.

### Isolation

Acme runs `SELECT count(*) FROM tenant_globex.projects` — fully qualified, bypassing
`search_path`. It fails with `permission denied for schema tenant_globex`, because the
current role `tenant_acme` has no `USAGE` on that schema. `search_path` is only a
default lookup path; privileges are the actual boundary.

### `poolLeak` — the classic bug

A pool with `MaxConns = 1` forces two "requests" onto the same connection. Request 1
runs a session-level `SET search_path = tenant_acme` (no `LOCAL`, no role). Request 2
forgets to set a tenant and inherits that `search_path`. With search_path-only isolation
it would read acme's rows. Here it gets `relation "projects" does not exist`: `app_user`
has no `USAGE` on `tenant_acme`, so PostgreSQL skips that schema in the path. Lesson:
scope tenant context to the transaction, and back it with privileges.

### `migrate` — schema changes × N

For each schema, one transaction: set `search_path`, check
`schema_migrations` for the version, apply the DDL, record the version. Per-tenant
version tracking makes the rollout **idempotent and resumable** — a failure at tenant
150 leaves 1–149 migrated, 150+ untouched, each consistent. Re-running applies to zero
schemas. Duration grows linearly with tenant count (~1 s for 300 here; real migrations
on real data take far longer).

### Customization

`ALTER TABLE tenant_acme.projects ADD COLUMN sap_cost_center TEXT` — only acme has it.
Possible, but each divergence complicates every future migration.

### Backup and offboarding

- `pg_dump -n tenant_acme` (run via `docker exec`) exports exactly one tenant — handy for
  data-portability requests or restoring a single tenant.
- `offboard`: `DROP SCHEMA tenant_initech CASCADE`, `DROP ROLE`, delete from catalogue —
  in one transaction, milliseconds, no row-by-row deletes.

### Scale — catalog growth

Provisioning 300 tenants adds ~12 relations each (tables, indexes, sequences, TOAST) to
`pg_class` and ~70 columns each to `pg_attribute`. At 10,000+ tenants the catalog holds
millions of rows: `pg_dump` slows dramatically, relcache memory per backend grows, and
every migration loops over all schemas. That's the practical ceiling of this model.

### `dropAllTenants`

Offboards every tenant in the catalogue at startup, making runs repeatable.

---

## Summary: Pros and Cons

**Pros**

- **Strong logical isolation** — separate tables plus per-tenant roles.
- **Per-tenant operations** — backup/restore (`pg_dump -n`), offboarding (`DROP SCHEMA`),
  customization.
- **Shared infrastructure** — one server, one database, one pool.
- **No `tenant_id` columns** or RLS policies in every query.

**Cons**

- **Migrations × N** — slow, must be idempotent and resumable.
- **Catalog bloat** — limits practical tenant counts to the low thousands.
- **Routing discipline** — tenant context must be transaction-scoped with pooled
  connections.
- **Noisy neighbours** — tenants still share CPU, memory and I/O.
- **Cross-tenant analytics** — needs `UNION ALL` across schemas or an ETL.

**When to use:** hundreds to low thousands of B2B tenants needing stronger isolation
than RLS. **When not to:** huge tenant counts (use shared schema) or tenants needing
resource isolation (use separate databases/servers).

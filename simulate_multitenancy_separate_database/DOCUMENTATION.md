# Multi-Tenancy (Separate Database) Simulation - Line-by-Line Documentation

This document explains the `simulate_multitenancy_separate_database` project: **what**
each piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Init Scripts](#infrastructure--init-scripts)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

A PostgreSQL server ("cluster") hosts many databases. Databases are strongly separated:
a session is bound to exactly one, catalogs are per database, and privileges to connect
are per database. Using one per tenant gives near-dedicated isolation on shared hardware,
with the option to move a tenant to its own server later.

---

## Infrastructure & Init Scripts

### docker-compose.yml

- **`server_a`** (5533) — shared server for standard tenants and the `control` database.
  `max_connections=60` is deliberately low to make the connection problem visible.
- **`server_b`** (5534) — dedicated server for premium tenants and moved tenants.

Init scripts are mounted individually so `server_b` gets the template but not the
control catalogue.

### init/01_template.sh (both servers)

Creates `tenant_template` with the tenant schema (`invoices`, `schema_migrations` at
version 1), then:

```sql
ALTER DATABASE tenant_template WITH IS_TEMPLATE true ALLOW_CONNECTIONS false;
```

Cloning requires no active connections to the source, so connections to the template
are blocked. Changing the template affects only tenants created afterwards.

### init/02_control.sql (server_a)

```sql
CREATE TABLE tenants (slug TEXT PRIMARY KEY, server TEXT, dbname TEXT, plan TEXT, ..., UNIQUE (server, dbname));
```

The catalogue the application uses to route tenants.

---

## Application Code (main.go)

### Credentials — `adminDSN` / `tenantDSN`

Admin operations connect as `postgres`; tenant traffic connects as the tenant's own role
(`tenant_acme`). This matters: **superusers ignore `CONNECTION LIMIT`**, and separate
credentials mean a leaked tenant password exposes one tenant only.

### `provision`

```sql
CREATE ROLE tenant_acme LOGIN PASSWORD '…';
CREATE DATABASE tenant_acme OWNER tenant_acme TEMPLATE tenant_template;
ALTER DATABASE tenant_acme WITH IS_TEMPLATE false ALLOW_CONNECTIONS true CONNECTION LIMIT 3;
ALTER DATABASE tenant_acme SET statement_timeout = '1s';
REVOKE CONNECT ON DATABASE tenant_acme FROM PUBLIC;
-- then, connected to tenant_acme:
ALTER TABLE invoices OWNER TO tenant_acme; ...
```

- `CREATE DATABASE` cannot run inside a transaction, so provisioning isn't atomic; a
  failed run must be cleaned up with `DROP DATABASE IF EXISTS` before retrying.
- Cloned settings: the clone inherits the template's `ALLOW_CONNECTIONS false`, so it's
  re-enabled.
- **`CONNECTION LIMIT`** and **`ALTER DATABASE … SET`** are per-database resource
  controls derived from the tenant's plan.
- **`REVOKE CONNECT … FROM PUBLIC`** — by default *every* role may connect to *every*
  database; without this, acme's role could open a session in globex's database.
- Tables cloned from the template are owned by `postgres`; ownership is transferred so
  the tenant role can use and migrate them.

### `Router`

Looks tenants up in `control.tenants` and lazily creates a pool per tenant (`MinConns =
MaxConns = 2`, i.e. warm). Every application instance holds pools for every active
tenant — the root of the connection problem below.

### Isolation

- `SELECT … FROM tenant_globex.public.invoices` → `cross-database references are not
  implemented`. There is no SQL path from one database to another.
- Connecting to `tenant_globex` with acme's credentials → `permission denied for database`.

### `noisyNeighbour` — per-database limits

Acme's pool holds 2 connections; acme then tries 6 more. Only 1 succeeds (limit 3); the
rest get `too many connections for database "tenant_acme"`. Globex is unaffected. Then a
2-second query: globex (standard, `statement_timeout = 1s`) is cancelled; bigbank
(premium, `10s`) completes. Settings set with `ALTER DATABASE … SET` apply to every new
session in that database automatically.

### `printAccounting`

Joins `pg_database` with `pg_stat_database` for per-tenant size, connection limit,
current connections and commits — the raw material for billing and noisy-neighbour
detection.

### `migrate`

For each tenant database, in one transaction: check `schema_migrations`, apply the DDL,
record the version. Each database must be connected to separately; time grows with
tenant count.

### `moveTenant` — rebalancing

1. Close the tenant's pool; create its role on the target server (roles are
   cluster-wide objects, not in a database dump).
2. `docker exec server_a pg_dump -Fc tenant_globex | docker exec -i server_b pg_restore
   -C -d postgres` — the Go program connects the two processes' stdout/stdin. `-C`
   creates the database on the target.
3. Re-apply database-level settings, update the catalogue, drop the original database
   and role.

Production versions block writes during the copy or use logical replication for a
near-zero-downtime cutover.

### Offboarding

`DROP DATABASE … WITH (FORCE)` terminates any remaining sessions and removes the
database; then the role and catalogue entry go. Nothing remains in shared tables.

### `explode` — connection explosion

Provisions small tenants on `server_a` and opens a warm 2-connection pool for each.
After ~27 tenants, the server refuses new connections (`remaining connection slots are
reserved for roles with the SUPERUSER attribute`) — for **every** tenant on that server.
With several app instances the ceiling arrives even sooner. Fixes: a pooler such as
PgBouncer between app and database, pools that open lazily and close when idle, or fewer
tenants per server.

### `reset`

Drops every `tenant_%` database and role on both servers and truncates the catalogue.

---

## Summary: Pros and Cons

**Pros**

- **Strongest isolation** short of dedicated servers: separate catalog, credentials,
  and connect privileges.
- **Per-tenant resource controls** — connection limits and settings per database.
- **Per-tenant lifecycle** — backup, restore, move and drop as a unit.
- **Flexible placement** — big tenants on dedicated servers.

**Cons**

- **Connections** — a pool per tenant multiplies quickly; needs a pooler.
- **Migrations × N** and N databases to monitor and back up.
- **Non-atomic provisioning** — `CREATE DATABASE` can't run in a transaction.
- **Per-database overhead** — catalog cache per backend, more files, checkpoints on
  drop.
- **No cross-tenant SQL** — analytics need ETL.

**When to use:** a modest number of large or regulated tenants. **When not to:**
thousands of small tenants — prefer shared or separate-schema models.

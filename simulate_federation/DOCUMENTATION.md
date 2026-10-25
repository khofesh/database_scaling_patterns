# Federation / Functional Partitioning Simulation - Line-by-Line Documentation

This document explains the `simulate_federation` project: **what** each piece does,
**why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Schemas](#infrastructure--schemas)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

A monolithic database couples every domain: one schema, one server, one maintenance
window, one blast radius. Federation splits it along domain lines. It's often the first
"scale-out" step because it requires no change to how individual tables work — only to
how the application finds them — and it's the database side of a move to services.

---

## Infrastructure & Schemas

### docker-compose.yml

Three independent PostgreSQL 18 servers: `users_db` (5522), `orders_db` (5523),
`analytics_db` (5524). `analytics_db` uses `depends_on: condition: service_healthy`
because its init script connects to the other two. Health checks use `-h 127.0.0.1` so
they pass only after init scripts finish.

### init/users and init/orders

`users` (5,000 rows) and `orders` (50,000 rows). The important line:

```sql
user_id BIGINT NOT NULL,           -- logical reference to users_db.users.id
```

There is **no** `REFERENCES users(id)` — PostgreSQL foreign keys can't cross
databases. The relationship still exists in the domain model, but nothing enforces it.

### init/analytics/01_fdw.sql

```sql
CREATE EXTENSION postgres_fdw;
CREATE SERVER orders_srv FOREIGN DATA WRAPPER postgres_fdw
    OPTIONS (host 'orders_db', port '5432', dbname 'orders', use_remote_estimate 'true');
CREATE USER MAPPING FOR postgres SERVER orders_srv OPTIONS (user 'postgres', password 'postgres');
IMPORT FOREIGN SCHEMA public FROM SERVER orders_srv INTO orders_remote;
```

- A **server** describes a remote database; a **user mapping** supplies credentials
  (use a read-only role in production).
- **`IMPORT FOREIGN SCHEMA`** creates foreign tables matching the remote definitions,
  so they needn't be declared by hand. Each domain gets its own local schema
  (`users_remote`, `orders_remote`).
- **`use_remote_estimate`** makes the planner ask the remote for row estimates
  (via `EXPLAIN`), improving join planning at the cost of extra round trips.

---

## Application Code (main.go)

### `Federation` and domain routing

One pool per domain database. Requests are routed by what they touch: `GET /users/42` →
`users_db`; `POST /orders` → both. `placeOrder` validates the user in `users_db` and then
inserts in `orders_db`: **two separate transactions**. If the user is deleted between
the two, or the second call fails after the first succeeded, nothing rolls back the
other side — this is where sagas and the transactional outbox come in.

### `topCustomersAppJoin` — join in the application

1. `orders_db`: `SELECT user_id, sum(amount) … GROUP BY user_id ORDER BY 2 DESC LIMIT 5`.
2. `users_db`: `SELECT id, name, country FROM users WHERE id = ANY($1)` with all five ids.
3. Merge in Go.

Two round trips regardless of result size. The anti-pattern to avoid is looping and
querying users one by one (N+1).

### Federated query — `postgres_fdw`

The same question as one SQL statement on `analytics_db`, joining
`orders_remote.orders` with `users_remote.users`. `printRemoteSQL` runs
`EXPLAIN (VERBOSE)` and prints the `Remote SQL:` lines — exactly what's sent to each
server:

```
SELECT user_id, sum(amount)::double precision FROM public.orders
GROUP BY 1 ORDER BY sum(amount)::double precision DESC NULLS FIRST LIMIT 5::bigint
```

The aggregate, sort and limit were **pushed down**: `orders_db` did the heavy work and
returned five rows. The users side, however, was fetched whole (`SELECT id, name,
country FROM public.users`) for a local hash join — pushdown is a planner decision, and
inspecting Remote SQL is how you find out what happened.

### When federated joins get expensive

"Revenue by country" needs every order joined to its user's country. A join between two
*different* servers can't be pushed to either, so `postgres_fdw` streams all orders
(`SELECT user_id, amount FROM public.orders`) and all users to `analytics_db` and joins
locally. Fine at 50k rows; painful at 500M. For heavy analytics, replicate domain data
into the analytics store instead (logical replication, CDC, ETL).

### Lost guarantee — orphans

`INSERT INTO orders (user_id, …) VALUES (999999, …)` succeeds: `orders_db` has no way to
check `users_db`. An orphan check through fdw (`NOT EXISTS` against the remote users)
finds it. Integrity becomes an application responsibility: validate on write, react to
deletion events, and reconcile periodically.

### `printSizes`

Shows each database's size: separate servers that can be scaled, tuned (e.g. different
`shared_buffers`/`work_mem`) and backed up on separate schedules.

---

## Summary: Pros and Cons

**Pros**

- **Independent scaling and tuning** per domain; separate failure domains.
- **Clear ownership** — teams own their schema and release cadence.
- **Smaller databases** — faster backups, restores, vacuum and upgrades.
- **No change to table internals** — simpler than sharding.

**Cons**

- **No cross-domain JOINs** — app joins, fdw, or replicated copies instead.
- **No foreign keys or ACID transactions across domains** — sagas, outbox, reconciliation.
- **More infrastructure** — servers, pools, credentials, monitoring.
- **Reporting complexity** — needs a federated layer or data pipeline.

**When to use:** separable domains with different load or ownership. **When not to:**
when most queries join across those domains — keep them together (and scale with
replicas or sharding instead).

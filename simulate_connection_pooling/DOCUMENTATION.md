# Connection Pooling Simulation - Line-by-Line Documentation

This document explains the `simulate_connection_pooling` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure (docker-compose.yml)](#infrastructure-docker-composeyml)
3. [Schema (init/01_create_schema.sql)](#schema-init01_create_schemasql)
4. [Application Code (main.go)](#application-code-maingo)
5. [Summary: Pros and Cons of Connection Pooling](#summary-pros-and-cons-of-connection-pooling)

---

## Project Overview

A PostgreSQL connection is **not cheap**: each one is a separate OS process with
its own memory. `max_connections` bounds how many can exist at once, and going
near that limit degrades the whole server. Meanwhile, web apps tend to open
_many_ short-lived connections.

A **connection pooler** breaks the 1:1 mapping between client connections and
server connections. **PgBouncer** accepts thousands of cheap client connections
and forwards their work over a small, long-lived set of server connections.

---

## Infrastructure (docker-compose.yml)

### PostgreSQL with a tiny connection limit

```yaml
postgres:
  image: postgres:18-bookworm
  command: ["postgres", "-c", "max_connections=20"]
```

| Line                              | What It Does                          | Why We Do It                                          |
| --------------------------------- | ------------------------------------- | ----------------------------------------------------- |
| `command: ... max_connections=20` | Overrides the server connection limit | Makes exhaustion easy to reproduce in a demo          |
| `ports: "5440:5432"`              | Exposes the raw database              | The "direct" path connects here, bypassing the pooler |

### PgBouncer

```yaml
pgbouncer:
  image: edoburu/pgbouncer:v1.24.1-p1
  environment:
    DB_HOST: postgres
    POOL_MODE: transaction
    MAX_CLIENT_CONN: "1000"
    DEFAULT_POOL_SIZE: "5"
    AUTH_TYPE: scram-sha-256
    ADMIN_USERS: postgres
  ports:
    - "6432:5432"
```

| Setting                    | What It Does                                     | Why We Do It                                                |
| -------------------------- | ------------------------------------------------ | ----------------------------------------------------------- |
| `POOL_MODE: transaction`   | Returns a server conn to the pool after each txn | Highest practical throughput; the common production default |
| `MAX_CLIENT_CONN: 1000`    | How many clients may connect to PgBouncer        | The "cheap" side — can be huge                              |
| `DEFAULT_POOL_SIZE: 5`     | Server connections per (user, database)          | The "expensive" side — kept small to protect the server     |
| `AUTH_TYPE: scram-sha-256` | How clients authenticate to PgBouncer            | Matches PostgreSQL 18's default password encryption         |
| `ADMIN_USERS: postgres`    | Who may use the admin console                    | Lets us run `SHOW POOLS`                                    |
| `ports: "6432:5432"`       | Exposes the pooler                               | The "pooled" path connects here                             |

**Why these two numbers matter:** `MAX_CLIENT_CONN` (1000) ≫ `DEFAULT_POOL_SIZE`
(5). That ratio _is_ connection pooling — many clients, few backends.

---

## Schema (init/01_create_schema.sql)

A 50-row `accounts` table. The workload is intentionally trivial (`pg_sleep`,
counts) so the variable under test is **connection handling**, not query cost.

---

## Application Code (main.go)

### Three connection strings

```go
directURL    = "postgres://...@localhost:5440/app_db..." // straight to PostgreSQL
pooledURL    = "postgres://...@localhost:6432/app_db..." // through PgBouncer
adminConsole = "postgres://...@localhost:6432/pgbouncer..." // PgBouncer admin DB
```

The `pgbouncer` "database" is virtual — it exposes admin commands like
`SHOW POOLS` rather than real tables.

### newPool — building a pool, pooler-aware

```go
cfg.MaxConns = maxConns
if pooled {
    cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
}
```

| Line                            | Why We Do It                                                                                                                                                                                                                                               |
| ------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cfg.MaxConns = maxConns`       | Lets each demo dial the concurrency up or down                                                                                                                                                                                                             |
| `DefaultQueryExecMode = simple` | **Critical for transaction pooling.** pgx normally caches _server-side prepared statements_; in transaction mode a server connection hops between clients, so those prepared statements vanish and queries fail. The simple protocol avoids them entirely. |

This is the single most common foot-gun when putting pgx behind PgBouncer.

### demonstrateExhaustion — the problem

```go
const concurrency = 30
pool, _ := newPool(ctx, directURL, concurrency, false)
runLoad(concurrency, func() {
    _, err := pool.Exec(ctx, "SELECT pg_sleep(0.3)")
    ...
})
```

| Step                       | What Happens                                                       |
| -------------------------- | ------------------------------------------------------------------ |
| 30 goroutines, direct pool | The pool tries to open 30 server connections                       |
| Server allows only 20      | Excess acquisitions fail: _"too many clients already"_             |
| `pg_sleep(0.3)`            | Holds each connection long enough that they pile up simultaneously |

`sampleBackends` runs in parallel and reports the **peak** client backends — it
tops out near the server limit, not 30.

### demonstrateMultiplexing — the solution

```go
const concurrency = 100
pool, _ := newPool(ctx, pooledURL, concurrency, true)
runLoad(concurrency, func() {
    _, err := pool.Exec(ctx, "SELECT pg_sleep(0.05)")
    ...
})
```

| Step                         | What Happens                                                     |
| ---------------------------- | ---------------------------------------------------------------- |
| 100 goroutines via PgBouncer | All 100 connect to PgBouncer (well under `MAX_CLIENT_CONN`)      |
| Pool size 5                  | PgBouncer runs them over 5 server connections, queueing the rest |
| Result                       | Every query succeeds; peak server backends ≈ 5, **not** 100      |

This is the headline result: **5× the server's entire connection budget served
concurrently, using a quarter of it.**

### sampleBackends — measuring concurrency

```sql
SELECT COUNT(*) FROM pg_stat_activity
WHERE datname = 'app_db' AND backend_type = 'client backend';
```

Polls every 15 ms for a fixed window and keeps the maximum. `backend_type =
'client backend'` filters out PostgreSQL's own background workers so we count
only real client sessions. Running it in a goroutine lets us observe the server
_while_ the load test is in flight.

### showPgBouncerPools — the pooler's own view

```go
conn.Query(ctx, "SHOW POOLS")  // sv_active, cl_waiting, pool_mode, ...
```

`SHOW POOLS` is PgBouncer's admin command. `sv_active` is server connections in
use; `cl_waiting` is clients queued for one. We look columns up by name because
PgBouncer's column set varies across versions.

---

## Summary: Pros and Cons of Connection Pooling

### Pros

| Benefit                  | Explanation                                     |
| ------------------------ | ----------------------------------------------- |
| **Handles many clients** | Thousands of clients over a few dozen backends  |
| **Lower memory/CPU**     | Fewer backend processes on the server           |
| **Faster requests**      | Reuses warm connections instead of reconnecting |
| **Stability**            | Protects the server from connection storms      |

### Cons

| Drawback                  | Explanation                                                         |
| ------------------------- | ------------------------------------------------------------------- |
| **Lost session state**    | Transaction mode breaks `SET`, advisory locks, server-side prepares |
| **Extra hop / component** | One more network hop and a process to run and monitor               |
| **Sizing is tricky**      | Too small → queuing; too large → defeats the purpose                |
| **Protocol limitations**  | Some features require session mode (lower throughput)               |

### When to Use Connection Pooling

**Almost always** for web/serverless apps with many short requests. Use
**transaction** mode by default; fall back to **session** mode only for clients
that need session-scoped features. Size `DEFAULT_POOL_SIZE` to roughly your
number of CPU cores × a small factor, not your number of clients.

## Key Takeaways

1. Client connections are cheap; server connections are not — decouple them.
2. Transaction pooling gives the best throughput but forbids session state.
3. Behind a transaction pooler, use the simple query protocol (no server-side
   prepared statements).
4. Size the pool to the server's capacity, not the client count.
5. Watch `cl_waiting` in `SHOW POOLS` — sustained waiting means the pool is too small.

## go run output

```shell
$ go run main.go
✅ Connected to PostgreSQL (max_connections=20) and PgBouncer

============================================================
🏊 CONNECTION POOLING SIMULATION
============================================================

💥 DIRECT CONNECTIONS EXHAUST THE SERVER
--------------------------------------------------
   Firing 30 concurrent direct queries at a 20-connection server...
   Succeeded: 19   Failed (too many clients): 11
   Peak client backends seen on the server: 20

♻️  PGBOUNCER MULTIPLEXES MANY CLIENTS ONTO FEW BACKENDS
--------------------------------------------------
   Firing 100 concurrent queries through PgBouncer (pool size 5)...
   Succeeded: 100   Failed: 0
   Peak client backends seen on the server: 6 (≈ pool size, not 100)

📊 PGBOUNCER POOL STATS (SHOW POOLS)
--------------------------------------------------
   db=app_db mode=transaction server_active=0 client_waiting=0
   db=pgbouncer mode=statement server_active=0 client_waiting=0
```

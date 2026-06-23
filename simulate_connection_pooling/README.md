# Simulate Connection Pooling Using Docker

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18 (`max_connections=20`)
- **Pooler**: PgBouncer 1.24 (transaction pooling mode)

## Overview

Every PostgreSQL connection costs a backend process and a chunk of memory, and
the server caps them with `max_connections`. Under high concurrency, apps either
exhaust those slots or pay the cost of constantly opening/closing connections.

A **connection pooler** like **PgBouncer** sits between the application and the
database and **multiplexes** many client connections onto a small, reused set of
server connections. This project starts PostgreSQL with only **20** connections
and shows how PgBouncer lets **100+ concurrent clients** share just **5** backend
connections.

### Key Concepts Demonstrated

1. **Connection exhaustion** — too many direct clients hit "too many clients
   already".
2. **Multiplexing** — PgBouncer serves many clients from a tiny server pool.
3. **Pool modes** — transaction vs session pooling and their trade-offs.
4. **Pool sizing** — `DEFAULT_POOL_SIZE` vs `MAX_CLIENT_CONN`.
5. **Observability** — `pg_stat_activity` (server side) and `SHOW POOLS`
   (PgBouncer side).

### Architecture

```
   many app clients                            small server pool
   ┌────────────────┐    ┌───────────────┐    ┌──────────────────────┐
   │ 100 goroutines │──► │   PgBouncer   │──► │ PostgreSQL           │
   │  (pooledURL)   │    │  :6432        │    │ :5440                │
   └────────────────┘    │ pool size = 5 │    │ max_connections = 20 │
                         └───────────────┘    └──────────────────────┘
   direct clients ───────────────────────────────────►  (bypass pooler)
   (directURL :5440)        exhaust the 20 slots
```

## Pool Modes

| Mode            | Server conn released after... | Throughput | Caveats                                                             |
| --------------- | ----------------------------- | ---------- | ------------------------------------------------------------------- |
| **session**     | the client disconnects        | Lowest     | Safe for everything (prepared stmts, `SET`, etc.)                   |
| **transaction** | each transaction commits      | High       | No session state across txns; avoid server-side prepared statements |
| **statement**   | each statement                | Highest    | No multi-statement transactions                                     |

This demo uses **transaction** mode (the most common choice), which is why the Go
client uses pgx's simple query protocol.

## How to Run

```bash
docker compose up -d      # start PostgreSQL + PgBouncer
go mod tidy
go run main.go
```

## Expected Output

Direct connections partially fail under load (connection exhaustion); the same
load through PgBouncer fully succeeds while the server never exceeds ~5 backends;
`SHOW POOLS` reports the live pool state.

## Cleanup

```bash
docker compose down -v
```

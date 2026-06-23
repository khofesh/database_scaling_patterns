# Simulate Read-Write Splitting Using Docker

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18 (1 primary + 2 streaming replicas)

## Overview

**Read-write splitting** routes every database operation by _type_: writes go to
the **primary**, reads go to the **replicas**. This is the practical application
of the read-replica topology — instead of the app manually choosing a connection
each time, a small **router** decides automatically.

The interesting part is consistency. Because replication is asynchronous, a naive
split can let a user write data and then _not see it_ on the next read. This
project implements **read-your-writes via LSN tracking**: after a write the router
remembers the WAL position and only serves the follow-up read from a replica that
has caught up — otherwise it falls back to the primary.

> Related project: [`simulate_read_replicas`](../simulate_read_replicas) sets up
> the same primary/replica topology and focuses on replication itself. This
> project focuses on the **routing layer** on top of it.

### Key Concepts Demonstrated

1. **Automatic routing** — `Write()` → primary, `Read()` → replicas.
2. **Load balancing** — reads spread round-robin across replicas.
3. **Read-your-writes** — LSN-based consistency after a write.
4. **Routing statistics** — how many ops went where.

### Architecture

```
                    ┌────────────────────────────────────┐
                    │            Router (Go)             │
   app calls ─────► │  Write()  ─────────────► primary   │
                    │  Read()   ──┐                       │
                    │  Consistent │  picks replica iff    │
                    │  Read(lsn)  │  replay_lsn >= lsn,    │
                    └─────────────┼──── else primary ─────┘
                                  ▼
        ┌──────────────┐   ┌──────────────┐   ┌──────────────┐
        │ Primary 5442 │──►│ Replica 5443 │   │ Replica 5444 │
        │  (writes)    │   │   (reads)    │   │   (reads)    │
        └──────────────┘   └──────────────┘   └──────────────┘
```

## How to Run

```bash
docker compose up -d      # primary + 2 replicas (replicas clone via pg_basebackup)
docker compose ps         # wait until all healthy
go mod tidy
go run main.go
```

## Expected Output

Writes are routed to the primary and reads to alternating replicas. A fresh write
followed by a consistent read is served from the **primary** (replicas not caught
up yet); a moment later the same read is served from a **replica**. Routing
counters are printed at the end.

## App-level Router vs Proxy

| Approach              | Examples            | Pros                       | Cons                                   |
| --------------------- | ------------------- | -------------------------- | -------------------------------------- |
| **App-level routing** | this project        | No extra hop; full control | Every app/language reimplements it     |
| **Proxy routing**     | ProxySQL, pgpool-II | Transparent to the app     | Extra component; another failure point |

## Cleanup

```bash
docker compose down -v
```

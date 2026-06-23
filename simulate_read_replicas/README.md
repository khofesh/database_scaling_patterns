# Simulate Read Replicas Using Docker

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18 (1 primary + 2 streaming replicas)

## Overview

This project demonstrates the **read replica** pattern: a single writable
**primary** streams its write-ahead log (WAL) to one or more read-only
**replicas**. Writes go to the primary; reads are spread across the replicas to
scale read throughput. Because replication is **asynchronous**, replicas can be
slightly behind — this project also demonstrates that lag and how to deal with
it.

### Key Concepts Demonstrated

1. **Streaming replication** — replicas clone the primary with `pg_basebackup`
   and then continuously replay WAL.
2. **Write routing** — all writes go to the primary; replicas reject writes.
3. **Read load balancing** — reads are distributed across replicas round-robin.
4. **Replication lag** — measuring how long a write takes to appear on a replica.
5. **Read-your-writes consistency** — reading from the primary when a user must
   immediately see their own change.
6. **Monitoring** — inspecting `pg_stat_replication` on the primary.

### Architecture

```
                    ┌──────────────────────┐
        writes ───► │   Primary (5436)     │
                    │   - accepts writes   │
                    └──────────┬───────────┘
                       streams WAL (async)
                  ┌────────────┴────────────┐
                  ▼                          ▼
        ┌──────────────────┐      ┌──────────────────┐
        │ Replica 1 (5437) │      │ Replica 2 (5438) │
        │  read-only       │      │  read-only       │
        └──────────────────┘      └──────────────────┘
                  ▲                          ▲
                  └──────── reads ───────────┘
                     (round-robin)
```

## How to Run

### 1. Start the cluster

```bash
docker compose up -d
```

The primary initialises first; each replica then waits for it, runs
`pg_basebackup`, and starts as a hot standby. Wait until all three are healthy:

```bash
docker compose ps
```

### 2. Download Go dependencies

```bash
go mod tidy
```

### 3. Run the application

```bash
go run main.go
```

## Expected Output

The application demonstrates writes hitting the primary, replicas rejecting
writes, load-balanced reads across replicas, observed replication lag, the
read-your-writes mitigation, and live replication status from
`pg_stat_replication`.

## Replicas vs Sharding

| Aspect            | Read Replicas               | Sharding              |
| ----------------- | --------------------------- | --------------------- |
| Scales            | Reads                       | Reads **and** writes  |
| Data on each node | Full copy                   | A subset              |
| Write capacity    | Single primary (bottleneck) | Many primaries        |
| Consistency       | Eventually consistent reads | Strong within a shard |
| Complexity        | Lower                       | Higher                |

## Cleanup

```bash
docker compose down -v
```

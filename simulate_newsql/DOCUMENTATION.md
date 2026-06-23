# NewSQL (CockroachDB) Simulation - Line-by-Line Documentation

This document explains the `simulate_newsql` project: **what** each piece does,
**why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure (docker-compose)](#infrastructure-docker-compose)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of NewSQL](#summary-pros-and-cons-of-newsql)

---

## Project Overview

Classic scaling forces a choice: a single PostgreSQL gives you SQL + ACID but doesn't
scale writes horizontally; manual sharding scales but throws away cross-shard
transactions, automatic replication, and easy failover (see
[`simulate_database_sharding`](../simulate_database_sharding)).

**NewSQL** refuses that trade-off. CockroachDB presents **one logical SQL database**
that is internally a distributed system:

- data is auto-split into **ranges**;
- each range is replicated to **3 nodes** and kept consistent with **Raft consensus**;
- transactions are **SERIALIZABLE** across the whole cluster;
- losing a node is a non-event — the remaining quorum keeps serving.

You write ordinary SQL; the database does the distribution.

---

## Infrastructure (`docker-compose.yml`)

Three CockroachDB nodes (`roach1/2/3`) plus a one-shot `init` container.

```yaml
command: start --insecure --join=roach1,roach2,roach3 --advertise-addr=roachN
```

- `--join` lists the peers so the nodes can discover each other and gossip.
- `--advertise-addr` is the address other nodes use to reach this one (the container
  hostname here).
- `--insecure` is for local demos only — never in production.

The **`init`** service runs `cockroach init --host=roach1` exactly once. This is what
bootstraps the cluster; **before it runs the nodes are up but refuse SQL**, which is
why the app retries the connection. `restart: on-failure` covers the race where it
starts before the nodes are listening.

`roach1` publishes `26257` (SQL, where the app connects) and `8090→8080` (the DB
Console UI). The healthcheck runs `cockroach sql -e "SELECT 1"`, which only passes
once the node is actually serving SQL — i.e. after bootstrap.

Replication factor 3 across 3 nodes means **quorum = 2**: the cluster tolerates losing
any one node with zero data loss.

---

## Application Code (`main.go`)

### Connecting (with retry)

```go
const dsn = "postgres://root@localhost:26257/defaultdb?sslmode=disable"
```

Standard PostgreSQL DSN — `pgx` doesn't know or care that the other end is
CockroachDB. `connectWithRetry` pings in a loop because the cluster needs a few
seconds to bootstrap; this makes `docker compose up` + `go run` work without manual
waiting.

### Schema — plain SQL

```sql
CREATE TABLE IF NOT EXISTS accounts (id STRING PRIMARY KEY, owner STRING, balance INT);
UPSERT INTO accounts VALUES ('alice', 'Alice Wong', 10000), ('bob', 'Bob Singh', 10000);
```

Nothing distributed-looking here — no shard key, no partition clause. `UPSERT` (a
CockroachDB built-in) makes the seed idempotent so the demo is repeatable.

### Showing the distribution — `showRanges`

This deliberately uses **only supported SQL**, not `crdb_internal`:

```sql
SELECT count(*), max(array_length(voting_replicas, 1))
FROM [SHOW RANGES FROM TABLE accounts WITH DETAILS];

SELECT DISTINCT unnest(voting_replicas) FROM [SHOW RANGES FROM TABLE accounts WITH DETAILS];
```

In CockroachDB v26 the `crdb_internal` and `system` tables are **restricted by
default** (`SQLSTATE 42501`); the hint suggests `allow_unsafe_internals = true`, which
is explicitly _not recommended_. So we read topology from the stable `SHOW RANGES …
WITH DETAILS` surface instead. The output proves the table is replicated across nodes
`[1 2 3]` although the application never chose a placement.

### Distributed ACID transaction — `transfer` + `retryTxn`

```go
func transfer(...) error {
    return retryTxn(ctx, pool, func(tx pgx.Tx) error {
        // SELECT balance; check funds; UPDATE from; UPDATE to
    })
}
```

The transfer is a single transaction. The two rows may live on different nodes; the
cluster still commits it atomically and serializably. There is **no two-phase commit
written in application code** — that's the database's job.

`retryTxn` is the important, correct-usage detail:

```go
var pgErr *pgconn.PgError
if errors.As(err, &pgErr) && pgErr.Code == "40001" { // serialization_failure
    continue // the DB asked us to retry
}
```

Under `SERIALIZABLE` isolation, the database may abort a transaction that would
violate serializability and return `40001`. The **client is expected to retry** — so
we loop. This is the canonical pattern for any serializable database and the reason
the concurrent test below never loses an update.

### Serializable isolation under contention — `concurrentTransfers`

Eight goroutines each transfer `$1` between the same two rows simultaneously. Without
serializable isolation + retry you'd get lost updates; here the final balances are
exactly correct because every conflict was detected and retried.

### Fault tolerance

The program prints instructions rather than killing a node for you (so it stays a
read-only demo of the cluster). Stopping `roach2` and re-running shows queries still
succeed: with 2 of 3 replicas alive there is still a Raft quorum, so reads and writes
continue with **no failover step** — contrast
[`simulate_automatic_failover`](../simulate_automatic_failover), where failover is an
explicit, hand-built process.

---

## Summary: Pros and Cons of NewSQL

**Pros**

- **SQL + ACID at scale** — serializable distributed transactions, no sharding logic.
- **Automatic sharding & rebalancing** — data placement is the database's problem.
- **Built-in HA** — Raft replication; survives node loss with no manual failover.
- **PostgreSQL-compatible** — existing drivers/tools (like `pgx`) just work.

**Cons**

- **Operational complexity** — a cluster to run, not a single process.
- **Latency floor** — consensus adds cross-node round-trips vs a local PostgreSQL.
- **Serializable retries** — applications _must_ handle `40001` (shown here).
- **Not a drop-in for every PostgreSQL feature** — some extensions/behaviours differ.

**When to use:** globally distributed or write-scaled OLTP that still needs strong
consistency and can't tolerate manual sharding/failover. **When not to:** a single
node handles your load — the extra moving parts and latency aren't worth it.

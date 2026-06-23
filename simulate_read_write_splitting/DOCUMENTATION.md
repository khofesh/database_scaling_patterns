# Read-Write Splitting Simulation - Line-by-Line Documentation

This document explains the `simulate_read_write_splitting` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Replication](#infrastructure--replication)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of Read-Write Splitting](#summary-pros-and-cons-of-read-write-splitting)

---

## Project Overview

Read-write splitting is the routing policy that sits on top of a primary/replica
cluster: classify each operation as a read or a write and send it to the right
node. Done well, it scales reads across replicas while keeping a single source of
truth for writes. Done naively, it exposes users to **replication lag** — they
write data and then read a replica that hasn't received it yet.

This project's `Router` solves both: automatic routing **and** read-your-writes
consistency using PostgreSQL's WAL log sequence numbers (LSNs).

---

## Infrastructure & Replication

The Docker setup (`docker-compose.yml`, `init/primary/*`,
`init/replica-entrypoint.sh`) is the same streaming-replication topology used in
[`simulate_read_replicas`](../simulate_read_replicas) — one primary plus two hot
standbys cloned with `pg_basebackup`. See that project's DOCUMENTATION.md for the
line-by-line replication walkthrough. The only differences here are the **ports**
(5442/5443/5444), the **container/volume names** (`rwsplit_*`), and the
**schema** (a `customers` table). This project's value is the routing layer in
`main.go`.

---

## Application Code (main.go)

### Router — the read/write splitter

```go
type Router struct {
    primary  *pgxpool.Pool
    replicas []*pgxpool.Pool
    next     uint64
    writes, readsFromReplica, readsFromPrimary atomic.Int64
}
```

`Router` is a tiny, explicit version of what ProxySQL/pgpool do for you. The
atomic counters let us report where traffic actually went.

### Write — always the primary, returns the commit LSN

```go
func (r *Router) Write(ctx, sql, args...) (string, error) {
    r.primary.Exec(ctx, sql, args...)
    r.writes.Add(1)
    var lsn string
    r.primary.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&lsn)
    return lsn, ...
}
```

| Line                   | What It Does                             | Why We Do It                                        |
| ---------------------- | ---------------------------------------- | --------------------------------------------------- |
| `r.primary.Exec`       | Runs the mutation on the primary         | Writes must be linearised on one node               |
| `pg_current_wal_lsn()` | Reads the primary's current WAL position | This LSN marks "the point your write is durable at" |
| return `lsn`           | Hands the LSN back to the caller         | Enables read-your-writes on the follow-up read      |

**Why return an LSN?** It's a precise, monotonic "version token" for the write.
Comparing replica progress against it is how we know whether a replica is fresh
enough to answer a given read.

### Read — round-robin across replicas

```go
func (r *Router) Read(ctx, sql, args...) (pgx.Rows, error) {
    r.readsFromReplica.Add(1)
    return r.pickReplica().Query(ctx, sql, args...)
}
```

The default read path. Use it for anything that tolerates a little staleness —
listings, search, dashboards. `pickReplica()` falls back to the primary if no
replicas are configured.

### ConsistentRead — read-your-writes via LSN

```go
for _, replica := range r.replicas {
    replica.QueryRow(ctx,
        "SELECT pg_last_wal_replay_lsn() >= $1::pg_lsn", afterLSN).Scan(&caughtUp)
    if caughtUp {
        return replica.Query(...), "replica", nil   // fresh enough
    }
}
// none caught up -> primary guarantees consistency
return r.primary.Query(...), "primary", nil
```

| Step                             | What It Does                                      | Why We Do It                                         |
| -------------------------------- | ------------------------------------------------- | ---------------------------------------------------- |
| `pg_last_wal_replay_lsn() >= $1` | Asks a replica "have you replayed past my write?" | Only a caught-up replica can serve a consistent read |
| use replica when `caughtUp`      | Serve the read from a replica                     | Keeps load off the primary when it's safe            |
| fall back to primary             | When no replica is fresh enough                   | The primary always has the latest data → never stale |

**Why this beats "always read from primary after a write":** it uses a replica
_as soon as it is safe to_, so the primary only absorbs the brief window during
which replicas are still catching up. This is the same idea databases like Aurora
and Spanner expose as "read-your-writes / bounded-staleness" reads.

### The demo

- `demonstrateRouting`: one INSERT (→ primary) and several SELECTs (→ alternating
  replicas, shown via `inet_server_port()`).
- `demonstrateConsistentRead`: insert a customer, capture the LSN, and do a
  `ConsistentRead`. Immediately it's served from the **primary** (replicas not yet
  caught up); after a 300 ms pause the same read is served from a **replica**.

---

## Summary: Pros and Cons of Read-Write Splitting

### Pros

| Benefit                 | Explanation                                         |
| ----------------------- | --------------------------------------------------- |
| **Read scalability**    | Reads fan out across replicas                       |
| **Primary protection**  | The write node isn't burdened with read traffic     |
| **Tunable consistency** | Choose per-read: fast (replica) or consistent (LSN) |
| **Transparent to code** | Callers say `Read`/`Write`, not which host          |

### Cons

| Drawback                  | Explanation                                                  |
| ------------------------- | ------------------------------------------------------------ |
| **Lag-induced staleness** | Plain reads can be behind; needs read-your-writes for safety |
| **Routing complexity**    | Classifying queries and tracking LSNs adds code              |
| **No write scaling**      | Still one primary — shard when writes saturate               |
| **Replica health**        | Router must handle a lagging/dead replica gracefully         |

### When to Use Read-Write Splitting

**Good fit:** read-heavy apps already running replicas that want to use them
without per-call host juggling. **Add LSN-based reads** for flows where a user
must see their own write. **Move to sharding** when the single primary's write
throughput becomes the limit.

## Key Takeaways

1. Route by operation type: writes → primary, reads → replicas.
2. Asynchronous lag means plain replica reads can be stale.
3. LSNs are precise version tokens — use them for read-your-writes.
4. Prefer a replica when it's caught up; fall back to the primary when it isn't.
5. Read-write splitting scales reads, not writes.

## go run output

```shell
$ go run main.go
✅ Router connected: 1 primary + 2 replicas

============================================================
↔️  READ-WRITE SPLITTING SIMULATION
============================================================

🧭 AUTOMATIC ROUTING (writes→primary, reads→replicas)
--------------------------------------------------
   INSERT customer → routed to PRIMARY
   SELECT customers → routed to REPLICA (7 rows)
   SELECT customers → routed to REPLICA (7 rows)
   SELECT customers → routed to REPLICA (7 rows)
   SELECT customers → routed to REPLICA (7 rows)

🪞 READ-YOUR-WRITES via LSN tracking
--------------------------------------------------
   Inserted new customer; commit LSN = 0/5000818
   Read-your-writes hit from primary: New Signup (standard) ✅ never stale
   After 300ms, same read served from: replica (replicas caught up)

📊 ROUTING STATISTICS
--------------------------------------------------
   Writes → primary:        2
   Reads  → replicas:       5
   Reads  → primary (RYW):  1
```

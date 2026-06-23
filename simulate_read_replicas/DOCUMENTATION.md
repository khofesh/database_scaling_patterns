# Read Replicas Simulation - Line-by-Line Documentation

This document explains the `simulate_read_replicas` project: **what** each piece
does, **why** it is built that way, and the **pros and cons** of the design.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure (docker-compose.yml)](#infrastructure-docker-composeyml)
3. [Primary Setup (init/primary)](#primary-setup-initprimary)
4. [Replica Bootstrap (replica-entrypoint.sh)](#replica-bootstrap-replica-entrypointsh)
5. [Application Code (main.go)](#application-code-maingo)
6. [Summary: Pros and Cons of Read Replicas](#summary-pros-and-cons-of-read-replicas)

---

## Project Overview

A **read replica** is a read-only copy of a database that stays in sync with a
writable **primary** via **streaming replication**. PostgreSQL ships the
primary's write-ahead log (WAL) to each replica, which replays it continuously.

### Why Read Replicas?

- **Read scaling**: most applications read far more than they write. Spreading
  reads across replicas multiplies read capacity.
- **High availability**: a replica can be promoted to primary if the primary
  fails.
- **Workload isolation**: heavy analytics/reporting can run on a replica without
  slowing down the primary's transactional traffic.

The key trade-off is **asynchronous replication**: replicas lag the primary by a
small, variable amount, so reads from a replica may be slightly stale.

---

## Infrastructure (docker-compose.yml)

### The Primary

```yaml
postgres_primary:
  image: postgres:18-bookworm
  environment:
    POSTGRES_USER: postgres
    POSTGRES_PASSWORD: postgres
    POSTGRES_DB: app_db
    PGDATA: /var/lib/postgresql/data
  ports:
    - "5436:5432"
  volumes:
    - primary_data:/var/lib/postgresql/data
    - ./init/primary:/docker-entrypoint-initdb.d
```

| Line                   | What It Does                          | Why We Do It                                             |
| ---------------------- | ------------------------------------- | -------------------------------------------------------- |
| `PGDATA: .../data`     | Pins the data directory               | We mount the volume and reference `$PGDATA` from scripts |
| `ports: "5436:5432"`   | Exposes the primary on host port 5436 | Distinct from sharding/partitioning ports                |
| `./init/primary` mount | Runs init scripts on first boot       | Creates the replication role and the schema              |

**Why is `wal_level` not set here?** Since PostgreSQL 10 the default `wal_level`
is `replica`, and `max_wal_senders`/`hot_standby` already default to
streaming-friendly values. So the only things we must add are a **replication
role** and a **pg_hba.conf rule** — no custom `postgresql.conf` needed.

### The Replicas

```yaml
postgres_replica1:
  image: postgres:18-bookworm
  environment:
    PRIMARY_HOST: postgres_primary
    REPLICATOR_PASSWORD: replicator
  entrypoint: ["/bin/bash", "/replica-entrypoint.sh"]
  depends_on:
    postgres_primary:
      condition: service_healthy
```

| Line                                   | What It Does                              | Why We Do It                                                |
| -------------------------------------- | ----------------------------------------- | ----------------------------------------------------------- |
| `entrypoint: replica-entrypoint.sh`    | Overrides the default postgres entrypoint | A replica is bootstrapped from the primary, not by `initdb` |
| `depends_on ... service_healthy`       | Waits for the primary's healthcheck       | `pg_basebackup` needs the primary accepting connections     |
| `PRIMARY_HOST` / `REPLICATOR_PASSWORD` | Passed into the entrypoint script         | Tells the replica where/how to clone                        |

**Pros:** each replica is independent and can be added by copying one service
block. **Cons:** ports are managed manually, and adding a replica needs a compose
edit (no auto-discovery).

---

## Primary Setup (init/primary)

### 01_setup_replication.sh

```bash
psql ... <<-EOSQL
    CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD 'replicator';
EOSQL

cat >> "$PGDATA/pg_hba.conf" <<-EOF
host replication replicator all scram-sha-256
EOF
```

| Step                          | Why We Do It                                                                                |
| ----------------------------- | ------------------------------------------------------------------------------------------- |
| `CREATE ROLE ... REPLICATION` | A streaming connection requires a role with the `REPLICATION` attribute                     |
| Append `host replication ...` | The default `pg_hba.conf` ships replication lines commented out; replicas would be rejected |
| `scram-sha-256`               | Matches PostgreSQL's default password encryption so `pg_basebackup` can authenticate        |

**Why a dedicated role instead of `postgres`?** Principle of least privilege —
the replication role can only stream WAL, not run arbitrary SQL.

### 02_create_schema.sql

The schema and seed rows are created **only on the primary**. Replicas receive
them automatically because the `CREATE TABLE`/`INSERT` records travel through the
WAL. Running DDL directly on a replica is impossible (it is read-only) and
unnecessary.

---

## Replica Bootstrap (replica-entrypoint.sh)

```bash
mkdir -p "$PGDATA"; chown -R postgres:postgres "$PGDATA"; chmod 0700 "$PGDATA"

until gosu postgres pg_isready -h "$PRIMARY_HOST" -U postgres -q; do
    sleep 2
done

if [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
    PGPASSWORD="$REPLICATOR_PASSWORD" gosu postgres pg_basebackup \
        -h "$PRIMARY_HOST" -U replicator -D "$PGDATA" -Fp -Xs -R -P
fi

exec gosu postgres postgres
```

| Line / Flag            | What It Does                                 | Why We Do It                                                    |
| ---------------------- | -------------------------------------------- | --------------------------------------------------------------- |
| `chown` + `chmod 0700` | Fixes ownership of the named volume          | The volume starts root-owned; postgres must own its data dir    |
| `pg_isready` loop      | Waits for the primary                        | `pg_basebackup` fails if the primary is not yet accepting conns |
| `ls -A` empty check    | Only clone on first boot                     | On restart we resume from existing data instead of re-cloning   |
| `pg_basebackup -Fp`    | Plain file format                            | Produces a ready-to-run data directory                          |
| `-Xs`                  | Streams WAL while backing up                 | Guarantees a consistent, immediately-startable copy             |
| `-R`                   | Writes `standby.signal` + `primary_conninfo` | Makes the copy boot as a streaming standby automatically        |
| `gosu postgres`        | Drops root to the postgres user              | Postgres refuses to run as root                                 |

**Why override the entrypoint at all?** The stock image entrypoint assumes
`initdb` for an empty directory. A replica must instead be a _physical copy_ of
the primary, so we replace that bootstrap step.

---

## Application Code (main.go)

### ReplicaSet — the routing layer

```go
type ReplicaSet struct {
    primary  *pgxpool.Pool
    replicas []*pgxpool.Pool
    next     uint64
}
```

| Field      | Purpose                                              |
| ---------- | ---------------------------------------------------- |
| `primary`  | The single writable pool                             |
| `replicas` | Read-only pools, one per replica                     |
| `next`     | Atomic counter driving round-robin replica selection |

This struct is the application's **read/write router**. Everything else asks it
for a `Writer()` or a `Reader()`.

### Writer() and Reader()

```go
func (rs *ReplicaSet) Writer() *pgxpool.Pool { return rs.primary }

func (rs *ReplicaSet) Reader() *pgxpool.Pool {
    if len(rs.replicas) == 0 {
        return rs.primary
    }
    i := atomic.AddUint64(&rs.next, 1)
    return rs.replicas[i%uint64(len(rs.replicas))]
}
```

| Method     | Returns                    | Why                                                              |
| ---------- | -------------------------- | ---------------------------------------------------------------- |
| `Writer()` | Always the primary         | All mutations must be linearised on one node                     |
| `Reader()` | Next replica (round-robin) | Balances read load; `atomic.AddUint64` is safe under concurrency |
| fallback   | Primary when no replicas   | The app keeps working even if replicas are removed               |

**Why round-robin and not random?** Deterministic, even distribution with zero
state beyond a counter. Production systems often add health checks and
latency-aware weighting on top.

### demonstrateWrites — replicas reject writes

The app writes to `rs.Writer()` (succeeds) and then attempts a write on a replica
(fails). The failure is expected: a hot standby is **read-only**, so this proves
the topology is correct.

### demonstrateBalancedReads

Each read calls `rs.Reader()` and runs `SELECT inet_server_port()`; alternating
ports confirm the two replicas are being used in turn.

### demonstrateReplicationLag

```go
rs.Writer().Exec(ctx, "INSERT ... VALUES ($1,...)", marker, ...)
replica.QueryRow(ctx, "SELECT COUNT(*) ... WHERE name=$1", marker).Scan(&found)
```

Writes a uniquely-named row to the primary, then immediately checks a replica.
Because replication is asynchronous the row is often **not yet visible**; the code
then polls until it appears and reports the observed lag.

| Concept             | Takeaway                                                        |
| ------------------- | --------------------------------------------------------------- |
| Asynchronous replay | A committed write is not instantly on replicas                  |
| Observed lag        | Usually milliseconds on a healthy LAN, but unbounded under load |

### demonstrateReadYourWrites

The mitigation for lag: after writing, read the row back from the **primary**
(`rs.Writer()`), guaranteeing the user sees their own change. Use this only for
"edit then immediately view" flows; route everything else to replicas.

Other strategies (not all shown, but worth knowing):

- **Sticky/primary reads for N seconds** after a user's write.
- **LSN tracking**: remember the commit LSN and only read from a replica whose
  `pg_last_wal_replay_lsn()` has caught up.
- **Synchronous replication** for the rows that truly need it (at a latency cost).

### showReplicationStatus

```sql
SELECT client_addr, state, sync_state,
       pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn) AS replay_lag_bytes
FROM pg_stat_replication;
```

`pg_stat_replication` lives on the **primary** and has one row per connected
replica. `pg_wal_lsn_diff` reports the byte distance between what the primary has
written and what each replica has replayed — the canonical lag metric to alert
on.

---

## Summary: Pros and Cons of Read Replicas

### Pros

| Benefit                | Explanation                                        |
| ---------------------- | -------------------------------------------------- |
| **Read scalability**   | Add replicas to serve more read traffic            |
| **High availability**  | A replica can be promoted on primary failure       |
| **Workload isolation** | Run reporting/analytics off the primary            |
| **Simple app model**   | Full copy of the data on every node; no resharding |

### Cons

| Drawback                | Explanation                                           |
| ----------------------- | ----------------------------------------------------- |
| **Stale reads**         | Asynchronous lag means replicas can be behind         |
| **No write scaling**    | The single primary remains the write bottleneck       |
| **Storage cost**        | Each replica stores a full copy of the data           |
| **Failover complexity** | Promotion, re-pointing apps, and avoiding split-brain |

### When to Use Read Replicas

**Good fit:** read-heavy workloads, reporting/analytics offload, and as a
stepping stone to HA. **Avoid / combine with sharding if:** writes are the
bottleneck, or your app cannot tolerate any stale reads anywhere.

## Key Takeaways

1. Writes go to the primary; reads scale across replicas.
2. Replication is asynchronous — design for stale reads.
3. Use read-your-writes (or LSN tracking) where consistency matters.
4. Monitor lag with `pg_stat_replication`.
5. Replicas scale reads, not writes — reach for sharding when writes saturate.

## go run output

```shell
$ go run main.go
✅ Connected to 1 primary and 2 replicas

============================================================
🔁 READ REPLICAS SIMULATION
============================================================

✍️  WRITES GO TO THE PRIMARY
--------------------------------------------------
   Primary accepted write (1 row inserted)
   Replica correctly rejected write: ERROR: cannot execute INSERT in a read-only transaction (SQLSTATE 25006)

📖 READS ARE LOAD-BALANCED ACROSS REPLICAS
--------------------------------------------------
   Read #1 → replica 2 (8 products)
   Read #2 → replica 1 (7 products)
   Read #3 → replica 2 (8 products)
   Read #4 → replica 1 (7 products)
   Read #5 → replica 2 (8 products)
   Read #6 → replica 1 (7 products)

⏱️  REPLICATION LAG
--------------------------------------------------
   Wrote "lag-probe-1782222920469181800" to primary
   Immediate replica read (after 6.338ms): row visible = false
   Row became visible on the replica after ~17ms (observed lag)

🪞 READ-YOUR-WRITES CONSISTENCY
--------------------------------------------------
   Inserted product id=10 on primary
   Read-your-writes via primary: price=$12.34 (always consistent)
   ↳ Use this for "edit then view" flows; use replicas for everything else.

📊 REPLICATION STATUS (pg_stat_replication)
--------------------------------------------------
   Replica 172.18.0.3/32: state=streaming sync=async replay_lag=288 bytes
   Replica 172.18.0.4/32: state=streaming sync=async replay_lag=288 bytes
```

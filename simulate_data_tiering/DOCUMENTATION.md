# Data Tiering Simulation - Line-by-Line Documentation

This document explains the `simulate_data_tiering` project: **what** each piece does,
**why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Schema](#infrastructure--schema)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of Data Tiering](#summary-pros-and-cons-of-data-tiering)

---

## Project Overview

Storing everything on premium storage is wasteful: most queries touch only recent
("hot") data, while old data is rarely read but must stay available. **Data tiering**
keeps the hot working set on fast storage and migrates aging data down to cheaper
warm/cold storage — automatically, by age — without changing how the data is queried.

PostgreSQL provides the two primitives needed:

- **Tablespaces** name physical storage locations. Map them to different media and
  you have storage tiers.
- **Range partitioning** splits the table by time so each month is a unit you can
  move between tiers and prune at query time.

---

## Infrastructure & Schema

### Creating tier directories — `init/00_tiers.sh`

`CREATE TABLESPACE` requires its target directory to exist and be owned by the
`postgres` OS user. The postgres entrypoint runs `/docker-entrypoint-initdb.d/*`
scripts **as the postgres user**, so a tiny shell script can create the directories
with the right ownership:

```sh
mkdir -p /var/lib/postgresql/tiers/{hot,warm,cold}
```

They live under `/var/lib/postgresql` (the mounted volume, so they persist) but
_outside_ PGDATA (which is a subdirectory). PostgreSQL refuses a tablespace located
inside the data directory, so this placement matters. The `00_` prefix ensures it
runs before the SQL.

### Tablespaces and table — `init/01_schema.sql`

```sql
CREATE TABLESPACE hot_tier  LOCATION '/var/lib/postgresql/tiers/hot';
CREATE TABLESPACE warm_tier LOCATION '/var/lib/postgresql/tiers/warm';
CREATE TABLESPACE cold_tier LOCATION '/var/lib/postgresql/tiers/cold';

CREATE TABLE events (
    id BIGSERIAL,
    created_at TIMESTAMPTZ NOT NULL,
    payload TEXT NOT NULL,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
```

This is **native declarative partitioning** (PostgreSQL 10+), not the old
inheritance-and-triggers approach. The partition key (`created_at`) must be part of
the primary key — a rule of partitioned tables — hence the composite `(id,
created_at)` key. The actual monthly partitions are created by the Go program so the
demo is always relative to "now".

---

## Application Code (`main.go`)

### The tiering policy — `tierFor`

```go
func tierFor(ageMonths int) string {
    switch {
    case ageMonths <= 0: return "hot_tier"   // current month
    case ageMonths <= 2: return "warm_tier"  // 1–2 months old
    default:             return "cold_tier"  // older
    }
}
```

A single, declarative mapping from age to tier. Everything else just applies it.

### Creating partitions — `ensurePartition`

```go
CREATE TABLE IF NOT EXISTS events_2026_06
    PARTITION OF events FOR VALUES FROM ('2026-06-01') TO ('2026-07-01')
    TABLESPACE hot_tier
```

New partitions are always created on the **hot** tier — new data is hot by
definition. `IF NOT EXISTS` makes the program idempotent.

### Migrating data — `runTiering`

It lists the partitions from the catalog (`pg_inherits` joined to `pg_class`),
computes each one's age, and moves it:

```go
ALTER TABLE events_2026_02 SET TABLESPACE cold_tier
```

`SET TABLESPACE` **physically relocates** the partition's files to the tier's
directory. Readers don't notice; the partition is still attached to `events`. (Note:
this rewrites the partition and takes a lock — fine for cold, aged data, which is the
point of doing it off the hot path.)

### Observing placement — `printPlacement`

```sql
SELECT child.relname, COALESCE(ts.spcname, 'pg_default')
FROM pg_inherits
JOIN pg_class parent ON parent.oid = pg_inherits.inhparent
JOIN pg_class child  ON child.oid  = pg_inherits.inhrelid
LEFT JOIN pg_tablespace ts ON ts.oid = child.reltablespace
WHERE parent.relname = 'events'
```

`reltablespace = 0` means "the database default", so the `LEFT JOIN` +
`COALESCE(..., 'pg_default')` reports that correctly. Per-partition row counts confirm
the data moved with the partition.

### Proving pruning — `explainRecent`

```sql
EXPLAIN SELECT count(*) FROM events WHERE created_at >= '<first of this month>'
```

The output shows the planner scanning **only the hot partition** — the warm and cold
partitions are eliminated before execution (**partition pruning**). The code marks the
hot partition's line with `→` so it's obvious. This is the performance payoff: hot
queries never pay for cold data.

### Transparent access

```go
db.QueryRow(ctx, "SELECT count(*) FROM events").Scan(&total)
```

A query against the parent table reads across all three tiers with no tier-specific
code. Tiering is invisible to the application — exactly the goal.

---

## Summary: Pros and Cons of Data Tiering

**Pros**

- **Lower cost** — cold data sits on cheap storage; only the hot set needs premium IO.
- **Sustained performance** — pruning keeps hot queries scanning only hot data.
- **Transparent** — one table; the application is tier-agnostic.
- **Simple migration** — one `ALTER TABLE … SET TABLESPACE` per aging partition.

**Cons**

- **More to manage** — multiple storage tiers and a tiering policy to maintain.
- **Migration cost** — `SET TABLESPACE` locks and rewrites the partition (do it on
  cold, aged data, not hot).
- **Slow cold reads** — querying archival data is intentionally slower.
- **Policy design** — partition granularity and age thresholds must fit the workload.

**When to use:** large time-series / append-mostly datasets with a recency-based
access pattern. **When not to:** small datasets, or workloads with no age/access
correlation — the storage savings won't justify the operational overhead.

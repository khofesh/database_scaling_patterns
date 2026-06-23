# Simulate Data Tiering Using Docker

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18 (three tablespaces as hot/warm/cold tiers)

## Overview

**Data tiering** keeps frequently-accessed **hot** data on fast, expensive storage
and migrates aging data down to cheaper **warm** and **cold** storage — automatically,
by age — while keeping everything queryable through a single table.

This project combines two native PostgreSQL features:

1. **Tablespaces** (`hot_tier`, `warm_tier`, `cold_tier`) — named storage locations.
   In production each maps to different physical media (NVMe → SSD → HDD/object
   store); here they are separate directories so the mechanics are real on one disk.
2. **Range partitioning** — `events` is split into one partition per month, so each
   time-slice is an independent unit we can relocate between tiers and **prune** at
   query time.

### Key Concepts Demonstrated

1. **Tier mapping by age** — current month → hot, 1–2 months → warm, older → cold.
2. **Automatic migration** — a tiering job moves whole partitions with
   `ALTER TABLE … SET TABLESPACE`, physically relocating the data.
3. **Partition pruning** — a "recent data" query scans only the hot partition; warm
   and cold are skipped entirely.
4. **Transparent access** — one parent table; the application never knows or cares
   which tier a row lives on.

> Related project: [`simulate_database_partitioning`](../simulate_database_partitioning)
> covers the partitioning mechanics themselves; this project adds the storage-tier
> dimension on top.

### Architecture

```
                        events  (partitioned by month)
        ┌───────────────┬───────────────┬───────────────┐
   events_2026_06   events_2026_05   …   events_2026_02
        │                │                    │
   ┌────▼────┐      ┌────▼────┐          ┌────▼────┐
   │ hot_tier│      │warm_tier│          │cold_tier│   ← ALTER TABLE SET TABLESPACE
   │ (NVMe)  │      │  (SSD)  │          │  (HDD)  │
   └─────────┘      └─────────┘          └─────────┘
```

## How to Run

```bash
docker compose up -d
docker compose ps        # wait until healthy
go mod tidy
go run main.go
```

The partition boundaries are computed relative to the current month, so the demo is
always current. It is idempotent — re-running re-seeds and re-tiers cleanly.

## Expected Output

Five monthly partitions are created on the hot tier and seeded. The tiering job then
demotes them by age (oldest → cold, mid → warm, current stays hot), and the new
placement is printed. An `EXPLAIN` of a recent-only query shows just the hot partition
is scanned. A final `count(*)` reads transparently across all three tiers.

## Data Tiering Trade-offs

| Aspect          | Benefit                                        | Cost                                     |
| --------------- | ---------------------------------------------- | ---------------------------------------- |
| **Cost**        | Cold data on cheap storage, hot on fast        | More storage tiers to provision/manage   |
| **Performance** | Hot working set stays fast; pruning skips rest | Cold-data queries are slower             |
| **Operations**  | Migration is one `ALTER TABLE` per partition   | `SET TABLESPACE` takes a lock + rewrites |
| **Simplicity**  | Queries stay tier-agnostic                     | Partition/tier policy must be maintained |

## When to Use

- Time-series / append-mostly data with a clear recency-based access pattern
  (logs, events, metrics, orders).
- Datasets large enough that storing everything on premium storage is wasteful.

## Notes on "not outdated"

- Native **declarative range partitioning** (not legacy inheritance triggers).
- `ALTER TABLE … SET TABLESPACE` for migration; placement read from `pg_inherits` /
  `pg_tablespace` (no deprecated catalogs).
- Go 1.26 idioms (`for i := range n`).

## Cleanup

```bash
docker compose down -v
```

# Event Sourcing Simulation - Line-by-Line Documentation

This document explains the `simulate_event_sourcing` project: **what** each piece
does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Schema](#infrastructure--schema)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of Event Sourcing](#summary-pros-and-cons-of-event-sourcing)

---

## Project Overview

Traditional CRUD stores **the current state**: a row says `balance = 12000` and an
update overwrites it. The previous value — and the reason it changed — is gone.

**Event sourcing** inverts this. The source of truth is an **append-only log of
immutable events**:

```
AccountOpened(owner=Alice)
Deposited(amount=10000)
Deposited(amount=5000)
Withdrew(amount=3000)
```

The balance (`12000`) is not stored as truth; it is **derived** by folding the log:
`state(n) = apply(state(n-1), event(n))`. Because the log is never mutated, you get a
complete audit trail and the ability to reconstruct state as of any point in time —
for free.

The two costs this project addresses directly:

- replay gets expensive as history grows → **snapshots**;
- concurrent writers could append conflicting events → **optimistic concurrency**
  via a per-aggregate version.

---

## Infrastructure & Schema

A single PostgreSQL 18 instance (port `5490`). Event sourcing is a *modeling*
pattern, not a distribution one, so no replicas or shards are needed.

`init/01_schema.sql` defines two tables:

```sql
events(global_seq PK, aggregate_id, version, event_type, payload JSONB, created_at,
       UNIQUE(aggregate_id, version))
snapshots(aggregate_id, version, state JSONB, created_at, PRIMARY KEY(aggregate_id, version))
```

- **`events`** is INSERT-only. It is never UPDATEd or DELETEd — that immutability is
  the whole point.
- **`UNIQUE (aggregate_id, version)`** is the load-bearing constraint. Each aggregate
  has exactly one event at version 1, one at version 2, and so on. Two writers that
  both try to write version `N` will collide here, giving us **lock-free optimistic
  concurrency control**.
- **`snapshots`** is derived data. It caches "the state of aggregate X as of version
  V" so a load doesn't have to replay from the beginning. Deleting every snapshot
  must never change a rebuild's *result*, only its *speed*.

---

## Application Code (`main.go`)

### Domain: state and the `apply` fold

`Account` is the **derived** state — it lives only in memory, rebuilt from events:

```go
type Account struct { ID, Owner string; Balance int; Open bool; Version int }
```

`apply` folds one event into the state. This is the core of event sourcing and is a
**pure function of the event** — same event, same state transition, every time:

```go
case "Deposited": a.Balance += int(payload["amount"].(float64))
case "Withdrew":  a.Balance -= int(payload["amount"].(float64))
...
a.Version++   // every applied event advances the version
```

### `load` — rebuild state from the log

```go
// 1. start from the newest snapshot (if any)
SELECT version, state FROM snapshots WHERE aggregate_id=$1 ORDER BY version DESC LIMIT 1
// 2. replay only the events committed AFTER that snapshot
SELECT version, event_type, payload FROM events
WHERE aggregate_id=$1 AND version > $2 ORDER BY version
```

With no snapshot, `fromVersion` is `0` and the full history is replayed. With a
snapshot at version 5, only versions 6+ are replayed. The function returns how many
events it actually replayed so the demo can *show* the snapshot saving work.

### `append` — write one event with concurrency control

```go
INSERT INTO events (aggregate_id, version, event_type, payload)
VALUES ($1, expectedVersion+1, $3, $4)
```

If another writer already committed `expectedVersion+1`, PostgreSQL raises a unique
violation (`SQLSTATE 23505`). We detect it with `errors.As` on `*pgconn.PgError` and
translate it into `ErrConcurrency`:

```go
var pgErr *pgconn.PgError
if errors.As(err, &pgErr) && pgErr.Code == "23505" { return ErrConcurrency }
```

The caller is expected to reload and retry — standard optimistic-concurrency flow.

### Commands: load → validate → append

Each command (`OpenAccount`, `Deposit`, `Withdraw`) follows the same shape: load the
current state, enforce business rules against it, then append a new event. The
withdraw rule is enforced against the **derived** balance:

```go
if acc.Balance < amount { return fmt.Errorf("insufficient funds: ...") }
```

This is important: business invariants are checked against the folded state, not
against any single stored row.

### The walkthrough in `main`

1. **Commands** — four events appended (open, deposit, deposit, withdraw).
2. **Rebuild** — `load` replays all four; prints the count and derived balance.
3. **Business rule** — an over-withdrawal is rejected against the derived state.
4. **Snapshot** — `snapshot` caches the fold; one more deposit is appended; the next
   `load` reports replaying only **1** post-snapshot event.
5. **Optimistic concurrency** — two readers load the same version; the first `append`
   wins, the second is rejected with `ErrConcurrency`.
6. **Audit log** — the full immutable history is printed in version order.

---

## Summary: Pros and Cons of Event Sourcing

**Pros**

- **Complete audit trail** — every change is an immutable, queryable fact.
- **Temporal queries** — reconstruct state at any past version/time.
- **Lock-free concurrency** — optimistic control via the version constraint.
- **Captures intent** — `Withdrew` carries more meaning than `balance changed`.
- **Natural fit for CQRS / projections** — read models rebuild from the log.

**Cons**

- **Storage grows unbounded** — mitigate with archival and snapshots.
- **Reads need replay** — mitigate with snapshots (shown here).
- **Event schema evolution** — old events must remain replayable forever (versioning,
  upcasting).
- **Eventual consistency** when projecting to read models.
- **Higher conceptual overhead** than CRUD — only worth it when history matters.

**When to use:** finance, healthcare, and other audit-heavy domains; systems needing
"what did it look like back then?"; the write side of [CQRS](../simulate_cqrs).
**When not to:** simple CRUD where only the latest state is ever needed.

# Simulate Event Sourcing Using Docker

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18

## Overview

**Event Sourcing** stores state as an **append-only log of immutable events** rather
than as mutable rows. You never overwrite "balance = 120"; you append the facts that
happened — `AccountOpened`, `Deposited`, `Withdrew` — and **derive** the current
state by replaying them.

This project models a bank account and demonstrates the four ideas that make event
sourcing work in practice:

1. **Append-only log** — every command produces an immutable event; nothing is ever
   UPDATEd or DELETEd.
2. **State reconstruction (replay)** — `state(n) = apply(state(n-1), event(n))`. The
   balance is a fold over the log.
3. **Snapshots** — a cached fold so loads don't replay the entire history; pure
   optimization, safe to delete.
4. **Optimistic concurrency** — a `UNIQUE (aggregate_id, version)` constraint makes
   two racing writers collide, and exactly one wins — no locks.

> Related project: [`simulate_cqrs`](../simulate_cqrs) often pairs with event
> sourcing — the event log here is the same idea as the CQRS outbox, and a CQRS
> projector could build read models from these events.

### Architecture

```
  COMMAND ──► load (snapshot + replay tail) ──► validate ──► append event
                                                                  │
                            ┌──────────── eventstore (5490) ──────▼─────┐
                            │ events    (append-only, UNIQUE(id,version))│
                            │ snapshots (cached fold, optimization only) │
                            └────────────────────────────────────────────┘
  QUERY  ──► load ──► replay events in version order ──► derived Account state
```

## How to Run

```bash
docker compose up -d
docker compose ps        # wait until healthy
go mod tidy
go run main.go
```

## Expected Output

Four events are appended, then the account state is rebuilt by replaying them. A
withdrawal that breaks a business rule is rejected against the derived balance. A
snapshot is taken so the next load replays only the events _after_ it. Finally two
writers read the same version and race — one commits, the other is rejected with a
concurrency error and must reload and retry. The full immutable audit log is printed.

## Event Sourcing Trade-offs

| Aspect           | Benefit                                       | Cost                                       |
| ---------------- | --------------------------------------------- | ------------------------------------------ |
| **Auditability** | Complete, immutable history of every change   | Storage grows forever (mitigate: archive)  |
| **Temporal**     | Reconstruct state at any past point in time   | Reads require replay (mitigate: snapshots) |
| **Concurrency**  | Lock-free optimistic control via version      | Writers must handle retry on conflict      |
| **Modeling**     | Business intent captured as first-class facts | Event schema versioning becomes a concern  |

## When to Use

- Audit/compliance domains (finance, healthcare) where _why_ state changed matters.
- Systems needing temporal queries ("what was the balance on March 1?").
- As the write side of [CQRS](../simulate_cqrs), feeding projected read models.

Avoid it for simple CRUD where the latest state is all anyone ever needs.

## Cleanup

```bash
docker compose down -v
```

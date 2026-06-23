# CQRS Simulation - Line-by-Line Documentation

This document explains the `simulate_cqrs` project: **what** each piece does,
**why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Schemas](#infrastructure--schemas)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of CQRS](#summary-pros-and-cons-of-cqrs)

---

## Project Overview

A single table design is always a compromise: normalize it and writes are clean
but reads need JOINs; denormalize it and reads are fast but every write has to
update redundant copies. **CQRS** stops compromising by using _two_ models:

- the **write model** stays normalized and transactional;
- the **read model** is denormalized for the exact queries the app makes;
- a **projector** keeps the read model up to date from write-side events.

Because projection is asynchronous, the read model lags slightly — the system is
**eventually consistent**, and the demo deliberately shows that gap.

---

## Infrastructure & Schemas

Two separate PostgreSQL instances make the separation physical:

**Write store** (`init/write/01_schema.sql`, port 5460) — normalized:

```sql
customers(id, name, email)
orders(id, customer_id → customers, status, created_at)
order_items(id, order_id → orders, product, qty, price_cents)
events(id, aggregate, payload JSONB, processed, created_at)   -- outbox
```

The `events` table is the **transactional outbox**. Every command appends an event
to it _inside the same transaction_ as the data change, so an event can never be
lost relative to a committed write.

**Read store** (`init/read/01_schema.sql`, port 5461) — denormalized:

```sql
order_summary(order_id PK, customer_name, customer_email,
              item_count, total_cents, status, placed_at)
```

One flat row per order: the customer is already joined in and the line items are
already summed. A query is a primary-key lookup.

---

## Application Code (main.go)

### Command — `PlaceOrder`

```go
tx := c.write.Begin(ctx)
tx.QueryRow("INSERT INTO orders ... RETURNING id").Scan(&orderID)
for _, it := range items { tx.Exec("INSERT INTO order_items ...") }
appendEvent(ctx, tx, "order_placed", orderID)   // SAME transaction
tx.Commit(ctx)
```

| Step                 | What It Does                              | Why We Do It                                  |
| -------------------- | ----------------------------------------- | --------------------------------------------- |
| `Begin` / `Commit`   | Wrap the whole command in one transaction | Order, items, and event commit atomically     |
| insert order + items | Write the normalized facts                | Source of truth lives here                    |
| `appendEvent`        | Enqueue an outbox row in the same TX      | Guarantees the projector will see this change |

**Why the outbox instead of writing the read model directly?** Writing to two
databases in one step has no shared transaction — a crash between them desyncs the
models. The outbox makes the write atomic and lets the projector apply it later,
retrying safely.

### Projection — `Project` and `projectOrder`

```go
rows := c.write.Query("SELECT id, payload FROM events WHERE NOT processed ORDER BY id")
// for each event:
c.projectOrder(ctx, orderID)                                   // rebuild read row
c.write.Exec("UPDATE events SET processed = TRUE WHERE id=$1") // mark done
```

`projectOrder` is where the denormalization happens:

```sql
SELECT cu.name, cu.email, o.status, o.created_at,
       COUNT(oi.id), SUM(oi.qty * oi.price_cents)
FROM orders o JOIN customers cu ... LEFT JOIN order_items oi ...
WHERE o.id = $1 GROUP BY ...
```

| Step                      | What It Does                                  | Why We Do It                                   |
| ------------------------- | --------------------------------------------- | ---------------------------------------------- |
| read unprocessed events   | Pull the backlog in order                     | Process changes once, in sequence              |
| JOIN + aggregate write DB | Compute the flat summary from normalized data | The read model is _derived_, not authoritative |
| `INSERT ... ON CONFLICT`  | Upsert the denormalized row in the read store | First projection inserts; later ones update    |
| mark `processed`          | Advance the outbox cursor                     | Idempotent, resumable, no double-apply         |

In production this loop runs continuously (or is driven by `LISTEN/NOTIFY` / a
log-based CDC stream). Here we call `Project()` explicitly so the consistency gap
is visible.

### Query — `GetOrderSummary`

```go
c.read.QueryRow("SELECT ... FROM order_summary WHERE order_id = $1")
```

A single-row read against the read store. No JOIN, no `SUM`, no touching the write
store. Returns `nil` (not an error) when the projector hasn't created the row yet —
that `nil` is the eventual-consistency window.

### The demo flow

1. `PlaceOrder` → write store + outbox.
2. `GetOrderSummary` → **nil** (read model not yet projected).
3. `Project` → drains the outbox, builds the summary row.
4. `GetOrderSummary` → the denormalized row.
5. `UpdateStatus("shipped")` → read model still shows the old status until
   `Project` runs again.

---

## Summary: Pros and Cons of CQRS

### Pros

| Benefit                      | Explanation                                              |
| ---------------------------- | -------------------------------------------------------- |
| **Independent optimization** | Normalize writes, denormalize reads — no compromise      |
| **Independent scaling**      | Add read replicas/stores without touching the write path |
| **Fast reads**               | Pre-joined, pre-aggregated single-row lookups            |
| **Reliable sync**            | Transactional outbox guarantees no lost updates          |

### Cons

| Drawback                 | Explanation                                                  |
| ------------------------ | ------------------------------------------------------------ |
| **Eventual consistency** | Reads lag writes until projection runs                       |
| **More moving parts**    | Projector, outbox, two schemas to operate and monitor        |
| **Duplicated data**      | Read model is a derived copy that must be rebuildable        |
| **Harder debugging**     | Tracing a value means following command → event → projection |

### When to Use CQRS

**Good fit:** read-heavy domains where query shapes differ a lot from the write
shape (dashboards, feeds, search-like views), or where read and write load must
scale on separate curves. Pairs naturally with **event sourcing**. **Avoid it**
for simple CRUD — the extra projector/outbox machinery isn't worth it when one
normalized schema already serves both sides well.

## Key Takeaways

1. Separate the write model (normalized) from the read model (denormalized).
2. Use a transactional outbox so events commit atomically with data.
3. A projector turns events into denormalized read rows.
4. Reads are eventually consistent — design the UX for the lag.
5. Don't reach for CQRS until plain CRUD genuinely stops scaling.

## go run output

```shell
$ go run main.go
✅ Connected: write store (5460) + read store (5461)

============================================================
🔀 CQRS SIMULATION
============================================================

✍️  COMMAND — place an order (write store + outbox)
--------------------------------------------------
   Order #1 written to normalized store; event queued in outbox

🔎 QUERY before projection — eventual consistency in action
--------------------------------------------------
   Read model has NO row for order #1 yet (projector hasn't run) ⏳

⚙️  PROJECTOR — drain outbox, rebuild read model
--------------------------------------------------
   Projected 1 event(s) into the read store

🔎 QUERY after projection — single-row, pre-joined read
--------------------------------------------------
   order #1 | Alice Wong | 2 items | $131.97 | placed

✍️  COMMAND — mark order shipped, then re-project
--------------------------------------------------
   status → 'shipped' (write store); read model still shows old status:
   order #1 | Alice Wong | 2 items | $131.97 | placed
   after projection:
   order #1 | Alice Wong | 2 items | $131.97 | shipped

📊 SUMMARY
--------------------------------------------------
   Writes: normalized, transactional, outbox-backed.
   Reads:  denormalized single-row lookups, eventually consistent.
```

# Denormalization Simulation - Line-by-Line Documentation

This document explains the `simulate_denormalization` project: **what** each piece
does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Schema & Triggers (init/01_schema.sql)](#schema--triggers-init01_schemasql)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

An order list that shows the customer's name, number of items, and total looks
innocent, but in a normalized schema it's a three-table join plus a `GROUP BY` for
every page view. Denormalization precomputes those values onto the order row. The
normalized tables remain the **source of truth**; the redundant columns are **derived**
and must be kept consistent.

---

## Schema & Triggers (init/01_schema.sql)

### Normalized model

`customers`, `products`, `orders`, `order_items` in third normal form. `order_items`
stores `unit_price` (the price at purchase time — itself a classic, *correct*
denormalization: historical prices must not change when the catalogue does).

### Redundant columns on `orders`

```sql
customer_name TEXT,                               -- copy of customers.name
item_count    INT            NOT NULL DEFAULT 0,  -- count(order_items)
total_amount  NUMERIC(12, 2) NOT NULL DEFAULT 0   -- sum(qty * unit_price)
```

Two kinds of redundancy: a **copied attribute** from a parent row, and **aggregates**
of child rows. Indexes on `(customer_id, created_at DESC)` and `(created_at DESC)`
support the hot queries.

### Seeding and backfill

Rows are generated with `generate_series`; customer 1 receives every 20th order (10,000
orders) to make the rename fan-out visible. The redundant columns are filled with two
set-based `UPDATE … FROM` statements *before* the triggers exist — backfilling through
row triggers would be far slower.

### Trigger 1 — `orders_customer_name` (BEFORE INSERT OR UPDATE OF customer_id)

Copies the customer's current name into the new order row. A `BEFORE` trigger can
modify `NEW` directly, so no second write is needed.

### Trigger 2 — `customers_name_fanout` (AFTER UPDATE OF name)

```sql
FOR EACH ROW WHEN (OLD.name IS DISTINCT FROM NEW.name)
... UPDATE orders SET customer_name = NEW.name WHERE customer_id = NEW.id;
```

The `WHEN` clause skips no-op updates. This is the **write amplification** of
denormalization made explicit: one logical change, N row rewrites, N× WAL, and row locks
on all of them.

### Trigger 3 — `order_items_recalc` (AFTER INSERT / UPDATE / DELETE, FOR EACH STATEMENT)

Statement-level triggers with **transition tables** (`REFERENCING NEW TABLE AS new_rows
OLD TABLE AS old_rows`) see every row affected by the statement. The function groups
them by `order_id` and issues **one** `UPDATE` with deltas — so inserting 1,000 items is
one grouped update, not 1,000 single-row ones.

- `INSERT` adds the new rows' counts and amounts.
- `DELETE` subtracts the old rows'.
- `UPDATE` subtracts the old versions and adds the new ones (a `UNION ALL` of +/-).

Transition tables are allowed only on single-event triggers, hence three triggers
sharing one function that branches on `TG_OP`. PL/pgSQL plans each statement lazily, so
the `DELETE` branch may reference `old_rows` even though the `INSERT` trigger doesn't
define it.

Deltas (`item_count + d.n`) rather than full recomputation keep the trigger cheap, at the
cost of compounding any earlier error — which is why reconciliation matters.

---

## Application Code (main.go)

### Read comparison — `compare` / `best`

Each query runs five times; the best time is reported.

- **Latest 50 orders of customer 1**: normalized must join and aggregate all 10,000 of
  the customer's orders' items before sorting; denormalized reads 50 rows from the
  `(customer_id, created_at DESC)` index. ~250x faster.
- **Top 10 customers by revenue (30 days)**: both scan recent orders, but denormalized
  skips the item join. ~6x faster.

### Consistency — `placeOrder` and follow-up edits

The application writes only normalized data inside `pgx.BeginFunc`. `showOrder` reads
back the redundant columns after an insert, a quantity update and a line deletion — each
time correct, without application code touching them.

### Write cost — `timeExec`

Renaming customer 2 (~100 orders) is quick; renaming customer 1 rewrites 10,000 orders
and takes over 100 ms. In a normalized design both are a single-row update.

### Drift — bypassing the sync logic

The demo disables `order_items_ins` and bulk-inserts an item into all of customer 7's
orders — standing in for an import job, a manual hotfix, or another service writing the
same tables. `driftQuery` compares every order's copies with values recomputed from the
source tables (`IS DISTINCT FROM` handles NULLs) and finds the corrupted orders.

### Reconciliation

A set-based `UPDATE … FROM (aggregate) … WHERE values differ` recomputes only mismatched
rows. Run on a schedule, this bounds how long drift can survive.

### App-level alternative — `appLevelAddItem`

With the trigger off, the application inserts the item and updates `item_count` /
`total_amount` in the same transaction. Correct and explicit — but only as long as
*every* write path does it. `NUMERIC` prices are carried as text and cast in SQL to
avoid float rounding.

### `reset`

Removes demo rows and restores customer names, so the program is re-runnable.

---

## Summary: Pros and Cons

**Pros**

- **Much faster reads** — no joins/aggregates on hot paths.
- **Simpler queries** — the read model matches what the UI shows.
- **Predictable read cost** — independent of child-row counts.

**Cons**

- **Write amplification** — each change updates every copy (fan-out).
- **Consistency risk** — any path that bypasses the sync logic causes drift.
- **More storage** and more complex write logic.
- **Lock contention** — popular parent rows (a celebrity's name) touch many rows.

**When to use:** read-heavy data that changes rarely, after indexing alone isn't enough.
**When not to:** frequently changing attributes or unbounded fan-out.

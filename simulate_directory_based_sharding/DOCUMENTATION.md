# Directory-Based Sharding Simulation - Line-by-Line Documentation

This document explains the `simulate_directory_based_sharding` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Schemas](#schemas)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

Every sharded system needs a function `shard_for(key)`. Hash and range sharding make it
a computation; directory sharding makes it a **table lookup**. The lookup costs a round
trip (so it's cached), but it buys complete freedom over placement — the property that
matters most when data is skewed, as tenant sizes almost always are.

---

## Schemas

### Directory — `init/directory/01_schema.sql`

```sql
CREATE TABLE shards (name TEXT PRIMARY KEY, dsn TEXT NOT NULL, status TEXT ... 'open' | 'closed');
CREATE TABLE tenant_directory (
    tenant_id TEXT PRIMARY KEY,
    shard     TEXT NOT NULL REFERENCES shards (name),
    status    TEXT NOT NULL DEFAULT 'active',   -- 'active' | 'moving'
    version   INT  NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

- **`shards`** is a registry. The DSN lives here so the application discovers new shards
  at runtime. `status = 'closed'` keeps a shard from receiving new tenants (used for a
  dedicated shard).
- **`tenant_directory`** is the routing table. `status = 'moving'` marks an in-progress
  migration; `version` increments on every move so caches can be compared.
- The foreign key guarantees no tenant points at an unregistered shard.

(The DSNs use `localhost:551x` because the Go program runs on the host.)

### Shards — `init/shard/01_schema.sql`

```sql
CREATE TABLE hosted_tenants (tenant_id TEXT PRIMARY KEY);
CREATE TABLE orders (tenant_id TEXT, id BIGINT, amount NUMERIC, created_at TIMESTAMPTZ,
                     PRIMARY KEY (tenant_id, id));
```

`hosted_tenants` is the shard's own record of which tenants it is **authoritative**
for. It's the fence that makes stale directory caches safe (see below). `orders` is
keyed by `(tenant_id, id)` so tenant-scoped queries use the primary key index.

---

## Application Code (main.go)

### `Router`

Each application instance holds a directory pool, a lazily opened pool per shard, and a
`cache map[string]entry`. Two routers (`app-1`, `app-2`) are created to show what
happens when one has a stale cache.

### `lookup`

Cache hit → return. Miss → `SELECT shard, version, status FROM tenant_directory`. A
mapping whose status is `moving` is **not** cached, so a migration in progress never
gets frozen into a cache. The demo's 300 reads across 6 tenants need 6 lookups.

### `Onboard` — dynamic assignment

Lists open shards, counts rows on each, and places the tenant on the smallest, then
inserts into `hosted_tenants` on that shard and into `tenant_directory`. The policy is
arbitrary code: it could equally use CPU, region or pricing tier — impossible with a
hash function. After `shard3` becomes a dedicated (closed) shard, new tenants `wayne`
and `stark` land on `shard1`, the lighter of the remaining open shards.

### `InsertOrders` — fenced writes

```sql
INSERT INTO orders (tenant_id, id, amount)
SELECT $1, ... FROM generate_series(1, $2) AS g
WHERE EXISTS (SELECT 1 FROM hosted_tenants WHERE tenant_id = $1 FOR SHARE)
```

- If the shard doesn't host the tenant, zero rows are inserted. The router treats that
  as "my mapping is stale", drops the cache entry and retries once with a fresh lookup.
- `FOR SHARE` locks the `hosted_tenants` row. A mover deleting that row must wait for
  in-flight writers to finish, and writers arriving after the delete see no row. That
  closes the race where a write lands on the source after its rows were copied.

### `MoveTenant` — rebalancing one tenant

1. **Directory:** `UPDATE … SET status = 'moving' … RETURNING shard` (also guards against
   two concurrent moves of the same tenant).
2. **Source:** open a transaction and `DELETE FROM hosted_tenants` — the fence goes up.
3. **Read** the tenant's rows inside that transaction.
4. **Target:** in one transaction, `CopyFrom` the rows (PostgreSQL COPY protocol — much
   faster than INSERTs) and insert into `hosted_tenants`; commit.
5. **Source:** delete the tenant's rows; commit.
6. **Directory:** set the new shard, `version + 1`, `status = 'active'`.

Only the moving tenant's rows travel; every other tenant is untouched. If step 5 failed
after step 4 committed, the target would hold a copy while the source remains
authoritative — a retry would need to clear the target first. Production tools make
this resumable and stream changes during the copy (logical replication) to keep the
write pause short.

### Stale cache demo

`app-2` caches `megacorp → shard1` before the move. Afterwards its write to `shard1` is
rejected by the fence, it refreshes, and writes to `shard3` — no lost or misplaced data,
just one extra round trip.

### `compareWithHash`

Computes `fnv32a(tenant) % 2` vs `% 3` for the same tenants. With modulo hashing about
half the tenants (in general `1 − 1/N`-ish) change shards when one is added. The
directory moved nobody automatically, and exactly one tenant by choice.

### `reset`

Truncates all shard tables and the directory and unregisters `shard3`, so each run starts
from scratch.

---

## Summary: Pros and Cons

**Pros**

- **Arbitrary placement** — by load, size, region, compliance or price tier.
- **Granular rebalancing** — move one tenant at a time; isolate hotspots.
- **Painless growth** — new shards take new tenants; nobody is rehashed.

**Cons**

- **Extra hop** — every cache miss costs a directory query.
- **Critical dependency** — if the directory is down and caches are cold, nothing can be
  routed; it must be replicated and highly available.
- **Cache coherence** — moved tenants make caches stale; shards must fence writes.
- **Migration engineering** — copy, fence, flip and clean up must be crash-safe.

**When to use:** skewed multi-tenant data, placement rules, frequent rebalancing.
**When not to:** uniform keys where a hash ring is simpler and needs no lookup service.

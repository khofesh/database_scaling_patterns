# Consistent Hashing Simulation - Line-by-Line Documentation

This document explains the `simulate_consistent_hashing` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure](#infrastructure)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of Consistent Hashing](#summary-pros-and-cons-of-consistent-hashing)

---

## Project Overview

When you shard data with `node = hash(key) % N`, the node count `N` is baked into
every key's location. Change `N` (add or remove a node) and almost every key now
maps somewhere else — a near-total reshuffle that, on a real cluster, means moving
terabytes and invalidating every cache.

**Consistent hashing** removes `N` from the formula. Nodes and keys are hashed
onto a fixed ring; a key belongs to the next node clockwise. Adding a node only
steals the keys in the arc between it and its predecessor. That's about `1/N` of
the data instead of `(N-1)/N`.

---

## Infrastructure

`docker-compose.yml` starts **four identical PostgreSQL nodes** (ports
5450–5453), each with the same one-table schema from `init/01_create_tables.sql`. The demo
`TRUNCATE`s the `kv` table before writing so re-runs reflect the current placement
rather than rows accumulated from earlier runs:

```sql
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

The nodes are plain and independent — there is no replication here. Consistent
hashing lives entirely in the **application**: it decides which node each key goes
to. All four containers run from the start; the demo only _adds node4 to the ring_
partway through to measure rebalancing.

---

## Application Code (main.go)

### The ring

```go
type HashRing struct {
    replicas int               // vnodes per physical node
    ring     []uint32          // sorted hashes of every vnode
    owners   map[uint32]string // vnode hash -> physical node
    nodes    map[string]bool
}
```

`ring` is a **sorted slice of vnode hashes**. Keeping it sorted lets us binary
search for the owning node. `owners` maps each vnode point back to its physical
node.

### Adding a node — placing its virtual nodes

```go
for i := 0; i < h.replicas; i++ {
    hash := hashKey(vnodeKey(node, i))   // e.g. crc32("node4#37")
    h.ring = append(h.ring, hash)
    h.owners[hash] = node
}
sort.Slice(h.ring, ...)
```

| Line               | What It Does                                  | Why We Do It                                         |
| ------------------ | --------------------------------------------- | ---------------------------------------------------- |
| `vnodeKey(...)`    | Builds `node#i` so each vnode hashes uniquely | Spreads one physical node across many ring positions |
| `crc32` the vnode  | Picks a point on the 0..2³²-1 ring            | Pseudo-random placement → even coverage              |
| keep `ring` sorted | Maintains search invariant                    | Lookups use binary search                            |

**Why 1000 vnodes?** With one point per node the ring is lumpy — one node can own a
huge arc by luck. Many vnodes per node average out to near-equal shares (the law of
large numbers) and make rebalancing spread across _all_ remaining nodes. (Too few
vnodes here, e.g. 150, leaves a visible imbalance like 41/26/33 even though the
_movement_ on resize stays ~1/N.)

### Looking up a key — walk clockwise

```go
hash := hashKey(key)
idx := sort.Search(len(h.ring), func(i int) bool { return h.ring[i] >= hash })
if idx == len(h.ring) {
    idx = 0 // wrap around
}
return h.owners[h.ring[idx]]
```

| Step                  | What It Does                                | Why We Do It                        |
| --------------------- | ------------------------------------------- | ----------------------------------- |
| `hashKey(key)`        | Places the key on the same ring             | Keys and nodes must share one space |
| `sort.Search(... >=)` | First vnode at or after the key (clockwise) | That vnode's node owns the key      |
| wrap to `0`           | If past the last point, wrap to the first   | The ring is circular                |

`sort.Search` is O(log V) where V is the total vnode count — fast even with
thousands of points.

### Removing a node

```go
for _, hash := range h.ring {
    if h.owners[hash] == node { delete(h.owners, hash); continue }
    kept = append(kept, hash)
}
h.ring = kept
```

Drops every vnode owned by that node. The keys it held flow to whichever nodes now
sit clockwise of those gaps — again, only a `1/N` slice is disturbed.

### The demo

1. **Initial placement** — 1000 keys across 3 nodes; a bar chart shows the split.
2. **Persist** — each key is `INSERT`ed into its owning PostgreSQL node, then we
   `SELECT COUNT(*)` per node to prove the in-memory ring matches stored data.
3. **Add node4** — recompute placement and count how many keys changed owner.
4. **Modulo comparison** — `moduloPlacement(keys, 3)` vs `moduloPlacement(keys, 4)`
   shows `key % N` re-homing the large majority of keys.

`countMoved` simply compares the before/after owner maps key by key.

---

## Summary: Pros and Cons of Consistent Hashing

### Pros

| Benefit                 | Explanation                                                 |
| ----------------------- | ----------------------------------------------------------- |
| **Minimal reshuffling** | Adding/removing a node moves ~1/N of keys, not (N-1)/N      |
| **Even load**           | Virtual nodes smooth out per-node imbalance                 |
| **Incremental scaling** | Grow the cluster one node at a time, cheaply                |
| **Graceful node loss**  | A dead node's keys spread to neighbors, not all to one node |

### Cons

| Drawback                     | Explanation                                               |
| ---------------------------- | --------------------------------------------------------- |
| **More complex**             | Ring + vnodes vs a one-line modulo                        |
| **Still need data movement** | Re-homed keys must physically migrate between nodes       |
| **Range queries**            | Keys are scattered by hash; no locality for range scans   |
| **Vnode bookkeeping**        | Too few vnodes → imbalance; too many → memory/lookup cost |

### When to Use Consistent Hashing

**Good fit:** distributed caches and key-value stores that scale elastically
(Cassandra, DynamoDB, Riak, memcached client rings) where nodes come and go and
you cannot afford to move the whole dataset on each change. **Skip it** when the
shard count is small and fixed, or when you need range locality — there a directory
or range-based scheme fits better.

## Key Takeaways

1. Hash nodes and keys onto one ring; own by the next node clockwise.
2. Virtual nodes are the trick that makes the distribution even.
3. Resizing moves ~1/N of keys vs almost everything for `key % N`.
4. It optimizes _movement_, not _locality_ — range queries still scatter.
5. This is the backbone of elastic, shared-nothing data stores.

## go run output

```shell
$ go run main.go
✅ Connected to 4 PostgreSQL nodes

============================================================
🔗 CONSISTENT HASHING SIMULATION
============================================================

📦 INITIAL PLACEMENT — 3 nodes, 1000 vnodes each
--------------------------------------------------
   node1   360 keys  ██████████████ 36.0%
   node2   326 keys  █████████████ 32.6%
   node3   314 keys  ████████████ 31.4%

💾 WRITING KEYS TO THEIR OWNING NODES
--------------------------------------------------
   node1  stored 360 rows
   node2  stored 326 rows
   node3  stored 314 rows

➕ ADDING node4 — consistent hashing
--------------------------------------------------
   node1   312 keys  ████████████ 31.2%
   node2   276 keys  ███████████ 27.6%
   node3   232 keys  █████████ 23.2%
   node4   180 keys  ███████ 18.0%

   Keys re-homed: 180 / 1000  (18.0%)
   Ideal for 3→4 nodes: ~25.0% (1/4 of keys)

⚖️  COMPARISON — modulo sharding (key % N)
--------------------------------------------------
   Modulo 3→4 re-homed: 752 / 1000  (75.2%)
   Consistent hash:     180 / 1000  (18.0%)
   → modulo moves most keys; consistent hashing moves ~1/N.

📊 SUMMARY
--------------------------------------------------
   Virtual nodes smooth out the per-node load imbalance.
   Adding/removing a node only disturbs neighboring arcs of the ring.
```

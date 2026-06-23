# Simulate Consistent Hashing Using Docker

## Tech Stack

- **Programming Language**: Go 1.26.0
- **Container**: Docker Compose
- **Database**: PostgreSQL 18 (4 independent nodes)

## Overview

**Consistent hashing** maps both _nodes_ and _keys_ onto the same circular hash
space (a "ring"). A key is owned by the first node found walking clockwise from
the key's position. The payoff: when you add or remove a node, only the keys in
the affected arc move — roughly **1/N of the keyset** — instead of nearly all of
them, which is what naive `key % N` sharding forces.

Each physical node is placed at many points on the ring as **virtual nodes
(vnodes)**. More vnodes → smoother, more even distribution and gentler rebalancing.

> Related project: [`simulate_database_sharding`](../simulate_database_sharding)
> uses `hash % N` routing. This project shows why that approach is painful to
> scale and how consistent hashing fixes it.

### Key Concepts Demonstrated

1. **The hash ring** — nodes and keys share one 32-bit CRC space.
2. **Virtual nodes** — 1000 vnodes per node for even load.
3. **Minimal data movement** — adding node4 re-homes only ~1/4 of keys.
4. **Modulo comparison** — `key % N` reshuffles most keys on resize.
5. **Real persistence** — each key is stored on its owning PostgreSQL node.

### Architecture

```
                 hash ring (0 .. 2^32-1)
                ┌───────────────────────┐
        key ───►│  •n2  •n1   •n3  •n1   │  key owned by first
                │ •n3        •n2    •n1  │  vnode clockwise
                │   •n1  •n3    •n2  •n3 │
                └───────────────────────┘
                  1000 vnodes per node

   node1:5450   node2:5451   node3:5452   node4:5453 (added mid-demo)
   each is a plain PostgreSQL holding the keys that hash to it
```

## How to Run

```bash
docker compose up -d      # 4 PostgreSQL nodes
docker compose ps         # wait until all healthy
go mod tidy
go run main.go
```

## Expected Output

Keys distribute fairly evenly across 3 nodes. Adding node4 re-homes about
**25%** of keys (consistent hashing), versus **~75%** for `key % N` modulo
sharding — printed side by side.

## Consistent Hashing vs Modulo Sharding

| Aspect             | Consistent hashing        | `key % N` modulo           |
| ------------------ | ------------------------- | -------------------------- |
| **Resize cost**    | ~1/N keys move            | Almost all keys move       |
| **Load evenness**  | Even with enough vnodes   | Even while N is fixed      |
| **Implementation** | Ring + vnodes (more code) | One line                   |
| **Used by**        | Cassandra, DynamoDB, Riak | Simple/static shard counts |

## Cleanup

```bash
docker compose down -v
```

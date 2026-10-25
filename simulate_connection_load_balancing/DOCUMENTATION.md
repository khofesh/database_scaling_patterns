# Connection Load Balancing Simulation - Line-by-Line Documentation

This document explains the `simulate_connection_load_balancing` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure](#infrastructure)
3. [HAProxy Configuration (haproxy/haproxy.cfg)](#haproxy-configuration-haproxyhaproxycfg)
4. [Application Code (main.go)](#application-code-maingo)
5. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

[`simulate_read_write_splitting`](../simulate_read_write_splitting) routes queries in
the application, which therefore has to know every server's address. A load balancer
moves that knowledge into the network: the application connects to a single address per
role and the balancer handles distribution and failure detection.

---

## Infrastructure

### docker-compose.yml

- `pg_primary` — started with `cluster_name=primary`.
- `pg_replica1`, `pg_replica2` — streaming replicas cloned with `pg_basebackup`
  (`init/replica-entrypoint.sh`), each with `NODE_NAME` → `cluster_name`.
- `haproxy` — the only service with published PostgreSQL ports: `5506` (write), `5507`
  (read) and `8404` (stats). The database containers publish nothing; clients cannot
  bypass the balancer.

`current_setting('cluster_name')` lets the demo ask any backend "who are you?", which is
how it shows where HAProxy routed each connection.

---

## HAProxy Configuration (haproxy/haproxy.cfg)

### defaults

```
mode tcp
timeout client 30m
timeout server 30m
default-server inter 1s fall 2 rise 2 resolvers docker init-addr last,libc,none
```

- **`mode tcp`** — HAProxy forwards bytes; it never parses the PostgreSQL protocol
  beyond health checks. Hence: it balances **connections**, not queries.
- **Long timeouts** — pooled database connections sit idle for long periods; short
  timeouts would cut them.
- **`inter 1s fall 2 rise 2`** — check every second; 2 consecutive failures → DOWN, 2
  successes → UP. Detection time ≈ `inter × fall`.
- **`resolvers docker`** + **`init-addr`** — resolve container names through Docker's
  DNS at runtime and let HAProxy start even if a backend isn't resolvable yet.

### pg_write

```
listen pg_write
    bind :5000
    option pgsql-check user postgres
    default-server on-marked-down shutdown-sessions
    server primary pg_primary:5432 check
```

`pgsql-check` sends a PostgreSQL startup message and requires an authentication request
in reply — stronger than "TCP port open" (a hung postmaster wouldn't pass), but it does
**not** check whether the node is the primary. `on-marked-down shutdown-sessions` closes
client sessions to a server as soon as it's marked DOWN, so clients fail fast and
reconnect instead of hanging on a dead TCP connection.

### pg_read

```
balance roundrobin
server replica1 pg_replica1:5432 check
server replica2 pg_replica2:5432 check
server primary  pg_primary:5432  check backup
```

Round-robin alternates new connections. `backup` means the primary receives read
connections only when **no** non-backup server is UP — reads stay available even when
every replica is down, at the cost of extra primary load.

### stats

An HTTP listener on `:8404` serving the dashboard at `/stats` and machine-readable CSV
at `/stats;csv`. The Go program reads the CSV to show server states.

---

## Application Code (main.go)

### `whoAmI`

Opens a fresh connection to an endpoint and runs
`SELECT current_setting('cluster_name'), pg_is_in_recovery()`. The write endpoint
answers `primary (read-write)`; the read endpoint answers a replica.

### `distribute`

Opens *n* separate connections to the read port and tallies which backend served each.
With two healthy replicas, round-robin gives an even split.

### `pinnedPool`

A `pgxpool` with `MinConns = MaxConns = 2` opens two connections at startup — one per
replica, thanks to round-robin. Four goroutines then run 20 queries; each query reports
`pg_backend_pid()`, and all 20 land on those same two backend processes. This is the
key operational fact: **pools pin traffic**. Adding a replica does nothing for an
existing pool until its connections are recycled — set `MaxConnLifetime` to bound that.

### Failure handling

`dockerCmd("stop", "lb_replica1")` stops a replica. `waitServers` polls the stats CSV
until HAProxy reports it DOWN (≈ 1–2 s). New connections then go only to `replica2`.
Stopping `replica2` too sends reads to the `backup` primary. After `docker start`, two
successful checks (`rise 2`) bring both replicas back.

### `serverStatus`

Parses `/stats;csv` with `encoding/csv`, locating the `status` column by header name
(rather than a magic index), skipping the `FRONTEND`/`BACKEND` aggregate rows, and
trimming transition suffixes such as `DOWN 1/2`.

---

## Summary: Pros and Cons

**Pros**

- **Single address per role** — servers can be added, removed or replaced without client
  changes.
- **Automatic removal of failed backends** — health checks every second.
- **Mature and fast** — HAProxy adds negligible latency at TCP level.

**Cons**

- **Connection-level only** — no per-query routing; pools stay pinned until reconnect.
- **Not role-aware by itself** — after a failover, `pgsql-check` would happily send
  writes to a demoted node. Use Patroni's REST API (`option httpchk GET /primary`).
- **Another component to run highly available** — two balancers + VIP or cloud LB.
- **Detection delay** — in-flight sessions to a failed node are lost.

**When to use:** any deployment with replicas or HA failover where clients shouldn't
track topology. **When not to:** a single database server, or when a client-side
multi-host connection string (`target_session_attrs`) is enough.

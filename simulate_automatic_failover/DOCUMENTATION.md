# Automatic Failover Simulation - Line-by-Line Documentation

This document explains the `simulate_automatic_failover` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure & Replication](#infrastructure--replication)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons of Automatic Failover](#summary-pros-and-cons-of-automatic-failover)

---

## Project Overview

A single primary is a single point of failure: when it dies, writes stop. **High
availability** adds a standby that already has a near-current copy of the data
(via streaming replication) and a **failover mechanism** that detects the primary's
death and promotes the standby to take over.

The two hard parts are **detection** (is the primary really dead, or just slow?)
and **promotion** (make the standby writable, then send traffic to it). This
project implements both as an explicit control loop so the logic is visible.

---

## Infrastructure & Replication

The Docker topology is the same streaming-replication setup used in
[`simulate_read_replicas`](../simulate_read_replicas) and
[`simulate_read_write_splitting`](../simulate_read_write_splitting): one **primary**
(port 5470) and one **hot standby** (port 5471) cloned with `pg_basebackup -R`. See
those projects' DOCUMENTATION.md for the line-by-line replication walkthrough. The
only differences are the **ports** and the **container/volume names** (`failover_*`).

What matters here: the standby is a _hot_ standby — it replays WAL continuously and
serves read-only queries — and it can be **promoted** to a full primary on demand.

---

## Application Code (main.go)

### FailoverManager — the control loop

```go
type FailoverManager struct {
    current    *pgxpool.Pool  // whoever is the writable primary right now
    standby    *pgxpool.Pool  // promoted on failover
    failThresh int            // consecutive failures before failover
    probeEvery, probeTimout time.Duration
}
```

`current` always points at the node we believe is the writable primary. After a
successful promotion it is swapped to point at the (former) standby.

### Detection — `healthy` + `WatchAndFailover`

```go
func (m *FailoverManager) healthy(ctx) bool {
    cctx, cancel := context.WithTimeout(ctx, m.probeTimout)
    defer cancel()
    return m.current.QueryRow(cctx, "SELECT 1").Scan(&one) == nil
}
```

| Element           | What It Does                          | Why We Do It                                     |
| ----------------- | ------------------------------------- | ------------------------------------------------ |
| `SELECT 1`        | Cheapest possible liveness check      | Confirms the server actually answers queries     |
| `WithTimeout`     | Bounds how long a probe can hang      | A hung primary must count as a failure           |
| consecutive count | Requires `failThresh` misses in a row | One blip shouldn't trigger a disruptive failover |

```go
if failures >= m.failThresh {
    m.promote(ctx)   // commit to failover
}
```

**Why a threshold?** Networks hiccup. Failing over on a single missed probe causes
_flapping_ — needless promotions that themselves cause downtime. Requiring N
consecutive failures trades a few seconds of detection delay for stability.

### Promotion — `promote`

```go
m.standby.Exec(ctx, "SELECT pg_promote(wait := true)")
// then poll until writable:
m.standby.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)
if !inRecovery { m.current = m.standby; m.promoted = true }
```

| Step                       | What It Does                                          | Why We Do It                                  |
| -------------------------- | ----------------------------------------------------- | --------------------------------------------- |
| `pg_promote(wait:=true)`   | Tells the standby to exit recovery and accept writes  | This is PostgreSQL's built-in promotion call  |
| poll `pg_is_in_recovery()` | Wait until the node reports it is no longer a replica | Don't route writes before it's truly writable |
| swap `current = standby`   | Point the manager at the new primary                  | All later writes go to the promoted node      |

After this, the former standby is a normal read-write primary holding all WAL it
had replayed up to the crash.

### Simulating the crash — `simulatePrimaryCrash`

```go
exec.Command("docker", "stop", "failover_primary").CombinedOutput()
```

The program stops the primary's container so the manager observes a genuine
outage rather than a faked flag. If docker isn't reachable it prints the manual
command instead.

### The demo flow

1. Write a row to the primary; wait a second; confirm the **standby replicated** it.
2. `docker stop` the primary.
3. The probe loop logs `OK` until the stop, then logs failures `1/3, 2/3, 3/3`.
4. On the 3rd failure, `pg_promote()` runs; the standby becomes writable.
5. A post-failover write succeeds; the final count includes the pre-crash row →
   **no data lost**.

---

## Summary: Pros and Cons of Automatic Failover

### Pros

| Benefit               | Explanation                                             |
| --------------------- | ------------------------------------------------------- |
| **High availability** | Survives a primary crash without manual intervention    |
| **Fast recovery**     | Promotion takes seconds, not a human paging cycle       |
| **Data durability**   | Standby already holds replicated data up to the crash   |
| **Hands-off ops**     | No 3am "promote the replica" runbook to execute by hand |

### Cons

| Drawback                   | Explanation                                                   |
| -------------------------- | ------------------------------------------------------------- |
| **Split-brain risk**       | Two primaries if the old one returns un-fenced → divergence   |
| **Possible data loss**     | Async replication can lose the last un-replicated writes      |
| **False positives**        | A slow/partitioned primary may be promoted away unnecessarily |
| **Operational complexity** | Real HA needs consensus, fencing, and re-cloning the old node |

### When to Use Automatic Failover

**Always**, for any service that must stay up — but use a **battle-tested tool**
(Patroni, repmgr, pg_auto_failover, or a managed RDS/Cloud SQL HA option) rather
than hand-rolled logic. Those add the consensus store, fencing, and quorum that
prevent split-brain. The value of this simulation is understanding what those
tools do under the hood.

## Key Takeaways

1. Detection = repeated, timed health probes with a failure threshold.
2. A threshold prevents flapping on transient blips.
3. Promotion = `pg_promote()` + wait for `pg_is_in_recovery() = false`.
4. Async replication means failover can lose the last few writes (RPO > 0).
5. Production HA must add fencing/quorum to avoid split-brain — don't ship this as-is.

## go run output

```shell
$ go run main.go
✅ Failover manager up: primary (5470) + standby (5471)

============================================================
🔁 AUTOMATIC FAILOVER SIMULATION
============================================================

✍️  WRITE to primary, confirm it replicates
--------------------------------------------------
   wrote 1 row; primary now has 5 customers
   standby replicated: 5 customers (read-only hot standby)

💥 SIMULATE PRIMARY CRASH
--------------------------------------------------
   💥 stopping primary container (docker stop failover_primary)...
   stopped: failover_primary

🩺 HEALTH-CHECK LOOP → automatic promotion
--------------------------------------------------
   probe  1: primary UNREACHABLE ❌ (1/3)
   probe  2: primary UNREACHABLE ❌ (2/3)
   probe  3: primary UNREACHABLE ❌ (3/3)
   → failure threshold reached; promoting standby
   ✅ standby promoted; it now accepts writes

✍️  WRITE after failover (now served by promoted node)
--------------------------------------------------
   write succeeded on the new primary; 6 customers total
   (includes both the pre- and post-failover rows → no data lost)

📊 SUMMARY
--------------------------------------------------
   Detection: consecutive failed health probes.
   Action:    pg_promote() the standby, wait until writable, reroute writes.
   Restore the old node with: docker compose up -d postgres_primary
```

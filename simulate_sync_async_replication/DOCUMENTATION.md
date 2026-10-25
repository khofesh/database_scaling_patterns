# Synchronous vs Asynchronous Replication - Line-by-Line Documentation

This document explains the `simulate_sync_async_replication` project: **what** each
piece does, **why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure](#infrastructure)
3. [Application Code (main.go)](#application-code-maingo)
4. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

Streaming replication in PostgreSQL is asynchronous by default: the primary commits,
replies to the client, and ships WAL to standbys in the background. If the primary
dies before a standby receives that WAL, the commit is lost on failover.

Synchronous replication closes that gap by making the commit wait for standby
acknowledgements. The cost is latency on every commit and — more importantly — a
dependency: if the standby you're waiting for stops responding, writes stop too.
**Quorum** replication softens that by waiting for *any k of n* standbys.

---

## Infrastructure

### docker-compose.yml

- `postgres_primary` (:5502) — `wal_keep_size=256MB` so standbys can clone with
  `pg_basebackup` without the primary recycling WAL they still need.
- `standby_s1` (:5503), `standby_s2` (:5504) — physical streaming standbys, each given
  a `STANDBY_NAME`.

### init/replica-entrypoint.sh

Same approach as [`simulate_read_replicas`](../simulate_read_replicas): clone the
primary with `pg_basebackup -R` on first boot, then run as a hot standby. The one
addition:

```bash
exec gosu postgres postgres -c cluster_name="$STANDBY_NAME"
```

When `primary_conninfo` has no `application_name`, the WAL receiver reports
`cluster_name` instead. That name is what `synchronous_standby_names` matches and what
appears in `pg_stat_replication.application_name`.

### init/primary/

`01_setup_replication.sh` creates the `replicator` role and a `pg_hba.conf` rule;
`02_create_schema.sql` creates a single `ledger` table — the demo is about commit
behaviour, not schema.

---

## Application Code (main.go)

### Switching modes — `setStandbyNames`

```go
ALTER SYSTEM SET synchronous_standby_names = 'ANY 1 (s1, s2)'
SELECT pg_reload_conf()
```

`synchronous_standby_names` is reloadable (SIGHUP), so no restart is required. The
function then polls `pg_stat_replication.sync_state` until it shows `async`, `sync`
or `quorum`, so the next step never races the reload.

- `''` → every standby is `async`.
- `FIRST 1 (s1, s2)` → `s1` is `sync`, `s2` is `potential` (takes over if `s1`
  disconnects).
- `ANY 1 (s1, s2)` → both are `quorum`; whichever acknowledges first satisfies the
  commit.

### Measuring latency — `benchmark`

A pinned connection runs `SET synchronous_commit = <level>` and then 200 autocommit
inserts. Pinning matters: `SET` is per session, so with a pool you must run the
statements on the same connection.

| Level          | Commit returns after…                                       |
| -------------- | ----------------------------------------------------------- |
| `local`        | local WAL flush only (ignores standbys even if configured)  |
| `remote_write` | the sync standby wrote the WAL to its OS (not yet fsynced)  |
| `on`           | the sync standby fsynced the WAL — survives a standby crash |
| `remote_apply` | the sync standby replayed the WAL — visible to its queries  |

`synchronous_commit` can be set per transaction, so a single app can use `remote_apply`
for payments and `local` for click logs.

### Stale reads under async

`pauseReplay` calls `pg_wal_replay_pause()` on `s1`. The standby keeps **receiving and
flushing** WAL but stops **applying** it — a deterministic stand-in for a lagging
replica. An async insert commits immediately on the primary, and a read on `s1` doesn't
see it.

### Read-your-writes with `remote_apply` — `insert`

```go
pgx.BeginFunc(ctx, c.primary, func(tx pgx.Tx) error {
    tx.Exec(ctx, "SET LOCAL synchronous_commit = remote_apply")
    ...
})
```

`SET LOCAL` scopes the level to this one transaction. Once `COMMIT` returns, `s1` has
replayed the row, so a follow-up read on `s1` sees it.

### The blocked commit — `blockedCommit`

With `s1` paused and `remote_apply` required, the commit can never be acknowledged. The
function:

1. Opens a dedicated `pgx.Conn` with an `OnNotice` callback to capture server warnings.
2. Starts the insert, and after 2 seconds a goroutine calls
   `pg_cancel_backend(pid)` from another connection.

Why not `statement_timeout`? It is disabled before the commit begins, so it does not
interrupt the synchronous-replication wait. A cancel does — and PostgreSQL answers:

```
WARNING: canceling wait for synchronous replication due to user request
DETAIL:  The transaction has already committed locally, but might not have been
         replicated to the standby.
```

The insert returns its id and the row is visible on the primary. This is the important
subtlety: synchronous replication delays the *acknowledgement*; it cannot roll back a
commit that is already in the primary's WAL. Clients that time out must treat the
outcome as **unknown**, not failed.

### Quorum commit

With `ANY 1 (s1, s2)` and `s1` paused, a `remote_apply` commit returns in a few
milliseconds because `s2` acknowledges it. The row exists on `s2` but not yet on `s1`.
Pausing `s2` as well drops below quorum and commits block again.

### Cleanup — `reset`

Deferred at the top of `main`: `ALTER SYSTEM RESET synchronous_standby_names` and
`pg_wal_replay_resume()` on both standbys. A demo that leaves the primary in sync mode
with a paused standby would hang the next run.

---

## Summary: Pros and Cons

**Asynchronous**

- ✅ Lowest latency; standby problems never affect writes.
- ❌ Failover can lose recently acknowledged commits (RPO > 0).
- ❌ Replicas serve stale data.

**Synchronous (`FIRST n`)**

- ✅ RPO = 0 for acknowledged commits.
- ✅ With `remote_apply`, read-your-writes on the sync standby.
- ❌ Every commit pays a network round trip (and remote fsync / replay).
- ❌ A slow or dead sync standby blocks all writes until a potential standby takes over
  or an operator changes the setting.

**Quorum (`ANY k`)**

- ✅ RPO = 0 with tolerance for `n − k` slow/failed standbys.
- ✅ Latency is the k-th fastest acknowledgement, not the slowest.
- ❌ Needs more standbys; you can't tell which standby has the latest data without
  checking LSNs (failover tooling must handle this).

**When to use:** async for read scaling and DR; quorum sync for data that must survive a
primary loss; per-transaction `synchronous_commit` to pay the cost only where it matters.

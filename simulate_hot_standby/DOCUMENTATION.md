# Hot Standby + PITR Simulation - Line-by-Line Documentation

This document explains the `simulate_hot_standby` project: **what** each piece does,
**why** it is built that way, and the **pros and cons**.

---

## Table of Contents

1. [Project Overview](#project-overview)
2. [Infrastructure](#infrastructure)
3. [Entrypoint Scripts](#entrypoint-scripts)
4. [Application Code (main.go)](#application-code-maingo)
5. [Summary: Pros and Cons](#summary-pros-and-cons)

---

## Project Overview

Everything PostgreSQL changes is first written to the WAL. Any copy that replays the
same WAL ends up identical. This project uses that one mechanism three ways:

- **Streaming** it live to a **hot standby** (queryable).
- **Archiving** completed segments and replaying them on a **warm standby** (not
  queryable, log shipping).
- **Replaying** archived WAL on top of a **base backup**, stopping at a chosen point —
  **point-in-time recovery**.

---

## Infrastructure

### docker-compose.yml

| Service        | Port | Role                                                              |
| -------------- | ---- | ----------------------------------------------------------------- |
| `pg_primary`   | 5525 | `archive_mode=on`, `archive_command=test ! -f /archive/%f && cp %p /archive/%f` |
| `standby_hot`  | 5526 | Streaming standby; `hot_standby` defaults to on                   |
| `standby_warm` | 5527 | Archive-only standby, `hot_standby = off`; anonymous data volume  |
| `pitr`         | 5528 | Profile `pitr`; started by the demo from the base backup          |

Shared named volumes: **`hotstandby_archive`** (written by the primary, read-only for
the warm standby and PITR) and **`hotstandby_backups`** (base backup).

`archive_command` runs for every completed WAL segment. `test ! -f` refuses to overwrite
an existing file — a basic safety rule: an archive must never be silently replaced.
Non-zero exit makes PostgreSQL retry, keeping the segment until it is archived.

The warm standby and PITR instance keep their data in the image's anonymous volume, so
`docker compose up -V --force-recreate` gives them a fresh one each run. That's needed
because the demo promotes them.

### init/primary

`01_setup_replication.sh` creates the `replicator` role and `pg_hba.conf` entry;
`02_create_schema.sql` creates `accounts`, with a `batch` column so recovered data can
be traced to the step that wrote it.

---

## Entrypoint Scripts

### `primary-entrypoint.sh`

Creates `/archive` and `/backups` owned by `postgres` (Docker creates volume mount points
root-owned), then `exec`s the stock `docker-entrypoint.sh`.

### `hot-standby-entrypoint.sh`

Same as [`simulate_read_replicas`](../simulate_read_replicas): `pg_basebackup -R`
writes `standby.signal` and `primary_conninfo`, so the server starts as a streaming
standby; `hot_standby = on` (default) allows read-only queries during recovery.

### `warm-standby-entrypoint.sh`

```bash
pg_basebackup -h "$PRIMARY_HOST" -U replicator -D "$PGDATA" -Fp -Xs -P   # no -R
touch "$PGDATA/standby.signal"
restore_command = 'cp /archive/%f %p'
hot_standby = off
```

No `-R`, so no `primary_conninfo`: the standby never streams. `standby.signal` keeps it
in recovery indefinitely; it repeatedly calls `restore_command` for the next segment
(the `cp: cannot stat …` lines in its log are normal — they mean "not archived yet").
`hot_standby = off` makes it reject connections with SQLSTATE `57P03`.

### `pitr-entrypoint.sh`

```bash
cp -a /backups/base/. "$PGDATA"/
touch "$PGDATA/recovery.signal"
restore_command = 'cp /archive/%f %p'
recovery_target_name = 'before_disaster'
recovery_target_action = 'promote'
```

`recovery.signal` (not `standby.signal`) means **targeted recovery**: replay archived
WAL until the target, then act. `recovery_target_action = promote` turns it into a
normal read-write server at that point. Other targets: `recovery_target_time`,
`recovery_target_xid`, `recovery_target_lsn`.

---

## Application Code (main.go)

### Prepare

`compose("rm", "-sf", "pitr")` and `compose("up", "-d", "-V", "--force-recreate",
"standby_warm")` reset the instances the demo promotes. `waitWarmInRecovery` runs
`pg_isready` inside the warm container until it reports **rejecting connections** — the
healthy state of a warm standby.

### Connect to each standby

The hot standby answers `pg_is_in_recovery() = true`. Connecting to the warm standby
fails with `FATAL: the database system is not accepting connections`.

### Streaming

100 rows inserted on the primary appear on the hot standby within milliseconds.
`pg_stat_replication` lists only one client — the hot standby — because the warm
standby doesn't stream.

### Log shipping and its RPO

1. `pg_switch_wal()` closes the current segment so it gets archived; `waitArchived`
   polls `pg_stat_archiver.last_archived_wal` until it covers `pg_walfile_name(lsn - 1)`.
2. `batch-2` (50 rows) is written into the *new* segment, which isn't complete and so
   isn't archived.
3. `pg_ctl promote` on the warm standby: it replays what's in the archive and becomes a
   primary with **100** rows. The hot standby has **150**. The 50-row gap is the RPO of
   log shipping. `archive_timeout` forces a segment switch every N seconds to bound it.

### Base backup

`pg_basebackup -D /backups/base -Fp -Xs -c fast` inside the primary container (local
socket, `postgres` user). `-Xs` streams the WAL generated during the backup so it is
self-consistent; `-c fast` requests an immediate checkpoint. 25 more rows are then
written — they exist only in WAL, not in the backup.

### Disaster

`pg_create_restore_point('before_disaster')` writes a named marker into the WAL. Then
`DELETE FROM accounts`, a segment switch, and archiving. The hot standby reaches 0 rows
almost immediately.

### Point-in-time recovery

`docker compose --profile pitr up -d -V --force-recreate pitr` starts the PITR instance.
It copies the base backup, replays archived WAL up to `before_disaster`, and promotes.
`waitPromoted` polls until it accepts connections and `pg_is_in_recovery()` is false.
All 175 rows are back — the 150 in the base backup plus the 25 replayed from the archive.

---

## Summary: Pros and Cons

**Warm standby**

- ✅ Simple; needs only shared archive storage, no replication connection.
- ❌ No queries; data loss up to the un-archived segment; slower catch-up.

**Hot standby**

- ✅ Near-zero lag; serves read traffic; fast failover.
- ❌ Replicates mistakes immediately; long queries can conflict with replay
  (`max_standby_streaming_delay`, `hot_standby_feedback`).

**PITR**

- ✅ Recovers from logical mistakes and corruption to any moment within retention.
- ❌ Recovery time grows with the WAL to replay; needs archive storage and regular base
  backups — and regular **restore tests**.

**When to use:** hot standbys for HA and reads, PITR for every production database, warm
standby only where streaming isn't possible.

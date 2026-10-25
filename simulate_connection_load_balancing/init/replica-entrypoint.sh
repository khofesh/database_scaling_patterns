#!/bin/bash
# Custom entrypoint for a named streaming read replica.
#
# A replica is NOT initialised by initdb. Instead it takes a physical copy of the
# primary's data directory with pg_basebackup and then starts in standby mode,
# continuously replaying WAL streamed from the primary.
set -e

PGDATA="${PGDATA:-/var/lib/postgresql/data}"
PRIMARY_HOST="${PRIMARY_HOST:-postgres_primary}"
REPLICATOR_PASSWORD="${REPLICATOR_PASSWORD:-replicator}"
# The replica's name. It becomes cluster_name, so a client can ask any backend
# "who are you?" with current_setting('cluster_name') — how the demo shows which
# server HAProxy routed a connection to.
NODE_NAME="${NODE_NAME:?NODE_NAME must be set}"

# The named volume is created root-owned; make sure postgres owns its data dir.
mkdir -p "$PGDATA"
chown -R postgres:postgres "$PGDATA"
chmod 0700 "$PGDATA"

# Block until the primary is accepting connections before we try to clone it.
until gosu postgres pg_isready -h "$PRIMARY_HOST" -U postgres -q; do
    echo "Waiting for primary ($PRIMARY_HOST) to be ready..."
    sleep 2
done

# Only clone on first boot. On restarts the data dir is already populated and we
# just resume streaming where we left off.
if [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
    echo "Empty data directory -> cloning primary via pg_basebackup..."
    # -Fp  plain format   -Xs stream WAL during backup
    # -R   write standby.signal + primary_conninfo so it boots as a standby
    # -P   show progress
    PGPASSWORD="$REPLICATOR_PASSWORD" gosu postgres pg_basebackup \
        -h "$PRIMARY_HOST" -U replicator -D "$PGDATA" -Fp -Xs -R -P
    chown -R postgres:postgres "$PGDATA"
    echo "Base backup complete; starting as hot standby."
fi

# Hand off to the real server as the postgres user. hot_standby defaults to 'on',
# so this replica accepts read-only queries while it replays WAL.
exec gosu postgres postgres -c cluster_name="$NODE_NAME"

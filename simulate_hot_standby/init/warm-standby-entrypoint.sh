#!/bin/bash
# Entrypoint for the WARM standby: classic log shipping.
#
# It never connects to the primary for replication. Instead it replays WAL segment
# files the primary has ARCHIVED to /archive, fetched with restore_command. With
# hot_standby = off it refuses all client connections while in recovery: it exists
# only to be promoted if the primary is lost.
set -e

PGDATA="${PGDATA:-/var/lib/postgresql/data}"
PRIMARY_HOST="${PRIMARY_HOST:-pg_primary}"

mkdir -p "$PGDATA"
chown -R postgres:postgres "$PGDATA"
chmod 0700 "$PGDATA"

until gosu postgres pg_isready -h "$PRIMARY_HOST" -U postgres -q; do
    echo "Waiting for primary ($PRIMARY_HOST)..."
    sleep 2
done

if [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
    echo "Cloning primary with pg_basebackup (no -R: we don't stream)..."
    PGPASSWORD="${REPLICATOR_PASSWORD:-replicator}" gosu postgres pg_basebackup \
        -h "$PRIMARY_HOST" -U replicator -D "$PGDATA" -Fp -Xs -P
    # standby.signal = stay in recovery forever, waiting for more WAL.
    gosu postgres touch "$PGDATA/standby.signal"
    cat >> "$PGDATA/postgresql.auto.conf" <<-CONF
	restore_command = 'cp /archive/%f %p'
	hot_standby = off
	CONF
    echo "Warm standby ready: replaying archived WAL, no client connections."
fi

exec gosu postgres postgres

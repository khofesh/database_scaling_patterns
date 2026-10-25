#!/bin/bash
# Entrypoint for a POINT-IN-TIME RECOVERY instance.
#
# Starts from the base backup in /backups/base, then replays archived WAL from
# /archive, stopping at the named restore point RECOVERY_TARGET_NAME, and promotes.
set -e

PGDATA="${PGDATA:-/var/lib/postgresql/data}"
: "${RECOVERY_TARGET_NAME:?RECOVERY_TARGET_NAME must be set}"

mkdir -p "$PGDATA"
if [ -z "$(ls -A "$PGDATA" 2>/dev/null)" ]; then
    echo "Restoring base backup from /backups/base..."
    cp -a /backups/base/. "$PGDATA"/
    chown -R postgres:postgres "$PGDATA"
    chmod 0700 "$PGDATA"
    # recovery.signal = targeted recovery: replay until the target, then stop.
    gosu postgres touch "$PGDATA/recovery.signal"
    cat >> "$PGDATA/postgresql.auto.conf" <<-CONF
	restore_command = 'cp /archive/%f %p'
	recovery_target_name = '${RECOVERY_TARGET_NAME}'
	recovery_target_action = 'promote'
	CONF
fi

exec gosu postgres postgres

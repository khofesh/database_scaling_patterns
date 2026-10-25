#!/bin/bash
# Wrapper around the stock entrypoint: the shared /archive and /backups volumes are
# created root-owned, but archive_command and pg_basebackup run as postgres.
set -e
mkdir -p /archive /backups
chown postgres:postgres /archive /backups
exec docker-entrypoint.sh "$@"

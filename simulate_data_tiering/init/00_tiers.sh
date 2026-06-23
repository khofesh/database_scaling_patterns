#!/bin/sh
# Create the directories that back our storage tiers BEFORE the SQL runs.
# This script is executed by the postgres entrypoint as the `postgres` OS user, so
# the directories are owned by postgres — a hard requirement for CREATE TABLESPACE.
# They live under /var/lib/postgresql but OUTSIDE PGDATA (which is a subdirectory),
# so PostgreSQL won't reject them for being inside the data directory.
set -e
mkdir -p /var/lib/postgresql/tiers/hot \
         /var/lib/postgresql/tiers/warm \
         /var/lib/postgresql/tiers/cold

#!/bin/bash
# Runs once, on first start of the primary (docker-entrypoint-initdb.d).
# Creates a dedicated replication role and opens pg_hba.conf so replicas can
# stream the WAL.
set -e

# Dedicated login role with the REPLICATION attribute. Replicas authenticate as
# this role to run pg_basebackup and to open a streaming connection.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
    CREATE ROLE replicator WITH REPLICATION LOGIN PASSWORD 'replicator';
EOSQL

# Allow replication connections from any host on the docker network. The default
# pg_hba.conf ships these lines commented out, so we append our own.
cat >> "$PGDATA/pg_hba.conf" <<-EOF

# Allow the replicator role to open streaming-replication connections.
host replication replicator all scram-sha-256
EOF

echo "Replication role and pg_hba.conf entry configured."

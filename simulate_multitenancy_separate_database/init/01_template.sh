#!/bin/bash
# Runs on BOTH servers. Creates `tenant_template`, the database every tenant
# database is cloned from (CREATE DATABASE … TEMPLATE tenant_template). Changing the
# template changes what NEW tenants get; existing tenants need migrations.
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<-EOSQL
    CREATE DATABASE tenant_template;
EOSQL

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname tenant_template <<-EOSQL
    CREATE TABLE invoices (
        id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
        customer   TEXT NOT NULL,
        amount     NUMERIC(12, 2) NOT NULL,
        created_at TIMESTAMPTZ NOT NULL DEFAULT now()
    );
    CREATE TABLE schema_migrations (version INT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());
    INSERT INTO schema_migrations VALUES (1);
EOSQL

# Mark it as a template and block direct connections to it (cloning requires that
# nobody is connected to the source database).
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<-EOSQL
    ALTER DATABASE tenant_template WITH IS_TEMPLATE true ALLOW_CONNECTIONS false;
EOSQL

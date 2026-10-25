-- ANALYTICS database: owns no source data. It federates the other two with
-- postgres_fdw so analysts can run cross-domain SQL in one place, read-only.

CREATE EXTENSION postgres_fdw;

-- A "server" per remote database. Hostnames are docker-compose service names.
CREATE SERVER users_srv  FOREIGN DATA WRAPPER postgres_fdw
    OPTIONS (host 'users_db',  port '5432', dbname 'users');
CREATE SERVER orders_srv FOREIGN DATA WRAPPER postgres_fdw
    OPTIONS (host 'orders_db', port '5432', dbname 'orders',
             -- let the planner fetch remote row estimates for better join plans
             use_remote_estimate 'true');

-- Credentials used when connecting to each remote. In production: a read-only role.
CREATE USER MAPPING FOR postgres SERVER users_srv  OPTIONS (user 'postgres', password 'postgres');
CREATE USER MAPPING FOR postgres SERVER orders_srv OPTIONS (user 'postgres', password 'postgres');

-- Import remote table definitions as foreign tables, each into its own schema.
CREATE SCHEMA users_remote;
CREATE SCHEMA orders_remote;
IMPORT FOREIGN SCHEMA public FROM SERVER users_srv  INTO users_remote;
IMPORT FOREIGN SCHEMA public FROM SERVER orders_srv INTO orders_remote;

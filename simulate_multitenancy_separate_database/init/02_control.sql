-- Control plane (server_a only): the tenant catalogue. The application looks a
-- tenant up here to learn WHICH SERVER and WHICH DATABASE hold its data.
CREATE TABLE tenants (
    slug       TEXT PRIMARY KEY,
    server     TEXT NOT NULL,                -- 'server_a' | 'server_b'
    dbname     TEXT NOT NULL,
    plan       TEXT NOT NULL,                -- 'standard' | 'premium'
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (server, dbname)
);

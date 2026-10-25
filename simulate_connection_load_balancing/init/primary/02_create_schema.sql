-- Schema lives only on the primary; replicas receive it via streaming replication.
CREATE TABLE visits (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    page       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO visits (page) SELECT '/page/' || g FROM generate_series(1, 100) AS g;

-- Schema lives only on the primary; standbys receive it through physical streaming
-- replication. One small table is enough: the demo is about COMMIT behaviour.
CREATE TABLE ledger (
    id         BIGSERIAL PRIMARY KEY,
    note       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

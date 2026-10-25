-- Schema lives only on the primary; standbys get it through WAL (streamed or archived).
CREATE TABLE accounts (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner      TEXT NOT NULL,
    balance    NUMERIC(12, 2) NOT NULL,
    batch      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

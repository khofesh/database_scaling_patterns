-- EVENT SOURCING: the source of truth is an append-only log of immutable facts.
-- Current state is never stored as the truth; it is *derived* by replaying events.
--
-- Two tables only:
--   events     - the immutable log (the truth)
--   snapshots  - a periodic, cached fold of the log (a pure optimization)

-- The event log. Rows are INSERT-only; they are never UPDATEd or DELETEd.
CREATE TABLE events (
    -- global, monotonically increasing ordering across all aggregates
    global_seq   BIGSERIAL PRIMARY KEY,

    -- which aggregate (e.g. one bank account) this event belongs to
    aggregate_id TEXT   NOT NULL,

    -- per-aggregate version, 1,2,3,... This is the optimistic-concurrency key:
    -- two writers that both think the account is at version N will collide on the
    -- UNIQUE constraint below, and exactly one wins.
    version      INT    NOT NULL,

    event_type   TEXT   NOT NULL,        -- 'AccountOpened', 'Deposited', ...
    payload      JSONB  NOT NULL,        -- the event's data
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- An aggregate can have only ONE event at each version. This single
    -- constraint is what gives us optimistic concurrency control for free.
    UNIQUE (aggregate_id, version)
);

-- Replaying an aggregate means scanning its events in version order.
CREATE INDEX idx_events_aggregate ON events (aggregate_id, version);

-- Snapshots cache "the state of aggregate X as of version V" so we don't have to
-- replay the entire history every time. They are derived data: deleting every
-- snapshot must never change the result of a rebuild, only its speed.
CREATE TABLE snapshots (
    aggregate_id TEXT NOT NULL,
    version      INT  NOT NULL,          -- the last event version folded in
    state        JSONB NOT NULL,         -- the materialized aggregate state
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (aggregate_id, version)
);

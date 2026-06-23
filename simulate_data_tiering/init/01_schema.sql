-- DATA TIERING: keep frequently-accessed ("hot") data on fast storage and migrate
-- aging data to progressively cheaper ("warm", then "cold") storage, automatically,
-- based on age — while keeping it all queryable through one table.
--
-- The two ingredients PostgreSQL gives us:
--   1. TABLESPACES  - named storage locations we can map to different physical media
--   2. PARTITIONING - the table is split by time, so each time-slice is a unit we can
--                     move between tablespaces independently and prune at query time.

-- Three tiers. In production each LOCATION would be a mount on different hardware.
CREATE TABLESPACE hot_tier  LOCATION '/var/lib/postgresql/tiers/hot';
CREATE TABLESPACE warm_tier LOCATION '/var/lib/postgresql/tiers/warm';
CREATE TABLESPACE cold_tier LOCATION '/var/lib/postgresql/tiers/cold';

-- A time-series table partitioned by month. The partitions are created by the Go
-- program (relative to "now") so the demo is always current. New data lands in the
-- hot tier; the tiering job ages partitions down to warm and cold over time.
CREATE TABLE events (
    id         BIGSERIAL,
    created_at TIMESTAMPTZ NOT NULL,
    payload    TEXT NOT NULL,
    -- The partition key must be part of the primary key in a partitioned table.
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

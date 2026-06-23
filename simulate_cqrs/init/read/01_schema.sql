-- READ MODEL: denormalized. One pre-joined, pre-aggregated row per order so a
-- query is a single-row primary-key lookup — no JOINs, no SUM at read time.
-- This table is derived data: it is rebuilt by the projector from write-side
-- events and may briefly lag behind the write store (eventual consistency).

CREATE TABLE order_summary (
    order_id      INT PRIMARY KEY,
    customer_name TEXT NOT NULL,
    customer_email TEXT NOT NULL,
    item_count    INT NOT NULL,
    total_cents   INT NOT NULL,
    status        TEXT NOT NULL,
    placed_at     TIMESTAMPTZ NOT NULL
);

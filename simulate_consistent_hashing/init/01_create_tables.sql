-- Every node has the same schema. Consistent hashing decides WHICH node a key
-- lives on; the table layout itself is identical everywhere.
CREATE TABLE IF NOT EXISTS kv (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

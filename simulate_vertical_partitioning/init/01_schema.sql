-- VERTICAL PARTITIONING: split a table by COLUMNS. Columns read on every request
-- ("hot") stay in a narrow table; rarely-read text ("cold") and large binary objects
-- (BLOBs) move to 1:1 side tables sharing the same primary key.

-- ❶ BEFORE: one wide table. bio (~1 KB) and preferences (~300 B) are below the
-- ~2 KB TOAST threshold, so they are stored INLINE in every heap row — every scan of
-- "hot" columns drags them along. avatar (4 KB) is TOASTed (moved out of line).
CREATE TABLE users_wide (
    id          BIGINT PRIMARY KEY,
    email       TEXT NOT NULL,
    name        TEXT NOT NULL,
    status      TEXT NOT NULL,
    last_login  TIMESTAMPTZ NOT NULL,
    bio         TEXT,
    preferences JSONB,
    avatar      BYTEA
);

INSERT INTO users_wide
SELECT g,
       'user' || g || '@example.com',
       'User ' || g,
       (ARRAY['active', 'active', 'active', 'inactive', 'banned'])[1 + g % 5],
       now() - (random() * interval '60 days'),
       repeat(md5(g::text), 31),                                   -- ~1 KB of text
       jsonb_build_object('theme', 'dark', 'lang', 'en', 'tags',
                          (SELECT jsonb_agg(md5((g * 10 + t)::text)) FROM generate_series(1, 6) t)),
       -- 4 KB of random (incompressible, like a JPEG) bytes. "g * 0" references the
       -- outer row so the subquery is re-run per user instead of once.
       (SELECT string_agg(uuid_send(gen_random_uuid()), ''::bytea)
          FROM generate_series(1, 256 + g * 0))
FROM generate_series(1, 30000) AS g;

-- ❷ AFTER: the same data split vertically.

-- Hot: small, fixed-ish columns used by login, listings, filters. Many rows per page.
CREATE TABLE users_core (
    id         BIGINT PRIMARY KEY,
    email      TEXT NOT NULL,
    name       TEXT NOT NULL,
    status     TEXT NOT NULL,
    last_login TIMESTAMPTZ NOT NULL
);

-- Cold: profile text read only on the profile page.
CREATE TABLE users_profile (
    user_id     BIGINT PRIMARY KEY REFERENCES users_core (id) ON DELETE CASCADE,
    bio         TEXT,
    preferences JSONB
);

-- BLOB: binary data, fetched by its own endpoint (or kept in object storage with
-- only a URL here). STORAGE EXTERNAL = out-of-line, no compression attempt — right
-- for already-compressed images.
CREATE TABLE users_avatar (
    user_id BIGINT PRIMARY KEY REFERENCES users_core (id) ON DELETE CASCADE,
    avatar  BYTEA NOT NULL
);
ALTER TABLE users_avatar ALTER COLUMN avatar SET STORAGE EXTERNAL;

INSERT INTO users_core    SELECT id, email, name, status, last_login FROM users_wide;
INSERT INTO users_profile SELECT id, bio, preferences FROM users_wide;
INSERT INTO users_avatar  SELECT id, avatar FROM users_wide;

-- Compatibility view: code that wants "the whole user" still gets one relation.
CREATE VIEW users_full AS
SELECT c.*, p.bio, p.preferences, a.avatar
FROM users_core c
LEFT JOIN users_profile p ON p.user_id = c.id
LEFT JOIN users_avatar  a ON a.user_id = c.id;

VACUUM ANALYZE;

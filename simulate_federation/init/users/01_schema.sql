-- USERS database: owned by the "accounts" domain/team. Nothing else lives here.
CREATE TABLE users (
    id         BIGINT PRIMARY KEY,
    name       TEXT NOT NULL,
    email      TEXT NOT NULL UNIQUE,
    country    TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO users (id, name, email, country)
SELECT g, 'User ' || g, 'user' || g || '@example.com',
       (ARRAY['ID', 'SG', 'MY', 'TH', 'VN'])[1 + g % 5]
FROM generate_series(1, 5000) g;

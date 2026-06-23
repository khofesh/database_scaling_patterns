-- SHARED DATABASE, SHARED SCHEMA multi-tenancy.
-- All tenants live in the same tables; every tenant-owned row carries a tenant_id.
-- Isolation is enforced by PostgreSQL Row-Level Security (RLS), so the application
-- physically cannot read or write another tenant's rows even if it forgets a
-- WHERE clause.

CREATE TABLE tenants (
    id   SERIAL PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE TABLE documents (
    id         SERIAL PRIMARY KEY,
    tenant_id  INT NOT NULL REFERENCES tenants(id),
    title      TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_documents_tenant ON documents (tenant_id);

-- Seed three tenants and a few documents each.
INSERT INTO tenants (name) VALUES ('Acme Corp'), ('Globex'), ('Initech');

INSERT INTO documents (tenant_id, title, body) VALUES
    (1, 'Acme roadmap',     'Acme internal plans'),
    (1, 'Acme invoices',    'Acme billing data'),
    (2, 'Globex strategy',  'Globex secret strategy'),
    (2, 'Globex contacts',  'Globex customer list'),
    (3, 'Initech memo',     'Initech TPS reports');

-- The application connects as this NON-superuser role. Superusers and table owners
-- bypass RLS by default, which would defeat the whole point — so the app must use
-- an ordinary role.
CREATE ROLE app_user WITH LOGIN PASSWORD 'app_pass';
GRANT SELECT, INSERT, UPDATE, DELETE ON documents TO app_user;
GRANT SELECT ON tenants TO app_user;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO app_user;

-- Turn on RLS. FORCE makes it apply even to the table owner, so nobody escapes it.
ALTER TABLE documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE documents FORCE ROW LEVEL SECURITY;

-- The policy: a session may only see/modify rows whose tenant_id matches the
-- current tenant, taken from the session setting app.current_tenant.
--   USING      → which existing rows are visible (SELECT/UPDATE/DELETE)
--   WITH CHECK → which new/updated rows are allowed (INSERT/UPDATE)
CREATE POLICY tenant_isolation ON documents
    USING (tenant_id = current_setting('app.current_tenant')::int)
    WITH CHECK (tenant_id = current_setting('app.current_tenant')::int);

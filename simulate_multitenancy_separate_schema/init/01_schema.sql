-- SHARED DATABASE, SEPARATE SCHEMA multi-tenancy.
--
-- The `public` schema holds only the control plane: the tenant catalogue. Tenant
-- data lives in one schema per tenant (tenant_acme, tenant_globex, ...), each
-- created by the provisioning code with identical tables.

CREATE TABLE tenants (
    slug        TEXT PRIMARY KEY,           -- 'acme'
    schema_name TEXT NOT NULL UNIQUE,       -- 'tenant_acme'
    plan        TEXT NOT NULL DEFAULT 'standard',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The application connects as app_user: NOT a superuser and with no table
-- privileges of its own. Per request it switches to the tenant's role
-- (SET LOCAL ROLE), which can only use that tenant's schema.
CREATE ROLE app_user LOGIN PASSWORD 'app';
GRANT SELECT ON tenants TO app_user;

-- Nobody but the owner may create objects in public (PG15+ default, made explicit).
REVOKE CREATE ON SCHEMA public FROM PUBLIC;

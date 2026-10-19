-- Migration 001: Initial schema for the Pi proxy service.
-- Creates all core tables defined in §15.1 of the implementation plan.

BEGIN;

-- ============================================================
-- Tenants
-- ============================================================
CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================================================
-- Users
-- ============================================================
CREATE TABLE IF NOT EXISTS users (
    id           TEXT PRIMARY KEY,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    email        TEXT NOT NULL,
    display_name TEXT,
    status       TEXT NOT NULL DEFAULT 'active',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, email)
);

CREATE INDEX idx_users_tenant_id ON users(tenant_id);

-- ============================================================
-- API Keys
-- ============================================================
CREATE TABLE IF NOT EXISTS api_keys (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id),
    user_id     TEXT NOT NULL REFERENCES users(id),
    prefix      TEXT NOT NULL UNIQUE,
    key_hash    BYTEA NOT NULL,
    scopes      TEXT[] NOT NULL DEFAULT '{llm:invoke}',
    status      TEXT NOT NULL DEFAULT 'active',
    expires_at  TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_api_keys_tenant_id ON api_keys(tenant_id);
CREATE INDEX idx_api_keys_user_id ON api_keys(user_id);
CREATE INDEX idx_api_keys_prefix ON api_keys(prefix);

-- ============================================================
-- Provider Accounts
-- ============================================================
CREATE TABLE IF NOT EXISTS provider_accounts (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT REFERENCES tenants(id),
    provider   TEXT NOT NULL,
    name       TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'active',
    config     JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_provider_accounts_tenant ON provider_accounts(tenant_id);

-- ============================================================
-- Provider Credentials
-- ============================================================
CREATE TABLE IF NOT EXISTS provider_credentials (
    id                  TEXT PRIMARY KEY,
    provider_account_id TEXT NOT NULL REFERENCES provider_accounts(id),
    version             INTEGER NOT NULL DEFAULT 1,
    status              TEXT NOT NULL DEFAULT 'pending_validation',
    secret_ref          TEXT,
    encrypted_blob      BYTEA,
    kms_key_id          TEXT,
    fingerprint         TEXT NOT NULL,
    valid_from          TIMESTAMPTZ,
    valid_until         TIMESTAMPTZ,
    last_used_at        TIMESTAMPTZ,
    created_by          TEXT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_account_id, version)
);

CREATE INDEX idx_provider_credentials_account ON provider_credentials(provider_account_id);

-- ============================================================
-- Model Aliases
-- ============================================================
CREATE TABLE IF NOT EXISTS model_aliases (
    id                TEXT PRIMARY KEY,
    tenant_id         TEXT REFERENCES tenants(id),
    public_provider   TEXT NOT NULL,
    public_model      TEXT NOT NULL,
    upstream_provider TEXT NOT NULL,
    upstream_model    TEXT NOT NULL,
    provider_account_id TEXT NOT NULL REFERENCES provider_accounts(id),
    api_type          TEXT NOT NULL,
    capabilities      JSONB NOT NULL DEFAULT '{}',
    cost              JSONB NOT NULL DEFAULT '{}',
    limits            JSONB NOT NULL DEFAULT '{}',
    status            TEXT NOT NULL DEFAULT 'active',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, public_provider, public_model)
);

CREATE INDEX idx_model_aliases_tenant ON model_aliases(tenant_id);
CREATE INDEX idx_model_aliases_provider ON model_aliases(public_provider);

-- ============================================================
-- Quota Policies
-- ============================================================
CREATE TABLE IF NOT EXISTS quota_policies (
    id         TEXT PRIMARY KEY,
    tenant_id  TEXT NOT NULL REFERENCES tenants(id),
    scope_type TEXT NOT NULL,
    scope_id   TEXT NOT NULL,
    priority   INTEGER NOT NULL DEFAULT 0,
    limits     JSONB NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_quota_policies_tenant ON quota_policies(tenant_id);
CREATE INDEX idx_quota_policies_scope ON quota_policies(scope_type, scope_id);

-- ============================================================
-- Usage Events
-- ============================================================
CREATE TABLE IF NOT EXISTS usage_events (
    id                 TEXT PRIMARY KEY,
    request_id         TEXT NOT NULL UNIQUE,
    tenant_id          TEXT REFERENCES tenants(id),
    user_id            TEXT REFERENCES users(id),
    key_id             TEXT REFERENCES api_keys(id),
    provider           TEXT NOT NULL,
    model              TEXT NOT NULL,
    upstream_provider  TEXT NOT NULL,
    upstream_model     TEXT NOT NULL,
    status             TEXT NOT NULL,
    error_class        TEXT,
    input_tokens       BIGINT NOT NULL DEFAULT 0,
    output_tokens      BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens   BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens  BIGINT NOT NULL DEFAULT 0,
    cache_write_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens       BIGINT NOT NULL DEFAULT 0,
    estimated_usage    BOOLEAN NOT NULL DEFAULT FALSE,
    cost_total_cents   NUMERIC(18,6) NOT NULL DEFAULT 0,
    started_at         TIMESTAMPTZ NOT NULL,
    completed_at       TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_usage_events_tenant ON usage_events(tenant_id);
CREATE INDEX idx_usage_events_user ON usage_events(user_id);
CREATE INDEX idx_usage_events_created ON usage_events(created_at);
CREATE INDEX idx_usage_events_request ON usage_events(request_id);

-- ============================================================
-- Audit Events
-- ============================================================
CREATE TABLE IF NOT EXISTS audit_events (
    id             TEXT PRIMARY KEY,
    tenant_id      TEXT REFERENCES tenants(id),
    actor_type     TEXT NOT NULL,
    actor_id       TEXT NOT NULL,
    action         TEXT NOT NULL,
    target_type    TEXT NOT NULL,
    target_id      TEXT,
    result         TEXT NOT NULL,
    redacted_diff  JSONB,
    request_id     TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_audit_events_tenant ON audit_events(tenant_id);
CREATE INDEX idx_audit_events_action ON audit_events(action);
CREATE INDEX idx_audit_events_created ON audit_events(created_at);

-- ============================================================
-- Version tracking for migrations
-- ============================================================
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version) VALUES (1);

COMMIT;

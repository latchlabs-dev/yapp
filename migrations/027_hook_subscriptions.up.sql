-- Migration 027: DB-managed hook subscriptions.
--
-- Hooks were previously one global webhook URL in the environment, so adding or
-- changing one meant a redeploy. These tables make them tenant-scoped records
-- the admin API manages and the proxy hot-reloads.
--
-- Two columns carry real risk and are deliberately explicit rather than implied:
-- include_content turns a metrics webhook into a content-egress channel, and
-- allow_mutation lets an endpoint rewrite what the model sees or what the user
-- reads. Both default to false.

BEGIN;

CREATE TABLE IF NOT EXISTS hook_subscriptions (
    id               TEXT PRIMARY KEY,
    name             TEXT NOT NULL,
    tenant_id        TEXT REFERENCES tenants(id),   -- NULL = every tenant
    project_id       TEXT,                          -- NULL = every project
    event_type       TEXT NOT NULL,
    url              TEXT NOT NULL,
    secret_encrypted BYTEA,
    timeout_ms       INTEGER NOT NULL DEFAULT 1500,
    mode             TEXT NOT NULL DEFAULT 'async',
    include_content  BOOLEAN NOT NULL DEFAULT FALSE,
    allow_mutation   BOOLEAN NOT NULL DEFAULT FALSE,
    fail_mode        TEXT NOT NULL DEFAULT 'open',
    priority         INTEGER NOT NULL DEFAULT 100,
    enabled          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT hook_subscriptions_mode_check
        CHECK (mode IN ('blocking', 'async')),
    CONSTRAINT hook_subscriptions_fail_mode_check
        CHECK (fail_mode IN ('open', 'closed')),
    -- An async hook is delivered after the turn has already been served, so it
    -- has nothing left to mutate and nothing to fail closed over. Rejecting the
    -- combination at write time beats silently ignoring it at delivery time.
    CONSTRAINT hook_subscriptions_async_cannot_mutate
        CHECK (mode = 'blocking' OR (allow_mutation = FALSE AND fail_mode = 'open')),
    CONSTRAINT hook_subscriptions_timeout_range
        CHECK (timeout_ms BETWEEN 100 AND 30000)
);

CREATE INDEX IF NOT EXISTS idx_hook_subscriptions_lookup
    ON hook_subscriptions(event_type, tenant_id)
    WHERE enabled;

CREATE TABLE IF NOT EXISTS hook_deliveries (
    id              BIGSERIAL PRIMARY KEY,
    subscription_id TEXT NOT NULL,
    event_id        TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    tenant_id       TEXT,
    status          TEXT NOT NULL,
    http_status     INTEGER,
    latency_ms      INTEGER,
    error           TEXT,
    decision        JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_hook_deliveries_subscription
    ON hook_deliveries(subscription_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_hook_deliveries_created
    ON hook_deliveries(created_at);

INSERT INTO schema_migrations (version, applied_at)
VALUES (27, now())
ON CONFLICT (version) DO NOTHING;

COMMIT;

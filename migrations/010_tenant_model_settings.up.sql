CREATE TABLE IF NOT EXISTS tenant_model_settings (
    tenant_id  TEXT PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
    mode       TEXT NOT NULL DEFAULT 'blacklist' CHECK (mode IN ('blacklist', 'whitelist')),
    model_ids  TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO schema_migrations (version, applied_at)
VALUES (10, now())
ON CONFLICT (version) DO NOTHING;

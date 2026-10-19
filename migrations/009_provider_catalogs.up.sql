CREATE TABLE IF NOT EXISTS provider_catalogs (
    namespace TEXT NOT NULL,
    provider_key TEXT NOT NULL,
    display_name TEXT NOT NULL,
    api_type TEXT NOT NULL,
    base_url TEXT NOT NULL DEFAULT '',
    public_base_path TEXT NOT NULL DEFAULT '',
    public_api_base_path TEXT NOT NULL DEFAULT '',
    allowed_paths TEXT[] NOT NULL DEFAULT '{}',
    metadata JSONB NOT NULL DEFAULT '{}',
    source_version TEXT NOT NULL DEFAULT '',
    synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, provider_key)
);

CREATE TABLE IF NOT EXISTS provider_catalog_models (
    namespace TEXT NOT NULL,
    provider_key TEXT NOT NULL,
    model_id TEXT NOT NULL,
    display_name TEXT NOT NULL,
    api_type TEXT NOT NULL,
    base_url TEXT NOT NULL DEFAULT '',
    reasoning BOOLEAN NOT NULL DEFAULT false,
    input TEXT[] NOT NULL DEFAULT '{}',
    thinking_level_map JSONB,
    compat JSONB,
    cost JSONB NOT NULL DEFAULT '{}',
    context_window INTEGER NOT NULL DEFAULT 0,
    max_tokens INTEGER NOT NULL DEFAULT 0,
    metadata JSONB NOT NULL DEFAULT '{}',
    source_version TEXT NOT NULL DEFAULT '',
    synced_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, provider_key, model_id),
    FOREIGN KEY (namespace, provider_key)
        REFERENCES provider_catalogs(namespace, provider_key)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_provider_catalog_models_provider
    ON provider_catalog_models(namespace, provider_key, model_id);

INSERT INTO schema_migrations (version, applied_at)
VALUES (9, now())
ON CONFLICT (version) DO NOTHING;

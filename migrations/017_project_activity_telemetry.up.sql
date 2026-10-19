-- Make lightweight session metadata a baseline capability and add durable,
-- repository-attributed model activity plus aggregated extension error reports.

-- Record exactly which existing keys received sessions:submit here so the down
-- migration never strips a scope that an administrator had already granted.
CREATE TABLE IF NOT EXISTS migration_017_session_submit_grants (
    key_id      TEXT PRIMARY KEY REFERENCES api_keys(id) ON DELETE CASCADE,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO migration_017_session_submit_grants (key_id)
SELECT id
  FROM api_keys
 WHERE NOT ('sessions:submit' = ANY(scopes))
ON CONFLICT (key_id) DO NOTHING;

UPDATE api_keys
   SET scopes = scopes || ARRAY['sessions:submit']::TEXT[]
 WHERE NOT ('sessions:submit' = ANY(scopes));

-- usage_events is the durable, server-observed request ledger. The proxy only
-- writes a project_id after validating it against the authenticated tenant.
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS project_id TEXT;

ALTER TABLE usage_events
    ADD CONSTRAINT usage_events_tenant_project_fk
    FOREIGN KEY (tenant_id, project_id)
    REFERENCES memory_projects(tenant_id, id);

CREATE INDEX IF NOT EXISTS idx_usage_events_project_started
    ON usage_events(project_id, started_at DESC)
    WHERE project_id IS NOT NULL;

-- Extension failures are aggregated by authenticated key, operation, error,
-- project and session. Heartbeats update occurrence_count instead of appending
-- an unbounded stream of identical rows.
CREATE TABLE IF NOT EXISTS extension_errors (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL REFERENCES tenants(id),
    user_id             TEXT REFERENCES users(id),
    key_id              TEXT NOT NULL REFERENCES api_keys(id),
    operation           TEXT NOT NULL,
    error_code          TEXT NOT NULL,
    http_status         INTEGER NOT NULL DEFAULT 0,
    message             TEXT NOT NULL DEFAULT '',
    project_id          TEXT NOT NULL DEFAULT '',
    conversation_id     TEXT NOT NULL DEFAULT '',
    extension_version   TEXT NOT NULL DEFAULT '',
    occurrence_count    BIGINT NOT NULL DEFAULT 1,
    first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT extension_errors_http_status_check CHECK (http_status BETWEEN 0 AND 599),
    CONSTRAINT extension_errors_operation_length CHECK (char_length(operation) BETWEEN 1 AND 64),
    CONSTRAINT extension_errors_code_length CHECK (char_length(error_code) BETWEEN 1 AND 64),
    CONSTRAINT extension_errors_message_length CHECK (char_length(message) <= 512),
    CONSTRAINT extension_errors_project_length CHECK (char_length(project_id) <= 200),
    CONSTRAINT extension_errors_conversation_length CHECK (char_length(conversation_id) <= 200),
    CONSTRAINT extension_errors_version_length CHECK (char_length(extension_version) <= 64),
    UNIQUE (tenant_id, key_id, operation, error_code, project_id, conversation_id)
);

CREATE INDEX IF NOT EXISTS idx_extension_errors_last_seen
    ON extension_errors(last_seen_at DESC);

CREATE INDEX IF NOT EXISTS idx_extension_errors_tenant_last_seen
    ON extension_errors(tenant_id, last_seen_at DESC);

INSERT INTO schema_migrations (version, applied_at)
VALUES (17, now())
ON CONFLICT (version) DO NOTHING;

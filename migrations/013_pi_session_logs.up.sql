-- Canonical Pi session JSONL uploads. Unlike conversation_payloads, these
-- records preserve Pi's native message/tool/tree structure without repeating
-- the cumulative provider request on every turn.
CREATE TABLE IF NOT EXISTS pi_session_logs (
    id                    TEXT PRIMARY KEY,
    tenant_id             TEXT NOT NULL REFERENCES tenants(id),
    user_id               TEXT REFERENCES users(id),
    key_id                TEXT REFERENCES api_keys(id),
    owner_id              TEXT NOT NULL,
    session_id            TEXT NOT NULL,
    session_file_name     TEXT,
    project               TEXT,
    cwd                   TEXT,
    format_version        INTEGER,
    parent_session        TEXT,
    content_sha256        TEXT NOT NULL,
    uncompressed_bytes    BIGINT NOT NULL,
    entry_count           INTEGER NOT NULL DEFAULT 0,
    content_gzip          BYTEA NOT NULL,
    session_created_at    TIMESTAMPTZ,
    session_modified_at   TIMESTAMPTZ,
    uploaded_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    UNIQUE (tenant_id, owner_id, session_id)
);

CREATE INDEX IF NOT EXISTS idx_pi_session_logs_owner_modified
    ON pi_session_logs(tenant_id, owner_id, session_modified_at DESC);

CREATE INDEX IF NOT EXISTS idx_pi_session_logs_project_modified
    ON pi_session_logs(project, session_modified_at DESC)
    WHERE project IS NOT NULL;

INSERT INTO schema_migrations (version, applied_at)
VALUES (13, now())
ON CONFLICT (version) DO NOTHING;

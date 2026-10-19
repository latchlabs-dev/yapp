-- Raw coding-agent conversation captures. Payloads are stored out of the main
-- usage_events table so reporting rows stay small and the backend remains
-- replaceable with blob/object storage later.
CREATE TABLE IF NOT EXISTS conversation_payloads (
    id                       TEXT PRIMARY KEY,
    request_id               TEXT NOT NULL UNIQUE,
    conversation_id          TEXT,
    tenant_id                TEXT REFERENCES tenants(id),
    user_id                  TEXT REFERENCES users(id),
    key_id                   TEXT REFERENCES api_keys(id),
    provider                 TEXT NOT NULL,
    model                    TEXT NOT NULL,
    status                   TEXT NOT NULL,
    error_class              TEXT,
    streaming                BOOLEAN NOT NULL DEFAULT false,
    request_body_compressed  BYTEA NOT NULL,
    response_body_compressed BYTEA NOT NULL,
    request_truncated        BOOLEAN NOT NULL DEFAULT false,
    response_truncated       BOOLEAN NOT NULL DEFAULT false,
    response_format          TEXT NOT NULL DEFAULT 'json',
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at             TIMESTAMPTZ,
    expires_at               TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_conversation_payloads_tenant_created
    ON conversation_payloads(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversation_payloads_conversation
    ON conversation_payloads(conversation_id, created_at DESC)
    WHERE conversation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conversation_payloads_expires
    ON conversation_payloads(expires_at)
    WHERE expires_at IS NOT NULL;

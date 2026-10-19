-- Migration 026: Web console — OIDC identity, browser sessions, conversations,
-- and blob-backed attachments.
--
-- The web console is a public (internet-facing) surface distinct from the
-- Tailscale-only admin UI. Users authenticate against an external OIDC provider
-- (Zitadel) rather than holding a proxy API key, so requests carry a session
-- cookie and resolve to a tenant/user with no api_keys row. Every table here is
-- tenant-scoped; nothing is global.

BEGIN;

-- ============================================================
-- Identity: email domain -> tenant
-- ============================================================
-- `users` is UNIQUE (tenant_id, email), not unique on email alone, so an email
-- address by itself cannot identify a tenant. This table is the mapping
-- authority: a login from an unmapped domain is refused rather than guessed.
CREATE TABLE IF NOT EXISTS web_identity_domains (
    domain         TEXT PRIMARY KEY,
    tenant_id      TEXT NOT NULL REFERENCES tenants(id),
    default_scopes TEXT[] NOT NULL DEFAULT '{llm:invoke,llm:models:read,memory:context:read,memory:observe}',
    status         TEXT NOT NULL DEFAULT 'active',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_web_identity_domains_tenant
    ON web_identity_domains(tenant_id);

-- ============================================================
-- Identity: OIDC subject -> user
-- ============================================================
-- Binds the provider's stable subject claim to a users row. Keyed on
-- (issuer, subject) rather than email so a later address change re-labels the
-- identity instead of re-mapping the person to a different user.
CREATE TABLE IF NOT EXISTS web_identities (
    id            TEXT PRIMARY KEY,
    issuer        TEXT NOT NULL,
    subject       TEXT NOT NULL,
    tenant_id     TEXT NOT NULL REFERENCES tenants(id),
    user_id       TEXT NOT NULL REFERENCES users(id),
    email         TEXT NOT NULL,
    display_name  TEXT,
    last_login_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (issuer, subject)
);

CREATE INDEX IF NOT EXISTS idx_web_identities_user ON web_identities(user_id);
CREATE INDEX IF NOT EXISTS idx_web_identities_tenant ON web_identities(tenant_id);

-- ============================================================
-- Browser sessions
-- ============================================================
-- The cookie carries the raw token; only its SHA-256 is stored, so a database
-- disclosure does not yield usable sessions. `scopes` is snapshotted at login
-- so a domain policy change does not silently widen a live session.
CREATE TABLE IF NOT EXISTS web_sessions (
    id           TEXT PRIMARY KEY,
    token_hash   BYTEA NOT NULL UNIQUE,
    tenant_id    TEXT NOT NULL REFERENCES tenants(id),
    user_id      TEXT NOT NULL REFERENCES users(id),
    identity_id  TEXT NOT NULL REFERENCES web_identities(id) ON DELETE CASCADE,
    scopes       TEXT[] NOT NULL DEFAULT '{}',
    csrf_token   TEXT NOT NULL,
    client_ip    INET,
    user_agent   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_web_sessions_user ON web_sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_web_sessions_expiry
    ON web_sessions(expires_at)
    WHERE revoked_at IS NULL;

-- ============================================================
-- Conversations
-- ============================================================
-- project_id uses the same composite FK as session_submissions and
-- pi_session_logs so a conversation can never point at another tenant's
-- project. Deletion is a soft archive, matching tenants/users.
CREATE TABLE IF NOT EXISTS web_conversations (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id),
    user_id     TEXT NOT NULL REFERENCES users(id),
    project_id  TEXT,
    title       TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',
    provider_id TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ,
    CONSTRAINT web_conversations_tenant_project_fk
        FOREIGN KEY (tenant_id, project_id) REFERENCES memory_projects(tenant_id, id)
);

CREATE INDEX IF NOT EXISTS idx_web_conversations_user
    ON web_conversations(user_id, updated_at DESC)
    WHERE archived_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_web_conversations_project
    ON web_conversations(project_id, updated_at DESC)
    WHERE project_id IS NOT NULL;

-- ============================================================
-- Messages
-- ============================================================
-- `content` holds the normalized block list (text plus attachment references),
-- not raw provider JSON, so one shape serves OpenAI, DeepSeek and Anthropic.
-- request_id joins to usage_events for per-message cost.
CREATE TABLE IF NOT EXISTS web_messages (
    id               TEXT PRIMARY KEY,
    conversation_id  TEXT NOT NULL REFERENCES web_conversations(id) ON DELETE CASCADE,
    seq              INTEGER NOT NULL,
    role             TEXT NOT NULL,
    content          JSONB NOT NULL DEFAULT '[]',
    model            TEXT,
    request_id       TEXT,
    input_tokens     BIGINT NOT NULL DEFAULT 0,
    output_tokens    BIGINT NOT NULL DEFAULT 0,
    cost_total_cents NUMERIC(18,6) NOT NULL DEFAULT 0,
    annotations      JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_web_messages_conversation
    ON web_messages(conversation_id, seq);

CREATE INDEX IF NOT EXISTS idx_web_messages_request
    ON web_messages(request_id)
    WHERE request_id IS NOT NULL;

-- ============================================================
-- Attachments
-- ============================================================
-- Bytes live in GCS; only metadata lives here. Rows start `pending` when the
-- signed upload URL is issued and become `ready` once the object is verified,
-- so an abandoned upload is distinguishable from a usable one and sweepable.
CREATE TABLE IF NOT EXISTS web_attachments (
    id              TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL REFERENCES tenants(id),
    user_id         TEXT NOT NULL REFERENCES users(id),
    conversation_id TEXT REFERENCES web_conversations(id) ON DELETE CASCADE,
    message_id      TEXT REFERENCES web_messages(id) ON DELETE SET NULL,
    filename        TEXT NOT NULL,
    mime_type       TEXT NOT NULL,
    kind            TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    sha256          TEXT,
    bucket          TEXT NOT NULL,
    object_key      TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending',
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finalized_at    TIMESTAMPTZ,
    UNIQUE (bucket, object_key)
);

CREATE INDEX IF NOT EXISTS idx_web_attachments_conversation
    ON web_attachments(conversation_id)
    WHERE conversation_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_web_attachments_expiry
    ON web_attachments(expires_at)
    WHERE expires_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_web_attachments_pending
    ON web_attachments(created_at)
    WHERE status = 'pending';

INSERT INTO schema_migrations (version, applied_at)
VALUES (26, now())
ON CONFLICT (version) DO NOTHING;

COMMIT;

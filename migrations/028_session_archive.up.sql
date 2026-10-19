-- Session archive: session metadata and per-request turn summaries stay in
-- Postgres; raw capture bodies, derived transcripts, and Pi session exports
-- move to object storage (GCS in production, session_objects otherwise).
-- See SESSION_ARCHIVE_PLAN.md. Every ALTER below is metadata-only (nullable or
-- constant-default columns, DROP NOT NULL), so it is safe on the large
-- conversation_payloads table.

-- Postgres object backend. Used when no bucket is configured so dev, tests,
-- and single-host installs work without GCS.
CREATE TABLE IF NOT EXISTS session_objects (
    key          TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL,
    data         BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per captured conversation. Display fields are maintained by the
-- transcript builder; the builder-state columns let it append a per-request
-- delta without reading earlier bodies.
CREATE TABLE IF NOT EXISTS session_transcripts (
    id                 TEXT PRIMARY KEY,
    tenant_id          TEXT REFERENCES tenants(id),
    user_id            TEXT REFERENCES users(id),
    key_id             TEXT REFERENCES api_keys(id),
    conversation_id    TEXT NOT NULL,
    provider           TEXT NOT NULL DEFAULT '',
    model              TEXT NOT NULL DEFAULT '',
    api                TEXT NOT NULL DEFAULT '',
    opening_prompt     TEXT,
    last_prompt        TEXT,
    request_count      INTEGER NOT NULL DEFAULT 0,
    user_turn_count    INTEGER NOT NULL DEFAULT 0,
    tool_call_count    INTEGER NOT NULL DEFAULT 0,
    error_count        INTEGER NOT NULL DEFAULT 0,
    reset_count        INTEGER NOT NULL DEFAULT 0,
    started_at         TIMESTAMPTZ NOT NULL,
    last_activity_at   TIMESTAMPTZ NOT NULL,
    generation         INTEGER NOT NULL DEFAULT 1,
    line_count         INTEGER NOT NULL DEFAULT 0,
    line_hashes        BYTEA NOT NULL DEFAULT ''::bytea,
    system_sha         TEXT NOT NULL DEFAULT '',
    last_seq           INTEGER NOT NULL DEFAULT 0,
    merged_through_seq INTEGER NOT NULL DEFAULT 0,
    merged_key         TEXT,
    merged_bytes       BIGINT NOT NULL DEFAULT 0,
    segment_bytes      BIGINT NOT NULL DEFAULT 0,
    last_output_count  INTEGER NOT NULL DEFAULT 0,
    last_payload_at    TIMESTAMPTZ,
    last_request_id    TEXT,
    objects_location   TEXT NOT NULL DEFAULT '',
    needs_rebuild      BOOLEAN NOT NULL DEFAULT false,
    rebuild_requested_at TIMESTAMPTZ,
    last_error         TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_session_transcripts_tenant_conversation
    ON session_transcripts ((COALESCE(tenant_id, '')), conversation_id);
CREATE INDEX IF NOT EXISTS idx_session_transcripts_conversation
    ON session_transcripts (conversation_id, last_activity_at DESC);
CREATE INDEX IF NOT EXISTS idx_session_transcripts_activity
    ON session_transcripts (last_activity_at DESC);
CREATE INDEX IF NOT EXISTS idx_session_transcripts_rebuild
    ON session_transcripts (updated_at) WHERE needs_rebuild;

-- One row per captured request within a transcript generation. Enough to
-- render the session timeline without touching object storage.
CREATE TABLE IF NOT EXISTS session_turns (
    transcript_id  TEXT NOT NULL REFERENCES session_transcripts(id) ON DELETE CASCADE,
    generation     INTEGER NOT NULL,
    seq            INTEGER NOT NULL,
    request_id     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL,
    provider       TEXT NOT NULL,
    model          TEXT NOT NULL,
    status         TEXT NOT NULL,
    kind           TEXT NOT NULL,
    base           INTEGER NOT NULL,
    input_count    INTEGER NOT NULL,
    delta_count    INTEGER NOT NULL,
    output_count   INTEGER NOT NULL,
    system_changed BOOLEAN NOT NULL DEFAULT false,
    prompt_preview TEXT,
    tool_names     TEXT[] NOT NULL DEFAULT '{}',
    segment_key    TEXT NOT NULL,
    segment_bytes  INTEGER NOT NULL,
    PRIMARY KEY (transcript_id, generation, seq)
);

CREATE INDEX IF NOT EXISTS idx_session_turns_request
    ON session_turns (request_id);

-- body_location = 'postgres' means the bytea columns hold the bodies;
-- otherwise the object keys point at them and the bytea columns may be NULL.
ALTER TABLE conversation_payloads
    ALTER COLUMN request_body_compressed DROP NOT NULL,
    ALTER COLUMN response_body_compressed DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS body_location TEXT NOT NULL DEFAULT 'postgres',
    ADD COLUMN IF NOT EXISTS request_object_key TEXT,
    ADD COLUMN IF NOT EXISTS response_object_key TEXT,
    ADD COLUMN IF NOT EXISTS request_bytes BIGINT,
    ADD COLUMN IF NOT EXISTS response_bytes BIGINT;

ALTER TABLE pi_session_logs
    ALTER COLUMN content_gzip DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS body_location TEXT NOT NULL DEFAULT 'postgres',
    ADD COLUMN IF NOT EXISTS object_key TEXT;

INSERT INTO schema_migrations (version, applied_at)
VALUES (28, now())
ON CONFLICT (version) DO NOTHING;

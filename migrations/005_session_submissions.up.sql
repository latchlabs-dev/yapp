-- Pi Coding Agent session submissions from the corp-proxy extension.
-- Each row is one Pi session sent by the extension at agent_end and/or
-- session_shutdown. Repeated submissions update the same row (keyed by
-- conversation_id) so the latest stats survive crashes.
CREATE TABLE IF NOT EXISTS session_submissions (
    id                  TEXT PRIMARY KEY,
    conversation_id     TEXT NOT NULL UNIQUE,
    session_file        TEXT,
    message_count       INTEGER NOT NULL DEFAULT 0,
    model               TEXT,
    total_tokens        BIGINT,
    total_cost_cents    DOUBLE PRECISION,
    started_at          TIMESTAMPTZ,
    ended_at            TIMESTAMPTZ,
    summary_compressed  BYTEA,                  -- gzip-compressed session summary JSON
    extension_version   TEXT,
    pi_version          TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_session_submissions_conversation
    ON session_submissions(conversation_id, created_at DESC);

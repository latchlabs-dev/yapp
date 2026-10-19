-- Capture gap markers: durable records that conversation data was dropped or
-- truncated at a point in time, so a stored session can be audited for
-- replayability ("could we re-run this against another model?").
--
-- Written by the capture pipeline's dedicated gap recorder for:
--   queue_overflow     - payload dropped because the capture queue was full
--   store_failure      - payload dequeued but its storage write failed
--   request_truncated  - stored request body cut at PROXY_CAPTURE_MAX_PAYLOAD_BYTES
--   response_truncated - stored response/SSE transcript cut at the ceiling
--
-- Rows are tiny (no bodies) and intentionally have no expiry and no FK
-- constraints: markers must survive even when the tenant/user rows are gone
-- and must never fail to insert because a referenced row vanished.
CREATE TABLE IF NOT EXISTS capture_gaps (
    id              TEXT PRIMARY KEY,
    conversation_id TEXT,
    tenant_id       TEXT,
    user_id         TEXT,
    request_id      TEXT,
    provider        TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    streaming       BOOLEAN NOT NULL DEFAULT false,
    reason          TEXT NOT NULL,
    detail          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_capture_gaps_conversation
    ON capture_gaps(conversation_id, created_at DESC)
    WHERE conversation_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_capture_gaps_tenant_created
    ON capture_gaps(tenant_id, created_at DESC)
    WHERE tenant_id IS NOT NULL;

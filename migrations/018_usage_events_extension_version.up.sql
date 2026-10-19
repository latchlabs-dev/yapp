-- Persist the client extension version on the accepted request ledger. Older
-- rows remain NULL because session metadata is only an inference, not direct
-- per-request evidence.
ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS extension_version TEXT;

ALTER TABLE usage_events
    ADD CONSTRAINT usage_events_extension_version_length
    CHECK (extension_version IS NULL OR char_length(extension_version) <= 64);

CREATE INDEX IF NOT EXISTS idx_usage_events_extension_version_started
    ON usage_events(extension_version, started_at DESC)
    WHERE extension_version IS NOT NULL;

INSERT INTO schema_migrations (version, applied_at)
VALUES (18, now())
ON CONFLICT (version) DO NOTHING;

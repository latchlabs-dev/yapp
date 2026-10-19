-- Preserve the complete successful provider-side usage response while also
-- promoting common values into queryable numeric columns. Raw headers are
-- restricted by the poller to quota/rate-limit metadata.
ALTER TABLE provider_balance_snapshots
    ADD COLUMN IF NOT EXISTS amount_limit NUMERIC,
    ADD COLUMN IF NOT EXISTS amount_used NUMERIC,
    ADD COLUMN IF NOT EXISTS amount_spent NUMERIC,
    ADD COLUMN IF NOT EXISTS normalization_version SMALLINT NOT NULL DEFAULT 1 CHECK (normalization_version > 0),
    ADD COLUMN IF NOT EXISTS normalization_error TEXT,
    ADD COLUMN IF NOT EXISTS raw_payload JSONB,
    ADD COLUMN IF NOT EXISTS raw_headers JSONB NOT NULL DEFAULT '{}'::jsonb;

INSERT INTO schema_migrations (version, applied_at)
VALUES (21, now())
ON CONFLICT (version) DO NOTHING;

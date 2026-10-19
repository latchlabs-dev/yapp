-- Timestamped provider-side credit and subscription-allowance observations.
-- The polling worker stores only normalized balance metadata; raw upstream
-- payloads and credentials are deliberately excluded.
CREATE TABLE IF NOT EXISTS provider_balance_snapshots (
    id                    BIGSERIAL PRIMARY KEY,
    provider_account_id   TEXT NOT NULL REFERENCES provider_accounts(id) ON DELETE CASCADE,
    billing_mode          TEXT NOT NULL CHECK (billing_mode IN ('subscription', 'pay_as_you_go')),
    status                TEXT NOT NULL CHECK (status IN ('success', 'error')),
    balance_kind          TEXT CHECK (balance_kind IN ('monetary', 'usage')),
    currency              TEXT,
    unit                  TEXT,
    amount_remaining      NUMERIC,
    usage_windows         JSONB NOT NULL DEFAULT '[]'::jsonb,
    source                TEXT,
    error_code            TEXT,
    error_message         TEXT,
    observed_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_provider_balance_snapshots_latest
    ON provider_balance_snapshots(provider_account_id, observed_at DESC, id DESC);

INSERT INTO schema_migrations (version, applied_at)
VALUES (20, now())
ON CONFLICT (version) DO NOTHING;

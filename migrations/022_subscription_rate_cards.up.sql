-- Daily effective pricing for fixed-price provider subscriptions.  The rate
-- card converts the subscription's shared token usage into a USD amount that
-- can flow through the normal spend-quota counters.
CREATE TABLE IF NOT EXISTS provider_subscription_rate_cards (
    id                            BIGSERIAL PRIMARY KEY,
    provider_account_id           TEXT NOT NULL REFERENCES provider_accounts(id) ON DELETE CASCADE,
    rate_date                     DATE NOT NULL,
    model                         TEXT NOT NULL DEFAULT '*',
    currency                      TEXT NOT NULL DEFAULT 'USD',
    monthly_cost_cents            NUMERIC(18,6) NOT NULL CHECK (monthly_cost_cents > 0),
    accrued_cost_cents            NUMERIC(18,6) NOT NULL CHECK (accrued_cost_cents >= 0),
    source_period_start           TIMESTAMPTZ NOT NULL,
    source_period_end             TIMESTAMPTZ NOT NULL,
    source_tokens                 BIGINT NOT NULL CHECK (source_tokens >= 0),
    usd_per_million_tokens        NUMERIC(18,9) NOT NULL CHECK (usd_per_million_tokens > 0),
    calculation_method            TEXT NOT NULL CHECK (calculation_method IN ('cycle_to_date', 'previous_rate', 'configured_fallback')),
    calculated_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_account_id, rate_date, model)
);

CREATE INDEX IF NOT EXISTS idx_provider_subscription_rate_cards_active
    ON provider_subscription_rate_cards(provider_account_id, rate_date DESC, model);

-- Durable outbox for exact subscription charges that must reach Redis after a
-- request completes. Rows are deleted only after an idempotent Redis apply.
CREATE TABLE IF NOT EXISTS quota_spend_settlements (
    id                  TEXT PRIMARY KEY,
    scope_type          TEXT NOT NULL,
    scope_id            TEXT NOT NULL,
    counter_key         TEXT NOT NULL,
    limit_type          TEXT NOT NULL CHECK (limit_type IN ('daily_spend_cents', 'monthly_spend_cents')),
    amount_millicents   BIGINT NOT NULL CHECK (amount_millicents > 0),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_quota_spend_settlements_scope
    ON quota_spend_settlements(scope_type, scope_id, created_at);

ALTER TABLE usage_events
    ADD COLUMN IF NOT EXISTS provider_account_id TEXT REFERENCES provider_accounts(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS subscription_rate_card_id BIGINT REFERENCES provider_subscription_rate_cards(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS cost_basis TEXT NOT NULL DEFAULT 'default_model_rate'
        CHECK (cost_basis IN ('metered', 'subscription_rate_card', 'default_model_rate'));

CREATE INDEX IF NOT EXISTS idx_usage_events_provider_account_started
    ON usage_events(provider_account_id, started_at);

-- Existing rows are attributable only when exactly one active account uses
-- the recorded provider protocol.  Ambiguous history deliberately stays NULL.
WITH unique_accounts AS (
    SELECT provider, MIN(id) AS id
    FROM provider_accounts
    WHERE status = 'active'
    GROUP BY provider
    HAVING COUNT(*) = 1
)
UPDATE usage_events ue
SET provider_account_id = ua.id
FROM unique_accounts ua
WHERE ue.provider_account_id IS NULL
  AND ue.upstream_provider = ua.provider;

WITH unique_codex AS (
    SELECT MIN(id) AS id
    FROM provider_accounts
    WHERE status = 'active'
      AND provider IN ('openai-codex', 'openai-codex-responses')
    HAVING COUNT(*) = 1
)
UPDATE usage_events ue
SET provider_account_id = uc.id
FROM unique_codex uc
WHERE ue.provider_account_id IS NULL
  AND ue.upstream_provider IN ('openai-codex', 'openai-codex-responses');

-- Codex is the first fixed-price subscription.  Preserve any non-zero amount
-- already configured by an administrator.
UPDATE provider_accounts
SET config = config || jsonb_build_object(
        'billing', COALESCE(config->'billing', '{}'::jsonb) ||
            jsonb_build_object(
                'mode', 'subscription',
                'currency', 'USD',
                'monthly_cost_cents', 20000
            )
    ),
    updated_at = now()
WHERE provider IN ('openai-codex', 'openai-codex-responses')
  AND CASE WHEN jsonb_typeof(config->'billing'->'monthly_cost_cents') = 'number'
           THEN (config->'billing'->>'monthly_cost_cents')::numeric ELSE 0 END <= 0;

INSERT INTO schema_migrations (version, applied_at)
VALUES (22, now())
ON CONFLICT (version) DO NOTHING;

-- Subscription rate cards are based on observed provider allowance movement,
-- not calendar-prorated subscription cost. Provider-side utilization includes
-- traffic sent outside PiProxy, so the effective rate can allocate that cost
-- across the proxied tokens seen during the same observation interval.
ALTER TABLE provider_subscription_rate_cards
    ADD COLUMN IF NOT EXISTS source_usage_percent NUMERIC(9,6) NOT NULL DEFAULT 0
        CHECK (source_usage_percent >= 0),
    ADD COLUMN IF NOT EXISTS source_usage_window TEXT,
    ADD COLUMN IF NOT EXISTS source_usage_window_seconds BIGINT NOT NULL DEFAULT 0
        CHECK (source_usage_window_seconds >= 0),
    ADD COLUMN IF NOT EXISTS source_usage_window_resets_at TIMESTAMPTZ;

ALTER TABLE quota_spend_settlements
    ADD COLUMN IF NOT EXISTS operation TEXT NOT NULL DEFAULT 'add'
        CHECK (operation IN ('add', 'ensure_minimum'));

ALTER TABLE provider_subscription_rate_cards
    DROP CONSTRAINT IF EXISTS provider_subscription_rate_cards_calculation_method_check;

ALTER TABLE provider_subscription_rate_cards
    ADD CONSTRAINT provider_subscription_rate_cards_calculation_method_check
        CHECK (calculation_method IN ('cycle_to_date', 'utilization_delta', 'previous_rate', 'configured_fallback'));

INSERT INTO schema_migrations (version, applied_at)
VALUES (23, now())
ON CONFLICT (version) DO NOTHING;

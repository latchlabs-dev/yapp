UPDATE provider_subscription_rate_cards
SET calculation_method = 'previous_rate'
WHERE calculation_method = 'utilization_delta';

ALTER TABLE provider_subscription_rate_cards
    DROP CONSTRAINT IF EXISTS provider_subscription_rate_cards_calculation_method_check;

ALTER TABLE provider_subscription_rate_cards
    ADD CONSTRAINT provider_subscription_rate_cards_calculation_method_check
        CHECK (calculation_method IN ('cycle_to_date', 'previous_rate', 'configured_fallback'));

ALTER TABLE provider_subscription_rate_cards
    DROP COLUMN IF EXISTS source_usage_window_resets_at,
    DROP COLUMN IF EXISTS source_usage_window_seconds,
    DROP COLUMN IF EXISTS source_usage_window,
    DROP COLUMN IF EXISTS source_usage_percent;

ALTER TABLE quota_spend_settlements
    DROP COLUMN IF EXISTS operation;

DELETE FROM schema_migrations WHERE version = 23;

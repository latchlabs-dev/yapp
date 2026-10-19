ALTER TABLE usage_events
    DROP COLUMN IF EXISTS cost_basis,
    DROP COLUMN IF EXISTS subscription_rate_card_id,
    DROP COLUMN IF EXISTS provider_account_id;

DROP TABLE IF EXISTS quota_spend_settlements;
DROP TABLE IF EXISTS provider_subscription_rate_cards;

DELETE FROM schema_migrations WHERE version = 22;

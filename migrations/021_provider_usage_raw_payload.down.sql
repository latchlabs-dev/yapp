ALTER TABLE provider_balance_snapshots
    DROP COLUMN IF EXISTS raw_headers,
    DROP COLUMN IF EXISTS raw_payload,
    DROP COLUMN IF EXISTS normalization_error,
    DROP COLUMN IF EXISTS normalization_version,
    DROP COLUMN IF EXISTS amount_spent,
    DROP COLUMN IF EXISTS amount_used,
    DROP COLUMN IF EXISTS amount_limit;

DELETE FROM schema_migrations WHERE version = 21;

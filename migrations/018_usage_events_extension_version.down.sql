DROP INDEX IF EXISTS idx_usage_events_extension_version_started;
ALTER TABLE usage_events DROP CONSTRAINT IF EXISTS usage_events_extension_version_length;
ALTER TABLE usage_events DROP COLUMN IF EXISTS extension_version;
DELETE FROM schema_migrations WHERE version = 18;

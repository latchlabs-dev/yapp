DROP INDEX IF EXISTS idx_usage_events_client_ip;
ALTER TABLE usage_events DROP COLUMN IF EXISTS client_ip;

-- Migration 002: Allow unauthenticated/degraded usage events

BEGIN;

ALTER TABLE usage_events ALTER COLUMN tenant_id DROP NOT NULL;
ALTER TABLE usage_events ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE usage_events ALTER COLUMN key_id DROP NOT NULL;

INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT DO NOTHING;

COMMIT;

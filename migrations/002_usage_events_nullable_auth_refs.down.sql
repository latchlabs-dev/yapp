-- Migration 002: Restore required usage event auth references

BEGIN;

DELETE FROM usage_events
WHERE tenant_id IS NULL OR user_id IS NULL OR key_id IS NULL;

ALTER TABLE usage_events ALTER COLUMN tenant_id SET NOT NULL;
ALTER TABLE usage_events ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE usage_events ALTER COLUMN key_id SET NOT NULL;

DELETE FROM schema_migrations WHERE version = 2;

COMMIT;

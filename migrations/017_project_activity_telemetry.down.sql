DROP INDEX IF EXISTS idx_extension_errors_tenant_last_seen;
DROP INDEX IF EXISTS idx_extension_errors_last_seen;
DROP TABLE IF EXISTS extension_errors;

DROP INDEX IF EXISTS idx_usage_events_project_started;
ALTER TABLE usage_events DROP CONSTRAINT IF EXISTS usage_events_tenant_project_fk;
ALTER TABLE usage_events DROP COLUMN IF EXISTS project_id;

UPDATE api_keys
   SET scopes = array_remove(scopes, 'sessions:submit')
 WHERE id IN (SELECT key_id FROM migration_017_session_submit_grants);

DROP TABLE IF EXISTS migration_017_session_submit_grants;
DELETE FROM schema_migrations WHERE version = 17;

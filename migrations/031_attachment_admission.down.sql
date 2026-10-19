BEGIN;
DROP INDEX IF EXISTS idx_web_attachments_tenant_owner;
ALTER TABLE web_attachments DROP COLUMN upload_expires_at;
DELETE FROM schema_migrations WHERE version = 31;
COMMIT;

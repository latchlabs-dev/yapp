DROP INDEX IF EXISTS idx_pi_session_logs_uploaded_at;
DROP INDEX IF EXISTS idx_session_submissions_tenant_activity;
DROP INDEX IF EXISTS idx_session_submissions_owner_conversation;

ALTER TABLE session_submissions
    DROP COLUMN IF EXISTS owner_id,
    DROP COLUMN IF EXISTS key_id,
    DROP COLUMN IF EXISTS user_id,
    DROP COLUMN IF EXISTS tenant_id;

ALTER TABLE session_submissions
    ADD CONSTRAINT session_submissions_conversation_id_key UNIQUE (conversation_id);

ALTER TABLE tenants
    DROP COLUMN IF EXISTS session_distillation_enabled;

DELETE FROM schema_migrations WHERE version = 14;

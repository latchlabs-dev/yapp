DROP INDEX IF EXISTS idx_pi_session_logs_project_id;
DROP INDEX IF EXISTS idx_session_submissions_project_id;

ALTER TABLE pi_session_logs DROP COLUMN IF EXISTS project_id;
ALTER TABLE session_submissions DROP COLUMN IF EXISTS project_id;

DROP TABLE IF EXISTS memory_project_remotes;
DROP TABLE IF EXISTS memory_projects;

DELETE FROM schema_migrations WHERE version = 15;

DROP TABLE IF EXISTS memory_injection_rules;

DROP INDEX IF EXISTS idx_session_submissions_undistilled;

ALTER TABLE session_submissions
    DROP COLUMN IF EXISTS distill_error,
    DROP COLUMN IF EXISTS distill_attempts,
    DROP COLUMN IF EXISTS distilled_at,
    DROP COLUMN IF EXISTS project;

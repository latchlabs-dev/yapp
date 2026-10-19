ALTER TABLE session_submissions
    DROP COLUMN IF EXISTS thinking_level,
    DROP COLUMN IF EXISTS session_name,
    DROP COLUMN IF EXISTS first_prompt,
    DROP COLUMN IF EXISTS run_mode;

DELETE FROM schema_migrations WHERE version = 25;

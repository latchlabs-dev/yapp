-- Restoring NOT NULL fails by design while object-backed rows exist: their
-- bodies are not in Postgres, so rolling back would silently lose them.
ALTER TABLE pi_session_logs
    DROP COLUMN IF EXISTS object_key,
    DROP COLUMN IF EXISTS body_location,
    ALTER COLUMN content_gzip SET NOT NULL;

ALTER TABLE conversation_payloads
    DROP COLUMN IF EXISTS response_bytes,
    DROP COLUMN IF EXISTS request_bytes,
    DROP COLUMN IF EXISTS response_object_key,
    DROP COLUMN IF EXISTS request_object_key,
    DROP COLUMN IF EXISTS body_location,
    ALTER COLUMN response_body_compressed SET NOT NULL,
    ALTER COLUMN request_body_compressed SET NOT NULL;

DROP TABLE IF EXISTS session_turns;
DROP TABLE IF EXISTS session_transcripts;
DROP TABLE IF EXISTS session_objects;

DELETE FROM schema_migrations WHERE version = 28;

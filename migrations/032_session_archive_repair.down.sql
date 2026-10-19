DROP INDEX IF EXISTS idx_session_turns_request_coverage;
DROP INDEX IF EXISTS idx_session_transcripts_repair_queue;
ALTER TABLE session_transcripts
 DROP COLUMN repair_lease_token,
 DROP COLUMN repair_lease_expires_at,
 DROP COLUMN repair_retry_at,
 DROP COLUMN repair_attempts;

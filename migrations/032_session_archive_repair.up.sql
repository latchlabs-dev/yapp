-- Transcript state is the durable repair queue. Leases bound one replica's work;
-- retries persist across process restarts and never discard source captures.
ALTER TABLE session_transcripts
 ADD COLUMN repair_lease_token TEXT,
 ADD COLUMN repair_lease_expires_at TIMESTAMPTZ,
 ADD COLUMN repair_retry_at TIMESTAMPTZ,
 ADD COLUMN repair_attempts INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_session_transcripts_repair_queue
 ON session_transcripts (repair_retry_at, rebuild_requested_at) WHERE needs_rebuild;
CREATE INDEX idx_session_turns_request_coverage
 ON session_turns (transcript_id, generation, request_id);

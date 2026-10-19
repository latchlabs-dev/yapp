-- Short leases serialize browser turns without retaining connections through
-- PgBouncer. Tokens fence stale workers after recovery; row counters allocate
-- message sequence numbers atomically under concurrent appends.
BEGIN;

ALTER TABLE web_conversations
    ADD COLUMN next_message_seq BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN turn_token TEXT,
    ADD COLUMN turn_expires_at TIMESTAMPTZ,
    ADD CONSTRAINT web_conversations_turn_lease_pair
        CHECK ((turn_token IS NULL) = (turn_expires_at IS NULL));

UPDATE web_conversations c
   SET next_message_seq = COALESCE((SELECT MAX(m.seq)::bigint + 1 FROM web_messages m WHERE m.conversation_id = c.id), 0);

-- Keep the counter current while the old binary is still deployed after
-- migration, or if it is rolled back. New binaries allocate before INSERT,
-- so this WHERE skips their already-accounted messages. Console turns must
-- still be drained for cutover: old binaries do not understand turn leases.
CREATE FUNCTION web_messages_track_sequence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE web_conversations
       SET next_message_seq = NEW.seq::bigint + 1
     WHERE id = NEW.conversation_id AND next_message_seq <= NEW.seq;
    RETURN NEW;
END;
$$;

CREATE TRIGGER web_messages_track_sequence
AFTER INSERT ON web_messages FOR EACH ROW
EXECUTE FUNCTION web_messages_track_sequence();

INSERT INTO schema_migrations (version, applied_at) VALUES (30, now())
ON CONFLICT (version) DO NOTHING;
COMMIT;

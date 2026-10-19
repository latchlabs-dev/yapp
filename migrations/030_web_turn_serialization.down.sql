BEGIN;
DROP TRIGGER web_messages_track_sequence ON web_messages;
DROP FUNCTION web_messages_track_sequence();
ALTER TABLE web_conversations
    DROP CONSTRAINT web_conversations_turn_lease_pair,
    DROP COLUMN turn_expires_at,
    DROP COLUMN turn_token,
    DROP COLUMN next_message_seq;
DELETE FROM schema_migrations WHERE version = 30;
COMMIT;

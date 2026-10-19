-- Rollback for 026_web_console.
--
-- Dropped in dependency order: attachments and messages reference
-- conversations, sessions reference identities.

BEGIN;

DROP TABLE IF EXISTS web_attachments;
DROP TABLE IF EXISTS web_messages;
DROP TABLE IF EXISTS web_conversations;
DROP TABLE IF EXISTS web_sessions;
DROP TABLE IF EXISTS web_identities;
DROP TABLE IF EXISTS web_identity_domains;

DELETE FROM schema_migrations WHERE version = 26;

COMMIT;

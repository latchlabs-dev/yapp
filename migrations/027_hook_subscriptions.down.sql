-- Rollback for 027_hook_subscriptions.

BEGIN;

DROP TABLE IF EXISTS hook_deliveries;
DROP TABLE IF EXISTS hook_subscriptions;

DELETE FROM schema_migrations WHERE version = 27;

COMMIT;

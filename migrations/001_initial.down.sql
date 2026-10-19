-- Migration 001: Rollback

BEGIN;

DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS usage_events;
DROP TABLE IF EXISTS quota_policies;
DROP TABLE IF EXISTS model_aliases;
DROP TABLE IF EXISTS provider_credentials;
DROP TABLE IF EXISTS provider_accounts;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS tenants;
DROP TABLE IF EXISTS schema_migrations;

COMMIT;

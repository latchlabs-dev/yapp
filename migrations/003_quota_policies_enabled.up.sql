-- The quota_policies store (internal/store/postgres/quota_policies.go) reads and
-- writes an `enabled` boolean, but 001_initial created the table with a `status`
-- text column instead. Add the column and backfill from the existing status so
-- quota policy create/update works. Safe to run on already-initialized databases.
ALTER TABLE quota_policies ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT true;
UPDATE quota_policies SET enabled = (status = 'active');

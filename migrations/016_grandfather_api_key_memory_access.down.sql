-- Memory access was intentionally grandfathered. A rollback must not silently
-- revoke scopes that may now be in active use or that an administrator may
-- have granted independently.
DELETE FROM schema_migrations WHERE version = 16;

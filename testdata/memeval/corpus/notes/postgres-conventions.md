# Postgres conventions

- pgx/v5 pool wrapped in internal/store/postgres.Pool. No ORM.
- Migrations live in migrations/NNN_name.up.sql / .down.sql, applied manually with
  make migrate-up DSN=... (golang-migrate). Always write the down migration.
- Use ON CONFLICT upserts with COALESCE(EXCLUDED.col, table.col) to merge partial updates.
- Claim work queues with FOR UPDATE SKIP LOCKED inside an UPDATE ... WHERE id IN (SELECT ...).
- nullableText() converts empty strings to NULL on insert.

-- Durable nightly queue for one server-side memory synthesis task per session.
-- The shared Absurd schema/functions are installed by migration 011.
select absurd.create_queue('memory_distillation');

INSERT INTO schema_migrations (version, applied_at)
VALUES (12, now())
ON CONFLICT (version) DO NOTHING;

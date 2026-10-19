-- Financial adjustment history is deliberately retained. Disable the observed
-- allocator before rolling back application binaries; do not erase its audit.
DO $$ BEGIN RAISE EXCEPTION 'Subscription allocation migration requires a reviewed financial rollback; automatic downgrade is disabled'; END $$;

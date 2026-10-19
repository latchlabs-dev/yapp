DO $$
BEGIN
    IF to_regprocedure('absurd.drop_queue(text)') IS NOT NULL THEN
        PERFORM absurd.drop_queue('memory_compaction');
    END IF;
END;
$$;

-- Leave the shared absurd schema installed; it can contain other queues.

DO $$
BEGIN
    IF to_regprocedure('absurd.drop_queue(text)') IS NOT NULL THEN
        PERFORM absurd.drop_queue('memory_distillation');
    END IF;
END;
$$;

DELETE FROM schema_migrations WHERE version = 12;

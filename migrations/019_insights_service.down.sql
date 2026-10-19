DELETE FROM schema_migrations WHERE version = 19;
DROP TABLE IF EXISTS insights;
DROP TABLE IF EXISTS insight_runs;
DROP TABLE IF EXISTS insight_tags;
DROP TABLE IF EXISTS insight_configurations;
DROP TABLE IF EXISTS insight_processors;

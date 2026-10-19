-- Independently deployed insight processors and their UI-managed policies.
-- PiProxy owns parsed transcripts, attribution, durable IDs, tag registry,
-- runs, and results; processors are stateless gRPC workers.
CREATE TABLE IF NOT EXISTS insight_processors (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    grpc_target         TEXT NOT NULL,
    plaintext           BOOLEAN NOT NULL DEFAULT false,
    tls_server_name     TEXT,
    timeout_ms          INTEGER NOT NULL DEFAULT 120000 CHECK (timeout_ms BETWEEN 1000 AND 900000),
    status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS insight_configurations (
    id                          TEXT PRIMARY KEY,
    processor_id                TEXT NOT NULL REFERENCES insight_processors(id),
    name                        TEXT NOT NULL,
    tenant_id                   TEXT REFERENCES tenants(id),
    project_id                  TEXT,
    instructions                TEXT NOT NULL DEFAULT '',
    allow_new_tags              BOOLEAN NOT NULL DEFAULT true,
    include_session_summary     BOOLEAN NOT NULL DEFAULT true,
    include_insight_summaries   BOOLEAN NOT NULL DEFAULT true,
    max_input_items             INTEGER NOT NULL DEFAULT 1000 CHECK (max_input_items BETWEEN 1 AND 10000),
    max_input_chars             INTEGER NOT NULL DEFAULT 500000 CHECK (max_input_chars BETWEEN 1000 AND 5000000),
    revision                    INTEGER NOT NULL DEFAULT 1,
    status                      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (project_id IS NULL OR tenant_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_insight_configurations_match
    ON insight_configurations(status, tenant_id, project_id);

CREATE TABLE IF NOT EXISTS insight_tags (
    name                TEXT PRIMARY KEY,
    description         TEXT NOT NULL DEFAULT '',
    usage_count         BIGINT NOT NULL DEFAULT 0 CHECK (usage_count >= 0),
    status              TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO insight_tags (name, description) VALUES
    ('context', 'Durable project or operational context'),
    ('mistake', 'A failure mode or action to avoid repeating'),
    ('lesson', 'A reusable learning or effective technique'),
    ('decision', 'A durable decision and its relevant rationale')
ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS insight_runs (
    id                      TEXT PRIMARY KEY,
    configuration_id        TEXT NOT NULL REFERENCES insight_configurations(id),
    configuration_revision  INTEGER NOT NULL,
    session_submission_id   TEXT NOT NULL REFERENCES session_submissions(id),
    tenant_id               TEXT NOT NULL REFERENCES tenants(id),
    owner_id                TEXT NOT NULL,
    conversation_id         TEXT NOT NULL,
    project_id              TEXT NOT NULL,
    input_sha256            TEXT NOT NULL,
    status                  TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running', 'completed', 'failed')),
    session_summary         TEXT NOT NULL DEFAULT '',
    usage                   JSONB NOT NULL DEFAULT '{}'::jsonb,
    warnings                JSONB NOT NULL DEFAULT '[]'::jsonb,
    error                   TEXT,
    started_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at            TIMESTAMPTZ,
    UNIQUE (configuration_id, configuration_revision, session_submission_id, input_sha256)
);

CREATE INDEX IF NOT EXISTS idx_insight_runs_recent
    ON insight_runs(started_at DESC);
CREATE INDEX IF NOT EXISTS idx_insight_runs_session
    ON insight_runs(tenant_id, owner_id, conversation_id);

CREATE TABLE IF NOT EXISTS insights (
    id                  TEXT PRIMARY KEY,
    run_id              TEXT NOT NULL REFERENCES insight_runs(id) ON DELETE CASCADE,
    summary             TEXT NOT NULL DEFAULT '',
    content             TEXT NOT NULL,
    tags                TEXT[] NOT NULL DEFAULT '{}',
    source_item_ids     TEXT[] NOT NULL DEFAULT '{}',
    metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_insights_run ON insights(run_id);
CREATE INDEX IF NOT EXISTS idx_insights_tags ON insights USING GIN(tags);

INSERT INTO schema_migrations (version, applied_at)
VALUES (19, now())
ON CONFLICT (version) DO NOTHING;

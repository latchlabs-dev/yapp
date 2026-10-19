-- Project memory & distillation state.
--
-- session_submissions gains a project identifier (sent by the corp-proxy
-- extension) and distillation bookkeeping so the proxy-side distiller sweeps
-- each ended session exactly once.
ALTER TABLE session_submissions
    ADD COLUMN IF NOT EXISTS project TEXT,
    ADD COLUMN IF NOT EXISTS distilled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS distill_attempts INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS distill_error TEXT;

CREATE INDEX IF NOT EXISTS idx_session_submissions_undistilled
    ON session_submissions (ended_at)
    WHERE distilled_at IS NULL;

-- Dynamic context-injection rules, evaluated by the proxy's injection hook
-- chain on every /proxy/v1/context request. Most-specific matching rule wins:
-- project+user > project+tenant > project > user > tenant > global.
-- A NULL column matches anything.
CREATE TABLE IF NOT EXISTS memory_injection_rules (
    id            TEXT PRIMARY KEY,
    project       TEXT,            -- sanitized project name, NULL = any
    tenant_id     TEXT,            -- NULL = any
    user_id       TEXT,            -- NULL = any
    enabled       BOOLEAN NOT NULL DEFAULT true,  -- inject context when matched?
    budget_tokens INTEGER,         -- optional bundle budget override
    note          TEXT,            -- operator note (why this rule exists)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_memory_injection_rules_project
    ON memory_injection_rules (project);

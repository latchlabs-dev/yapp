-- Canonical, tenant-scoped repository identities for shared project memory.
CREATE TABLE IF NOT EXISTS memory_projects (
    id                  TEXT PRIMARY KEY,
    tenant_id           TEXT NOT NULL REFERENCES tenants(id),
    slug                TEXT NOT NULL,
    display_name        TEXT NOT NULL,
    canonical_repo_key  TEXT,
    status              TEXT NOT NULL DEFAULT 'active',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_projects_tenant_repo
    ON memory_projects(tenant_id, canonical_repo_key)
    WHERE canonical_repo_key IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_memory_projects_tenant_slug
    ON memory_projects(tenant_id, slug);

CREATE TABLE IF NOT EXISTS memory_project_remotes (
    project_id          TEXT NOT NULL,
    tenant_id           TEXT NOT NULL REFERENCES tenants(id),
    normalized_remote   TEXT NOT NULL,
    remote_kind         TEXT NOT NULL DEFAULT 'observed',
    first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, normalized_remote),
    FOREIGN KEY (tenant_id, project_id)
        REFERENCES memory_projects(tenant_id, id) ON DELETE CASCADE
);

ALTER TABLE session_submissions
    ADD COLUMN IF NOT EXISTS project_id TEXT;

ALTER TABLE pi_session_logs
    ADD COLUMN IF NOT EXISTS project_id TEXT;

ALTER TABLE session_submissions
    ADD CONSTRAINT session_submissions_tenant_project_fk
    FOREIGN KEY (tenant_id, project_id) REFERENCES memory_projects(tenant_id, id);

ALTER TABLE pi_session_logs
    ADD CONSTRAINT pi_session_logs_tenant_project_fk
    FOREIGN KEY (tenant_id, project_id) REFERENCES memory_projects(tenant_id, id);

-- Preserve existing behavior while giving historical rows a stable identity.
-- These legacy projects have no canonical remote yet; the first validated
-- repository sighting with the same tenant/slug upgrades the row in place.
INSERT INTO memory_projects (id, tenant_id, slug, display_name)
SELECT 'mprj_' || substr(md5(tenant_id || ':' || project), 1, 26),
       tenant_id, project, project
  FROM session_submissions
 WHERE tenant_id IS NOT NULL AND project IS NOT NULL AND project <> ''
 GROUP BY tenant_id, project
ON CONFLICT (id) DO NOTHING;

UPDATE session_submissions ss
   SET project_id = mp.id
  FROM memory_projects mp
 WHERE ss.project_id IS NULL
   AND ss.tenant_id = mp.tenant_id
   AND ss.project = mp.slug;

UPDATE pi_session_logs pl
   SET project_id = mp.id
  FROM memory_projects mp
 WHERE pl.project_id IS NULL
   AND pl.tenant_id = mp.tenant_id
   AND pl.project = mp.slug;

CREATE INDEX IF NOT EXISTS idx_session_submissions_project_id
    ON session_submissions(project_id, ended_at DESC)
    WHERE project_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_pi_session_logs_project_id
    ON pi_session_logs(project_id, session_modified_at DESC)
    WHERE project_id IS NOT NULL;

INSERT INTO schema_migrations (version, applied_at)
VALUES (15, now())
ON CONFLICT (version) DO NOTHING;

-- Bind session metadata and distillation consent to a tenant/owner boundary.
-- Historical rows are backfilled from their latest captured provider request;
-- rows that cannot be correlated remain readable to admins but are not eligible
-- for distillation.
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS session_distillation_enabled BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE session_submissions
    ADD COLUMN IF NOT EXISTS tenant_id TEXT REFERENCES tenants(id),
    ADD COLUMN IF NOT EXISTS user_id TEXT REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS key_id TEXT REFERENCES api_keys(id),
    ADD COLUMN IF NOT EXISTS owner_id TEXT;

WITH unambiguous_owner AS (
    SELECT conversation_id,
           MIN(tenant_id) AS tenant_id,
           MIN(user_id) FILTER (WHERE user_id IS NOT NULL AND user_id <> '') AS user_id,
           MIN(key_id) FILTER (WHERE key_id IS NOT NULL AND key_id <> '') AS key_id
      FROM conversation_payloads
     WHERE conversation_id IS NOT NULL
       AND conversation_id <> ''
       AND tenant_id IS NOT NULL
     GROUP BY conversation_id
    HAVING COUNT(DISTINCT tenant_id) = 1
       AND COUNT(DISTINCT CASE
               WHEN user_id IS NOT NULL AND user_id <> '' THEN 'user:' || user_id
               WHEN key_id IS NOT NULL AND key_id <> '' THEN 'key:' || key_id
               ELSE NULL
           END) = 1
)
UPDATE session_submissions ss
   SET tenant_id = uo.tenant_id,
       user_id = uo.user_id,
       key_id = uo.key_id,
       owner_id = CASE
           WHEN uo.user_id IS NOT NULL AND uo.user_id <> '' THEN 'user:' || uo.user_id
           WHEN uo.key_id IS NOT NULL AND uo.key_id <> '' THEN 'key:' || uo.key_id
           ELSE NULL
       END
  FROM unambiguous_owner uo
 WHERE uo.conversation_id = ss.conversation_id
   AND ss.tenant_id IS NULL;

ALTER TABLE session_submissions
    DROP CONSTRAINT IF EXISTS session_submissions_conversation_id_key;

CREATE UNIQUE INDEX IF NOT EXISTS idx_session_submissions_owner_conversation
    ON session_submissions(tenant_id, owner_id, conversation_id)
    WHERE tenant_id IS NOT NULL AND owner_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_session_submissions_tenant_activity
    ON session_submissions(tenant_id, ended_at DESC);

CREATE INDEX IF NOT EXISTS idx_pi_session_logs_uploaded_at
    ON pi_session_logs(uploaded_at);

INSERT INTO schema_migrations (version, applied_at)
VALUES (14, now())
ON CONFLICT (version) DO NOTHING;

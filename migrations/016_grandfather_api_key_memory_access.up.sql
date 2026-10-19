-- Preserve project-memory access for every API key that existed when the
-- dedicated memory scopes were introduced. Future keys remain explicitly
-- configurable through the admin key-creation capabilities.
UPDATE api_keys
   SET scopes = scopes
       || CASE
              WHEN NOT ('memory:context:read' = ANY(scopes))
              THEN ARRAY['memory:context:read']::TEXT[]
              ELSE ARRAY[]::TEXT[]
          END
       || CASE
              WHEN NOT ('memory:observe' = ANY(scopes))
              THEN ARRAY['memory:observe']::TEXT[]
              ELSE ARRAY[]::TEXT[]
          END
 WHERE NOT (scopes @> ARRAY['memory:context:read', 'memory:observe']::TEXT[]);

INSERT INTO schema_migrations (version, applied_at)
VALUES (16, now())
ON CONFLICT (version) DO NOTHING;

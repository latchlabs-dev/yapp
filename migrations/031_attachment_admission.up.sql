BEGIN;

ALTER TABLE web_attachments ADD COLUMN upload_expires_at TIMESTAMPTZ;
CREATE INDEX idx_web_attachments_tenant_owner ON web_attachments(tenant_id, user_id);

INSERT INTO schema_migrations (version, applied_at) VALUES (31, now())
ON CONFLICT (version) DO NOTHING;
COMMIT;

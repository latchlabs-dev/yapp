-- Session attribution reported by the corp-proxy extension at session
-- submission time. All columns are nullable so older extension versions
-- keep working unchanged (values show as "—" in the admin UI).
ALTER TABLE session_submissions
    ADD COLUMN IF NOT EXISTS thinking_level TEXT,   -- Pi thinking level: off|minimal|low|medium|high|xhigh|max
    ADD COLUMN IF NOT EXISTS session_name  TEXT,    -- human session title set via /name (pi.setSessionName)
    ADD COLUMN IF NOT EXISTS first_prompt  TEXT,    -- truncated first user prompt (title fallback)
    ADD COLUMN IF NOT EXISTS run_mode      TEXT;    -- pi run mode: tui|rpc|json|print

INSERT INTO schema_migrations(version,applied_at) VALUES(25,now()) ON CONFLICT(version) DO NOTHING;

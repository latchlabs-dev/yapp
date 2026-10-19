-- ============================================================
-- Usage Events: client IP for abuse detection
-- ============================================================
-- Records the source IP the proxy observed for each request (post trusted-proxy
-- resolution — see ratelimit.ClientIP). INET enables efficient per-IP and
-- per-subnet abuse queries. NULL when the IP could not be determined.
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS client_ip INET;

CREATE INDEX IF NOT EXISTS idx_usage_events_client_ip ON usage_events(client_ip);

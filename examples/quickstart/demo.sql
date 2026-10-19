-- Synthetic documentation fixtures only. Run against the dedicated demo DB.
BEGIN;
INSERT INTO tenants (id, name) VALUES
 ('demo_tenant_atlas', 'Atlas Engineering'), ('demo_tenant_cedar', 'Cedar Research')
ON CONFLICT (id) DO NOTHING;
INSERT INTO users (id, tenant_id, email, display_name) VALUES
 ('demo_user_alex', 'demo_tenant_atlas', 'alex@atlas.example', 'Alex Morgan'),
 ('demo_user_sam', 'demo_tenant_atlas', 'sam@atlas.example', 'Sam Rivera'),
 ('demo_user_lee', 'demo_tenant_cedar', 'lee@cedar.example', 'Lee Chen')
ON CONFLICT (id) DO NOTHING;
INSERT INTO usage_events
 (id, request_id, tenant_id, user_id, provider, model, upstream_provider,
 upstream_model, status, input_tokens, output_tokens, total_tokens,
 cost_total_cents, started_at, completed_at, created_at)
SELECT 'demo_usage_' || n, 'demo_request_' || n,
 CASE WHEN n % 3 = 0 THEN 'demo_tenant_cedar' ELSE 'demo_tenant_atlas' END,
 CASE WHEN n % 3 = 0 THEN 'demo_user_lee' WHEN n % 3 = 1 THEN 'demo_user_alex' ELSE 'demo_user_sam' END,
 CASE WHEN n % 2 = 0 THEN 'openai' ELSE 'anthropic' END,
 CASE WHEN n % 2 = 0 THEN 'demo-chat' ELSE 'demo-reasoning' END,
 CASE WHEN n % 2 = 0 THEN 'openai' ELSE 'anthropic' END,
 CASE WHEN n % 2 = 0 THEN 'demo-chat' ELSE 'demo-reasoning' END,
 'success', 1200 + n * 17, 400 + n * 5, 1600 + n * 22,
 0.2 + n * 0.01,
 now() - (n % 14) * interval '1 day' - (n % 12) * interval '1 hour',
 now() - (n % 14) * interval '1 day' - (n % 12) * interval '1 hour' + interval '2 seconds',
 now() - (n % 14) * interval '1 day' - (n % 12) * interval '1 hour'
FROM generate_series(1, 168) AS n
ON CONFLICT (id) DO NOTHING;
COMMIT;

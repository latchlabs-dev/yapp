-- Idempotent local mock provider setup. Never run against a real deployment.
BEGIN;
INSERT INTO provider_catalogs
 (namespace, provider_key, display_name, api_type, base_url,
  public_base_path, public_api_base_path, allowed_paths)
VALUES ('quickstart', 'local-demo', 'Local demo (mock)', 'openai-completions',
 'http://mock:8080', '/openai', '/openai/v1', '{/v1/chat/completions}')
ON CONFLICT (namespace, provider_key) DO NOTHING;
INSERT INTO provider_catalog_models
 (namespace, provider_key, model_id, display_name, api_type, input,
  context_window, max_tokens, cost)
VALUES ('quickstart', 'local-demo', 'demo-chat', 'Local demo (mock)',
 'openai-completions', '{text}', 4096, 256,
 '{"input":0,"output":0,"cacheRead":0,"cacheWrite":0}')
ON CONFLICT (namespace, provider_key, model_id) DO NOTHING;
INSERT INTO provider_accounts (id, provider, name, config)
VALUES ('quickstart_mock', 'openai', 'Local demo (mock)',
 '{"catalog_namespace":"quickstart","catalog_key":"local-demo", "public_base_path":"/openai", "public_api_base_path":"/openai/v1", "upstream_base_url":"http://mock:8080", "allowed_hosts":["mock"], "allowed_paths":["/v1/chat/completions"], "allow_private_egress":true, "credential_id":"providers/demo/api-key"}')
ON CONFLICT (id) DO NOTHING;
COMMIT;

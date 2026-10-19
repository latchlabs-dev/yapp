# Synthetic deployment runbook

This evaluation fixture describes a fictional deployment, not an operator runbook.

- docker-compose.prod.yml requires PIPROXY_VERSION to be set; it errors if unset. The Makefile
  supplies it - never run docker compose by hand without it.
- PROXY_AUTH_HASH_PEPPER_REF and PROXY_CREDENTIAL_ENCRYPTION_KEY must match across proxy and
  admin nodes, or every API key is rejected after redeploy (keys are HMAC-hashed with the pepper).
- Example target: edge.example.test (arm64, loopback + nginx). Keep the gateway separate from batch.example.test.
- make deploy = buildx multi-arch build + push + SSH pull/recreate on the live edge host.
- Rollback: select an already-pushed image tag and recreate the service without rebuilding it.

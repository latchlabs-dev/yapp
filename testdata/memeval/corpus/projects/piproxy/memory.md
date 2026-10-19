# Project memory: piproxy

Repository: git@github.com:earlgrey/piproxy.git

## Conventions
- Go 1.26, env-var config only (PROXY_* vars in internal/config/config.go), no YAML.
- Standard library testing with httptest mock upstreams; no mocking frameworks.
- Migrations are manual via golang-migrate: make migrate-up. The proxy never auto-migrates.
- gofmt is enforced; CI rejects unformatted files.

## Pitfalls
- Quota reservations are per API key, not per user. Spend counters live in Redis.
- Provider routes fail closed (503) when no auth backend is configured.
- conversation_id only lands on captured payloads when the client sends X-Session-ID or metadata.

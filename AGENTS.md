# Yapp contributor runbook

This repository contains application source, tests and portable example configuration.
Instance deployment files, credentials, captured sessions and business reports belong
in an operator's private operations repository or secret store. Never add them here.

Canonical development repository: `latchlabs-dev/yapp`.

## Build and verification

- Use the Go toolchain declared in `go.mod`; module: `github.com/earlgrey/piproxy`.
- Use pnpm, frozen lockfiles, and the configured seven-day minimum dependency age.
  Do not bypass release-age restrictions. Python uses uv and the checked-in lockfile.
- `make build`, `make test`, `make lint`, and `make check-release` are the main gates.
- `make docker-up` starts the local example; see `docs/getting-started.md`.
- Keep `third_party/duffel`: it is a local Go module replacement with its own license.
- Preserve unrelated working-tree changes. Do not reset, clean or stage everything.
- Unit tests and compilation do not prove runtime behavior. Validate altered UI flows
  in a real browser against a disposable instance containing synthetic data.

## Architecture and security invariants

- Avoid import cycles: handlers may import leaf packages such as `upstream` and
  `httpapi/dataplane`; child handlers must not import parent `httpapi`.
- Chi route groups scope authentication, body limits and quotas. Health endpoints
  are public. Authenticated provider routes still need `RequireAnyScope` checks.
- Startup requires PostgreSQL, Redis and authentication. Nil test-router dependencies
  do not authorize an unauthenticated runtime mode.
- Strip client authorization, cookies and API-key headers before injecting upstream
  credentials. Missing credentials fail with `provider_unavailable`.
- Uploaded credentials are AES-256-GCM encrypted in PostgreSQL. Preserve the encryption
  key and shared auth pepper across upgrades; replacement requires a migration.
- Admin login verifies an authenticated admin endpoint, never `/healthz`.
- Match provider paths exactly unless an explicitly configured path ends in `/`.
  Preserve tests for prefix collisions and forwarded-header spoofing.
- Provider refresh replaces complete registry snapshots, including deletions.
- Model policy is opt-in via `PROXY_MODEL_GUARDS`; defaults must remain neutral.
- Pi capability `compat` flags belong on each model, not only the provider.
- Keep ULID monotonic entropy access mutex-protected. Avoid wrapping nil errors.

## Quotas, streams and capture

- Spend counters use fixed-point millicents. Reserve estimated spend and reconcile
  actual usage; rollback reservations when subsequent checks deny a request.
- Use image-aware token estimates. Do not count base64 image bytes as text tokens.
- Policies use the most restrictive matching limit. Optional request/token guardrails
  must not become implicit commercial limits. Current scope resolution selects one
  scope; changing it requires explicit multi-scope reservation semantics.
- Cap buffered upstream reads with `dataplane.ReadUpstreamBody`. Use stream-specific
  timeout contexts through forwarding and relay. Context overflow returns HTTP 400.
- Keep usage rows lean. Raw requests/responses go through asynchronous `internal/capture`;
  preserve durable gap markers and storage/backend separation.
- Capture stores compressed, unencrypted bodies. Document capture consent, retention,
  encryption-at-rest and backup requirements accurately. Never use live sessions as
  public fixtures or screenshots.

## Database and operational boundary

- Keep immutable numbered migrations. Apply all pending migrations for local tests;
  editing migration 001 does not upgrade an existing database.
- Use a direct PostgreSQL connection for session-advisory-lock migration tooling,
  not a transaction pooler. Runtime prepared statements can use a configured pooler.
- Preserve nullable usage auth references for unauthenticated/degraded records.
- Default local examples publish only loopback ports. Administration, metrics,
  PostgreSQL and Redis must remain behind an operator-controlled access boundary.
- Public scripts must never contain default production SSH targets, registry accounts,
  private domains, customer identities or credentials. Release tooling builds local
  artifacts; publication and deployment are separate explicit operations.

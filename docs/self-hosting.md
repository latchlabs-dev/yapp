# Self-hosting Yapp

The [quickstart](getting-started.md) is a local evaluation stack. A real installation needs private configuration, durable storage, and an access boundary around administration.

## Runtime layout

Run the `proxy` and `admin` binaries separately with shared PostgreSQL and Redis services. Serve the dashboard with its included nginx container; it forwards `/api` to the admin service. Expose the provider API through a TLS reverse proxy. Keep the admin API, dashboard, database, Redis, and metrics reachable only from your trusted network.

The quickstart Compose file is a starting example, not a production deployment definition. Its `bootstrap` and `mock` services install a local-only provider with private egress enabled. Omit those services, their demo credential, and their provider/catalog rows from a real installation; start production with a fresh database and migrations, not a copied quickstart volume. Keep your real Compose overrides, hostnames, registry configuration, certificates, keys, and backup settings in a separate private operations repository.

## Required configuration

| Setting | Purpose |
| --- | --- |
| `PROXY_POSTGRES_DSN` | Application database connection; same database for proxy and admin |
| `PROXY_REDIS_ADDRESS` | Shared Redis endpoint for quota and revocation state |
| `PROXY_ADMIN_KEY` | Secret used to authenticate admin API/dashboard access |
| `PROXY_AUTH_HASH_PEPPER_REF` | Nonempty secret reference enabling pepper resolution |
| `PROXY_KEY_HASH_PEPPER` | Fallback pepper; **must match on proxy and admin** |
| `PROXY_CREDENTIAL_ENCRYPTION_KEY` | 32-byte key, encoded as 64 hex characters or base64 |
| `PROXY_PUBLIC_BASE_URL` | External proxy URL used in generated onboarding instructions |

Generate independent values with `openssl rand -hex 32`. Store them in your secret manager or private deployment configuration. Replacing an encryption key without re-encrypting stored credentials makes those credentials unreadable; changing a pepper invalidates verification of existing user keys. Back up the secrets needed to restore your database.

Environment-based provider credentials use names such as `PROXY_SECRET_PROVIDERS_OPENAI_API_KEY` and `PROXY_SECRET_PROVIDERS_ANTHROPIC_API_KEY`. Admin-uploaded credentials are encrypted in PostgreSQL.

## Networking

Bind published private services to loopback or a private interface. Do not expose ports 8081 (admin) or 9090 (metrics) directly to the internet. Terminate TLS before the public API and configure connection/request limits for your traffic. If using forwarded client IPs, have the edge overwrite forwarding headers; preserve the proxy's trusted-peer checks.

PostgreSQL, Redis, and authentication dependencies are required for proxy startup. Health endpoints do not substitute for an authenticated provider request during rollout verification.

## Data and capture

Persist PostgreSQL and maintain tested backups. Redis quota state also needs an appropriate persistence and recovery policy for your budget enforcement requirements.

Conversation capture is disabled in the quickstart. When enabled, it stores request and response contents, including prompts and tool results, in compressed PostgreSQL payloads. Set retention deliberately:

```yaml
PROXY_CAPTURE_ENABLED: "true"
PROXY_CAPTURE_RETENTION_DAYS: "7"
```

Payloads are compressed, not encrypted by the capture layer. Protect database storage and backups accordingly. Retention cleanup is periodic, so rows can briefly outlive their expiration time. Capture gap markers record drops/truncations separately. Usage metadata and audit records need their own storage lifecycle policy.

Session archive workers and the optional web portal are disabled in the quickstart. Enable advanced services only after configuring their additional storage, authentication, and worker requirements.

## Migrations and upgrades

Back up the database and record your current image versions before upgrading. Apply all pending files from `migrations/` with `golang-migrate`, using a **direct PostgreSQL connection** and the `piproxy_migrations` tracking table. A transaction-pooling endpoint is unsuitable for the migration runner's session advisory lock.

The example's `migrate` service implements this for a fresh database. An existing database initialized outside this runner needs a verified baseline; do not force a migration version merely to suppress an error.

Build release images from a reviewed source revision and pin the resulting image digests in your private deployment configuration. Update admin first, verify authenticated access, then update proxy and verify readiness plus a small authenticated provider request. Reverting application images does not automatically undo schema changes; assess migration compatibility before rollback.

## Source distribution

Keep `third_party/duffel` with source distributions because `go.mod` replaces a module with that local path. Complete the project's license and third-party notice review before redistributing releases. Neither the quickstart nor this guide requires the maintainers' production infrastructure or credentials.

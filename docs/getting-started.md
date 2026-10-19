# Getting started

## Start the local stack

Install Docker with Compose v2 and `rsync`, then run from the repository root:

```sh
./examples/quickstart/build.sh
docker compose -f examples/quickstart/compose.yml up -d --wait
```

The first build downloads Go modules and dashboard dependencies.

Compose starts PostgreSQL and Redis, runs every pending migration, configures a local mock provider, then starts the proxy, admin API, and dashboard. Wait for `curl --fail http://localhost:18080/readyz` to succeed. Open http://localhost:13333 and sign in with `local-demo-admin-key`.

| Service | Local address |
| --- | --- |
| Provider API | `http://localhost:18080` |
| Admin dashboard | `http://localhost:13333` |
| Admin API | `http://localhost:13333/api` |
| PostgreSQL and Redis | Internal Docker network only |

## Try the bundled mock provider

The `demo-chat` model is served by a local nginx fixture inside this Compose project. It accepts non-streaming chat completion requests and always returns `Hello from the local Yapp demo.` It does not call an external provider.

Create a tenant, user, and key using the steps below, then use the README curl example. Or run the complete onboarding/request smoke test with Python 3:

```sh
python3 examples/quickstart/smoke-test.py
```

The test leaves a synthetic tenant and user in the local database. It verifies that invalid admin credentials and unauthenticated proxy requests are rejected; then creates a user key, discovers the model, and forwards a real request through the proxy to the mock service. The mock requires the server's demo upstream credential and rejects forwarded client cookies, so a successful response also checks credential injection and cookie stripping.

## Add a real upstream credential

To call a real model, configure the default OpenAI provider through a private Compose override. Create a file outside this repository, for example `$HOME/piproxy-local.yml`:

```yaml
services:
  proxy:
    environment:
      PROXY_SECRET_PROVIDERS_OPENAI_API_KEY: ${OPENAI_API_KEY:?Set OPENAI_API_KEY}
```

Set `OPENAI_API_KEY` in your shell without committing it, then apply the override:

```sh
docker compose -f examples/quickstart/compose.yml \
  -f "$HOME/piproxy-local.yml" up -d --wait
```

Alternatively, use the dashboard's Providers section to create an account and upload credentials. Database credentials are encrypted with `PROXY_CREDENTIAL_ENCRYPTION_KEY`. The local example supplies a demo encryption key; replace it before storing any credentials you intend to keep.

## Create a tenant, user, and API key

In **Tenants**, create a tenant, open it, add a user, then create a key for that user. Save the displayed key when it is issued. Use the generated onboarding instructions for Pi, or export the key as `PIPROXY_KEY` and use the request example in the README.

For an API-only setup, the same flow is:

```sh
curl --fail http://localhost:13333/api/tenants \
  -H 'X-Admin-Key: local-demo-admin-key' \
  -H 'Content-Type: application/json' -d '{"name":"My team"}'

# Replace TENANT_ID with the returned id.
curl --fail http://localhost:13333/api/tenants/TENANT_ID/users \
  -H 'X-Admin-Key: local-demo-admin-key' \
  -H 'Content-Type: application/json' \
  -d '{"email":"alex@example.com","display_name":"Alex"}'

# Replace USER_ID with the returned id.
curl --fail http://localhost:13333/api/tenants/TENANT_ID/users/USER_ID/keys \
  -H 'X-Admin-Key: local-demo-admin-key' \
  -H 'Content-Type: application/json' -d '{"scopes":["llm:invoke"]}'
```

Choose `demo-chat` for the local fixture or an upstream model available to your configured account. The mock supports non-streaming text requests only; it does not demonstrate model quality or validate a paid provider integration.

Visit **Usage** after a successful request to inspect its accounting. A readiness check confirms service dependencies; a successful provider request separately confirms credentials, model access, and upstream connectivity.

## Troubleshooting

```sh
docker compose -f examples/quickstart/compose.yml ps -a
docker compose -f examples/quickstart/compose.yml logs --tail=100 migrate bootstrap proxy admin mock
```

- **Cannot connect to Docker:** start your local Docker engine and verify `docker info` succeeds.
- **Migration failed:** inspect the `migrate` logs. The proxy and admin wait for migrations to finish. Do not bypass a failed migration.
- **Invalid API key:** use a user key for provider requests and the admin key for admin requests. The admin and proxy must use identical hash peppers.
- **Provider unavailable:** verify that an upstream credential is configured and that the model exists in your upstream account.
- **Port already allocated:** change the host side of the loopback port mapping in a private override.

Stop the stack while retaining its database:

```sh
docker compose -f examples/quickstart/compose.yml down
```

For a complete reset of this example only, append `--volumes`. That deletes its database and all local tenants, keys, and usage.

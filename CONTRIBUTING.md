# Contributing

Yapp is being prepared for a public source release. Until publication is approved, keep this repository and its release artifacts private. The owner has deferred license selection. A project license and dependency redistribution review must be completed before distributing source or binaries.

## Development setup

Use the Go version declared in `go.mod`, Docker with Compose v2, and pnpm for the dashboard. The local stack and synthetic mock provider are described in [Getting started](docs/getting-started.md).

```sh
./examples/quickstart/build.sh
docker compose -f examples/quickstart/compose.yml up -d --wait
python3 examples/quickstart/smoke-test.py
```

The optional smoke test needs Python 3 and uses only localhost. It creates a synthetic tenant, user, and proxy key, then requests a deterministic local mock completion. It makes no paid provider calls.

## Working on changes

Keep changes focused and preserve unrelated work in your checkout. Put runtime secrets and deployment-specific configuration in your private operations repository. Commit only source, tests, generic examples, and synthetic fixtures. Never add real conversation payloads, provider credentials, customer email addresses, or production screenshots.

Use `pnpm install --frozen-lockfile` in `ui/`. Honor configured package minimum-release-age settings; do not bypass them to install a newer dependency. Keep the lockfile changes relevant to your work.

For backend changes:

```sh
make build
make test
```

`make test` includes Go's race detector. For dashboard changes:

```sh
cd ui
pnpm install --frozen-lockfile
pnpm test
pnpm build
```

Verify affected interactions in an actual browser against an isolated stack. A successful build or API health check does not prove that a UI flow works. For schema changes, add a new numbered migration and verify it on a fresh database as well as an appropriate upgrade fixture.

The local module under `third_party/duffel` is required for backend builds. Shared handler services should remain in leaf packages to avoid import cycles between `internal/httpapi` and its handler subpackages.

## Proposing a change

Describe the problem, the behavior after the change, and how you verified it. Include commands/results and screenshots when useful. Distinguish local tests from browser checks and deployments. Use synthetic data for examples and screenshots.

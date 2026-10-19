# Documentation screenshots

Screenshots must come from a dedicated Docker Compose project with synthetic data. Never attach a production database or copy production session payloads into this environment.

```sh
./examples/quickstart/build.sh
docker compose -p piproxy-docs -f examples/quickstart/compose.yml up -d --wait
docker compose -p piproxy-docs -f examples/quickstart/compose.yml exec -T postgres \
  psql -U proxy -d proxy -v ON_ERROR_STOP=1 < examples/quickstart/demo.sql
```

Open http://localhost:13333 in a fresh browser profile and sign in with `local-demo-admin-key`. Capture the dashboard at 1440 × 900 and Tenants at 1440 × 650 after their API requests finish. Move the pointer away from charts so hover tooltips do not cover the data. Save the PNGs to `docs/images/dashboard.png` and `docs/images/tenants.png`. The fixture contains two fictional teams, three users at reserved `.example` addresses, and 168 synthetic usage records. Its fictional model names and costs are not provider pricing claims.

Use the `piproxy-docs` project exclusively for these captures. Stop any quickstart project using the same local ports before starting it. Teardown is scoped to this project's resources:

```sh
docker compose -p piproxy-docs -f examples/quickstart/compose.yml down --volumes
```

## Verification of the included captures

The included images were captured on 2026-10-19 from the local `piproxy-docs` Docker project after building both images with `examples/quickstart/build.sh`. Migrations 001–032 completed on a fresh PostgreSQL volume; fixture inserts created 2 tenants, 3 users, and 168 usage records. The actual dashboard login and both pages loaded without failed admin API requests. The screenshots were visually inspected.

Readiness, tenant/user/key creation, and authenticated model discovery were also verified against this isolated stack. No paid upstream completion or production deployment was tested, and no production data was used.

The subsequent release-preparation smoke test also built the finalized Dockerfile, started a fresh `piproxy-release-smoke` project with `up -d --wait`, and verified a real proxy-forwarded request to the bundled local mock. The mock required its own upstream credential and rejected forwarded client cookies. This is transport/authentication evidence, not a real provider integration test.

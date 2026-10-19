# DuckDB service over immutable Parquet transcripts

The admin transcript endpoint can use `services/session-query`, a separate
Python service with a long-lived DuckDB 1.4.5 instance. Go retains its static
build and renders the existing display response, including echo markers and
truncation indicators. PostgreSQL remains the authority for ownership,
generation, sequence, request metadata and accounting. Raw GCS archives remain
permanent. The reader itself does not migrate or delete raw bodies.

The backend's durable repair worker requires migration 032. One worker per
proxy reconciles missing or flagged transcripts and older captures absent from
a current generation. Database leases fence generation publication, renew
without retaining a connection, and persist retry backoff across restarts.
Legacy inline bodies are copied to immutable GCS keys and read back byte-for-byte
before their rows become permanent archives; inline copies remain intact.
Missing bodies fail repair without publishing a partial replacement. Admin
reads of known incomplete transcripts return 409 until repair finishes.
Monitor `corp_proxy_session_transcript_repairs_pending`, the oldest pending age,
and repair outcomes alongside reader query/export counters. Process readiness
alone does not prove complete archive coverage.

## Export and read flow

One background worker inventories readable GCS transcripts using short,
repeatable-read PostgreSQL transactions. Each connection closes before object
I/O; this adds at most one active PostgreSQL backend. Use a dedicated read-only
role, grant SELECT only on `session_transcripts`, `session_turns` and
`usage_events`, and recheck the shared database connection budget before rollout.
The client additionally sets transaction read-only, statement and lock limits.

Source objects are downloaded at pinned GCS generations and checked for size,
CRC32C and SHA-256. The worker verifies sequence/request coverage, preserves
each original JSONL line, and exports chunks of at most 128 turns, ordered by
sequence and event position. Parquet uses Zstd level 3 and 2,048-row groups.
All typed fields and raw lines are round-tripped before publication. Chunks
and manifests use immutable content-addressed keys under the separate
`session-parquet/v1` prefix. Uploaded copies are downloaded and hash-verified
before the local manifest becomes readable. Stable chunks are reused across
revisions; generation changes and late usage updates create new immutable data.

The admin first resolves the authoritative PostgreSQL state. A service request
contains the tenant, transcript ID, generation and last sequence. Only an exact
export match is eligible. DuckDB selects the page plus its preceding turn for
echo rendering. Go checks the returned snapshot identity and contiguous turn
coverage and uses the same renderer as the source reader. Missing/stale exports,
corruption, timeouts and service failures fall back to the existing source
reader. New captures never wait for exports and remain visible immediately.

GCS Parquet chunks are verified and cached locally once, with LRU eviction and
files pinned through query completion. DuckDB's external-file and metadata
caches stay alive between requests. This avoids requiring GCS HMAC keys or
runtime DuckDB extensions. The persistent cache volume also holds the latest
verified manifests; losing it triggers a bounded re-export from the permanent
source archives.

## Service API

All data routes require `Authorization: Bearer <dedicated service token>`.
No arbitrary SQL, caller-selected filenames, object keys or extension loading
is exposed. Every operation requires `tenant_id`, `transcript_id`, `generation`
and `last_seq` and uses bound SQL parameters.

- `POST /v1/page`: optional `from_seq` and `limit` (maximum 200); returns exact
  JSONL with snapshot headers. This is wired into the existing admin endpoint.
- `POST /v1/search`: `term`, optional `limit` (maximum 100); returns ordered
  record matches and previews from untruncated searchable text.
- `POST /v1/summary`: returns daily model/provider request/error/token/cost
  summaries and missing-usage counts for the verified snapshot. These are
  export-time accounting snapshots; PostgreSQL remains authoritative for billing.
- `GET /healthz`, `/readyz`, `/metrics`: process readiness and query/export
  counters, without session bodies or identifiers.

Captured transcript exports do not create canonical `pi_session_logs`, and
this service does not replace the existing Pi-log insights coordinator.

## Local validation and image

```sh
make test-session-query
GOCACHE=/tmp/piproxy-go-build make test-go
GOCACHE=/tmp/piproxy-go-build make build
make build-session-query VERSION=<exact-revision>
```

Python dependencies are locked in `uv.lock`; `uv sync --locked` preserves the
seven-day minimum release age. Docker builds from the service directory.

An existing private benchmark sample can exercise the exporter, HTTP service,
real Go client and source renderer without production access or writes:

```sh
cd services/session-query
uv run --locked python tests/replay_sample.py \
  --sample /path/to/private/sample --output /path/to/new/private/results
```

The sample needs `selection.json`, `object-receipts.jsonl` and its pinned gzip
objects. The harness verifies the source receipts, checks first/middle/end,
out-of-range and 128-turn chunk boundary pages, and compares model/error/token
summaries. Session bodies stay in the supplied private directories. To exercise
the PostgreSQL poller, point `PIPROXY_SESSION_QUERY_TEST_DSN` at a disposable
PostgreSQL database with all repository migrations applied before running the
Python tests. The integration test creates its own temporary SELECT-only role.

## Self-hosted configuration

Build the service independently with `make build-session-query VERSION=<revision>`.
Run it on the same private container network as admin, without publishing a host
port. Set `PROXY_SESSION_QUERY_URL=http://session-query:8092` on admin and use the
same dedicated token on both services. Required service environment includes:

```dotenv
PROXY_SESSION_QUERY_TOKEN=<dedicated random token, at least 32 characters>
SESSION_QUERY_POSTGRES_DSN=<dedicated read-only PostgreSQL DSN>
SESSION_QUERY_PARQUET_PREFIX=session-parquet/v1
GOOGLE_APPLICATION_CREDENTIALS=/run/secrets/archive-reader.json
```

Configure the archive bucket/prefix for your own installation. Mount the cloud
credential read-only and make it readable by UID 10001. Use a dedicated account
with source read and derived-prefix create/read access. Persist the service's
`/var/lib/session-query` cache directory; this is separate from source archives.
Keep raw-archive retention consistent with your installation's data policy.

Before enabling the admin URL, verify readiness, export progress and query
metrics. Compare authenticated first/middle/end transcript pages and an active
session. Rollback clears `PROXY_SESSION_QUERY_URL` and restarts admin, returning
reads to source archives. Keep image digests and infrastructure settings in your
private operations configuration. See [self-hosting](self-hosting.md).

Resource limits: one export worker, two query threads, four HTTP handlers,
128 MB DuckDB query buffer, 256 MB export buffer, 1 GiB container RAM, one CPU,
1 GiB local Parquet cache, bounded temporary spill, 64 MiB source-object/response
limits, a 10,000-turn metadata limit, 256 MiB decoded-source limit and 512 MiB
derived-JSON limit, with 1.5 GiB free export disk headroom required.
DuckDB memory limits do not bound every
Python/vector allocation; the container limit is the outer bound. Oversized
sessions remain readable through the source fallback. Failed exports leave no
published partial snapshot. A service restart retains the disk cache but loses
DuckDB's in-memory cache.

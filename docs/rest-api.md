# REST API

The server listens on port `8080` and serves the REST API, the embedded
console, `/metrics`, and an SSE event stream from one process. This page is
the endpoint map; the route bindings in `api/rest/bind/bind.go` are the
source of truth, and `caesium --help` is the quickest way to discover the CLI
verb that wraps each one. Paths below are under `/v1` unless shown otherwise.

## Authentication

With `CAESIUM_AUTH_MODE=none` (the default) every endpoint is open. With
`CAESIUM_AUTH_MODE=api-key` the REST API, `/metrics`, and the console require a
bearer API key or a browser session; webhook delivery keeps using per-trigger
signature configuration rather than bearer tokens. Native OIDC, SAML, and LDAP
sign-in can be enabled alongside API keys; see
[sso-authentication.md](sso-authentication.md).

When `CAESIUM_AUTH_MODE=api-key`, also set `CAESIUM_AUTH_KEY_HASH_SECRET` to a
long random server-side secret. New and rotated API keys are stored as
HMAC-SHA256 hashes derived from that secret. Legacy unkeyed SHA-256 hashes keep
validating after an upgrade so the change can roll out safely, but rotate those
keys so the database no longer contains them.

For the auth management CLI, supply credentials through `CAESIUM_API_KEY`. The
`--api-key` flag works but is visible in process listings.

| Endpoint | Purpose |
| --- | --- |
| `GET /auth/status` | Report the API-key and SSO methods available, so the console knows whether to show a login. |
| `GET /auth/whoami` | Return the current API-key or session principal, including the CSRF token cookie sessions must send on unsafe requests. |
| `POST /auth/logout` | Revoke the current browser session. |
| `GET /v1/auth/keys`, `POST /v1/auth/keys` | List and create API keys. |
| `POST /v1/auth/keys/:id/revoke`, `POST /v1/auth/keys/:id/rotate` | Revoke or rotate a key. |
| `GET /v1/auth/audit` | Query the audit log. |

## Jobs, runs, and definitions

| Endpoint | Purpose |
| --- | --- |
| `GET /health` | Health check (no prefix). |
| `GET /metrics` | Prometheus metrics (no prefix; viewer role required when API-key auth is on). |
| `GET /jobs`, `POST /jobs`, `GET /jobs/:id`, `DELETE /jobs/:id` | List, create, read, delete jobs. The list includes each job's recent runs. |
| `GET /jobs/:id/manifest` | The job's YAML as the server holds it. |
| `GET /jobs/:id/tasks`, `GET /jobs/:id/dag` | Persisted task definitions and the DAG's nodes and edges. |
| `GET /jobs/:id/topology`, `GET /jobs/:id/topology/history` | The current DAG snapshot and its append-only history. |
| `POST /jobs/:id/run` | Start a run. The `202` body's `outcome` is `created`, `queued`, or `skipped`; an `Idempotency-Key` header makes retries safe. See [job-definitions.md](job-definitions.md#starting-runs-from-other-systems-outcomes-and-idempotency). |
| `PUT /jobs/:id/pause`, `PUT /jobs/:id/unpause` | Suspend or resume scheduling without deleting the job. |
| `GET /jobs/:id/queue`, `DELETE /jobs/:id/queue/:queue_id` | Inspect or drop queued runs under a `queue` concurrency strategy. |
| `GET /jobs/:id/runs`, `GET /jobs/:id/runs/:run_id` | List runs; read one run with its tasks. |
| `GET /jobs/:id/runs/:run_id/logs?task_id=<task-id>` | Stream or fetch task logs. |
| `POST /jobs/:id/runs/:run_id/retry` | Retry a failed run, preserving succeeded and cached tasks. |
| `POST /jobs/:id/runs/:run_id/tasks/:task_id/partitions/:index/retry` | Retry one partition of a fanned-out task. |
| `GET /jobs/:id/runs/:run_id/tasks/:task_id/partitions` | List a fanned-out task's partitions. |
| `POST /jobs/:id/runs/:run_id/callbacks/retry` | Retry failed callbacks. |
| `POST /jobdefs/apply`, `POST /jobdefs/diff`, `POST /jobdefs/lint` | Apply, diff, or lint manifests against the server's catalog. |
| `GET /triggers`, `POST /triggers`, `GET /triggers/:id`, `PATCH /triggers/:id` | Manage triggers. |
| `POST /triggers/:id/fire` | Fire a trigger manually with optional params. |
| `GET /triggers/:id/events` | Events matched by an event trigger. |
| `GET /atoms`, `POST /atoms`, `GET /atoms/:id`, `DELETE /atoms/:id` | The container specs steps resolve to. |

## Backfills, cache, and events

| Endpoint | Purpose |
| --- | --- |
| `POST /jobs/:id/backfill`, `GET /jobs/:id/backfills`, `GET /jobs/:id/backfills/:backfill_id` | Start, list, and read backfills. See [backfill.md](backfill.md). |
| `PUT /jobs/:id/backfills/:backfill_id/cancel` | Cancel a backfill. |
| `GET /jobs/:id/cache`, `DELETE /jobs/:id/cache`, `DELETE /jobs/:id/cache/:task_name` | Inspect or invalidate a job's task cache. See [job-definitions.md](job-definitions.md#caching). |
| `POST /cache/prune` | Prune expired cache entries. |
| `GET /events` | Subscribe to lifecycle events over SSE. |
| `POST /events`, `GET /events/ingested` | Ingest an external event for event triggers; list ingested events. |
| `POST /hooks/*` | Webhook receiver for HTTP triggers. |

## Run queries and receipts

These back the investigation verbs described in
[design/data-plane-memory.md](design/data-plane-memory.md).

| Endpoint | Purpose |
| --- | --- |
| `GET /jobs/:id/runs/:run_id/why` | Why a task ran, was skipped, or failed (`caesium why`). |
| `GET /jobs/:id/blame` | Which change broke the job (`caesium blame`). |
| `GET /jobs/:id/runs/diff` | Compare two runs (`caesium run diff`). |
| `POST /jobs/:id/runs/:run_id/replay` | Start a quarantined replay (`caesium run replay`). |
| `GET /jobs/:id/runs/:run_id/tasks/:task/descriptor` | The recorded execution descriptor `caesium reproduce` rebuilds from. |
| `GET /jobs/:id/runs/:run_id/receipt`, `POST /jobs/:id/runs/:run_id/receipt/verify` | Fetch and verify a receipt (`caesium receipt get`, `caesium verify`). |
| `GET /lineage/impact` | Cross-job lineage impact. See [open-lineage.md](open-lineage.md). |
| `GET /contracts/graph` | The cross-job contract graph (`caesium contract graph`). |

## Datasets, incidents, and the agent runtime

| Endpoint | Purpose |
| --- | --- |
| `GET /datasets`, `GET /datasets/:ns/:name`, `GET /datasets/:ns/:name/derivations`, `POST /datasets/:ns/:name/advance` | Dataset freshness state, derivation history, and watermark advance. Always registered; the freshness evaluator that populates them runs when `CAESIUM_FRESHNESS_ENABLED=true`. |
| `GET /datasets/:ns/:name/metrics`, `GET /datasets/holds`, `POST /datasets/holds/:id/release` | Recorded `##caesium::metrics` samples and the holds raised by the data circuit breaker. Registered only when `CAESIUM_DATA_ASSERTIONS_ENABLED=true`; otherwise these paths return 404. |
| `GET /incidents`, `GET /incidents/:id` | Incidents opened by the remediation runtime. Gated by `CAESIUM_AGENT_REMEDIATION_ENABLED`. |
| `POST /incidents/:id/approvals/:approval_id/approve`, `…/reject` | Decide a tier-3 approval. |
| `GET /agent/incidents/:id/bundle`, `GET /agent/incidents/:id/context/*`, `POST /agent/incidents/:id/actions`, `POST /agent/incidents/:id/notes`, `POST /agent/incidents/:id/mcp` | The scoped surface the agent runtime uses. See [design/agent-in-the-loop.md](design/agent-in-the-loop.md). |
| `GET /agentprofiles`, `POST /agentprofiles`, `GET /agentprofiles/:id`, `PATCH /agentprofiles/:id`, `DELETE /agentprofiles/:id` | Agent profiles referenced by `metadata.remediation`. |
| `GET /notifications/channels`, `GET /notifications/policies` (plus create, read, patch, delete) | Notification channels and policies. |

## System and operator surfaces

| Endpoint | Purpose |
| --- | --- |
| `GET /stats`, `GET /stats/summary` | Aggregated job and run statistics. |
| `GET /system/features` | Which feature gates are enabled on this server. |
| `GET /system/nodes`, `DELETE /system/nodes/:id` | Cluster membership; remove a stale dqlite member. |
| `GET /nodes/:address/workers` | Worker state for one node. |

Two debugging surfaces in the console are gated behind environment variables
because they expose the server's internals and are not default public APIs.

| Surface | Enable with | Endpoints |
| --- | --- | --- |
| Server log console | `CAESIUM_LOG_CONSOLE_ENABLED=true` | `GET /logs/stream`, `GET /logs/level`, `PUT /logs/level` |
| Database console | `CAESIUM_DATABASE_CONSOLE_ENABLED=true` | `GET /database/schema`, `POST /database/query` |

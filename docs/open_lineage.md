# OpenLineage Integration

## What is OpenLineage?

[OpenLineage](https://openlineage.io) is an open standard for data lineage metadata collection, governed by the LF AI & Data Foundation. It defines a common JSON format for describing job executions, their inputs, outputs, and associated metadata. By emitting OpenLineage events, any data system can participate in a cross-platform lineage graph alongside tools like Apache Airflow, Apache Spark, dbt, and others.

The standard has three pillars:

- **Producers** emit lineage events (Caesium is a producer).
- **Spec** defines the event schema (`RunEvent` JSON with jobs, runs, datasets, and facets).
- **Consumers** ingest, store, and visualise lineage (e.g. Marquez, DataHub, Atlan, Google Cloud Data Catalog).

## How Caesium integrates

Caesium emits OpenLineage `RunEvent` messages at each lifecycle transition in a job run:

| Caesium event | OpenLineage event type | Scope |
|---------------|----------------------|-------|
| Run started | `START` | Parent (DAG-level) |
| Run completed | `COMPLETE` | Parent |
| Run failed | `FAIL` | Parent |
| Task started | `START` | Child (task-level, linked to parent via `parent` facet) |
| Task succeeded | `COMPLETE` | Child |
| Task failed | `FAIL` | Child (includes `errorMessage` facet) |
| Task skipped | `ABORT` | Child |

The integration subscribes to the internal event bus and translates events into the OpenLineage format. It is entirely non-blocking; if the lineage backend is unreachable, events are dropped rather than slowing down job execution.

### Naming conventions

- **Job namespace**: Configurable via `CAESIUM_OPEN_LINEAGE_NAMESPACE` (e.g. `caesium-prod`, `caesium-staging`).
- **Job name (DAG-level)**: The job alias (e.g. `nightly-etl`).
- **Job name (task-level)**: `{job_alias}.task.{task_id}` (e.g. `nightly-etl.task.a1b2c3d4-...`).

### Facets emitted

**Standard facets:**

| Facet | Attached to | Description |
|-------|------------|-------------|
| `jobType` | All events | `integration: CAESIUM`, `processingType: BATCH`, `jobType: JOB` or `TASK` |
| `parent` | Task events | Links child task run to parent job run |
| `errorMessage` | Failed events | Error message and programming language |
| `sourceCodeLocation` | All events (when provenance exists) | Git repo, branch, path, commit from job provenance |

**Custom Caesium facets:**

| Facet key | Attached to | Fields |
|-----------|------------|--------|
| `caesium_dag` | Run START | `totalTasks`, `triggerType`, `triggerAlias` |
| `caesium_execution` | Task events | `engine`, `image`, `command`, `runtimeId`, `claimedBy` |

## Configuration

The integration is controlled entirely via environment variables. It is disabled by default.

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `CAESIUM_OPEN_LINEAGE_ENABLED` | bool | `false` | Enable OpenLineage event emission |
| `CAESIUM_OPEN_LINEAGE_TRANSPORT` | string | `http` | Transport type: `http`, `console`, or `file` |
| `CAESIUM_OPEN_LINEAGE_URL` | string | (empty) | HTTP endpoint URL (required when transport is `http`) |
| `CAESIUM_OPEN_LINEAGE_NAMESPACE` | string | `caesium` | OpenLineage job namespace |
| `CAESIUM_OPEN_LINEAGE_HEADERS` | string | (empty) | Comma-separated `key=value` pairs for HTTP headers (e.g. auth tokens) |
| `CAESIUM_OPEN_LINEAGE_FILE_PATH` | string | `/var/lib/caesium/lineage.ndjson` | Output file path when transport is `file` |
| `CAESIUM_OPEN_LINEAGE_TIMEOUT` | duration | `5s` | HTTP client timeout |

## Transport options

### HTTP (default)

Sends events as JSON POST requests to an OpenLineage-compatible backend such as [Marquez](https://marquezproject.ai/).

```sh
export CAESIUM_OPEN_LINEAGE_ENABLED=true
export CAESIUM_OPEN_LINEAGE_TRANSPORT=http
export CAESIUM_OPEN_LINEAGE_URL=http://marquez:5000/api/v1/lineage
export CAESIUM_OPEN_LINEAGE_NAMESPACE=caesium-prod
```

To pass an API key or bearer token:

```sh
export CAESIUM_OPEN_LINEAGE_HEADERS="Authorization=Bearer my-secret-token"
```

### Console

Logs each event at INFO level via the structured logger. Useful for development and debugging.

```sh
export CAESIUM_OPEN_LINEAGE_ENABLED=true
export CAESIUM_OPEN_LINEAGE_TRANSPORT=console
```

### File

Appends events as newline-delimited JSON (NDJSON) to a file. Useful for testing or offline auditing.

```sh
export CAESIUM_OPEN_LINEAGE_ENABLED=true
export CAESIUM_OPEN_LINEAGE_TRANSPORT=file
export CAESIUM_OPEN_LINEAGE_FILE_PATH=/tmp/caesium-lineage.ndjson
```

## Example: Marquez quickstart

1. Start Marquez using Docker Compose (see [Marquez quickstart](https://marquezproject.ai/quickstart)):

   ```sh
   git clone https://github.com/MarquezProject/marquez.git
   cd marquez
   docker compose up -d
   ```

   Marquez UI will be available at `http://localhost:3000` and the API at `http://localhost:5000`.

2. Configure Caesium to emit to Marquez:

   ```sh
   export CAESIUM_OPEN_LINEAGE_ENABLED=true
   export CAESIUM_OPEN_LINEAGE_URL=http://localhost:5000/api/v1/lineage
   export CAESIUM_OPEN_LINEAGE_NAMESPACE=caesium-dev
   ```

3. Run a job in Caesium. You should see the job and its tasks appear in the Marquez UI lineage graph, with parent-child relationships linking the DAG run to each task run.

## Observability

When OpenLineage is enabled, the following Prometheus metrics are registered:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `caesium_lineage_events_emitted_total` | Counter | `event_type`, `status` | Total events emitted (`status`: `success` or `error`) |
| `caesium_lineage_emit_duration_seconds` | Histogram | `transport` | Duration of event emission |

## Troubleshooting

**Events are not appearing in Marquez:**

1. Verify the integration is enabled: look for `launching openlineage subscriber` in the Caesium logs at startup.
2. Switch to console transport temporarily to confirm events are being generated.
3. Check that `CAESIUM_OPEN_LINEAGE_URL` is reachable from the Caesium process.
4. Look for `lineage: failed to emit event` error messages in the logs.
5. Check the `caesium_lineage_events_emitted_total` metric for `status="error"` counts.

**Performance concerns:**

The OpenLineage subscriber runs in a dedicated goroutine and uses the event bus, which has a 100-event buffer. If the lineage backend is slow or unreachable, events are dropped silently rather than blocking job execution. The HTTP transport has a configurable timeout (default 5 seconds). Monitor the `caesium_lineage_emit_duration_seconds` histogram to identify latency issues.

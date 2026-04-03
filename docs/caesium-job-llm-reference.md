# Caesium Job Definition — LLM Authoring Reference

This document is the canonical reference for AI coding assistants generating Caesium job definitions. It covers the full YAML schema, validation rules, data contracts, and local development workflow.

## Quick Start Template

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: <unique-job-name>       # Required. Unique across the Caesium installation.
  labels: {}                     # Optional key-value pairs for filtering.
  annotations: {}                # Optional free-form metadata.
  maxParallelTasks: 2            # Optional. Max concurrent tasks.
  taskTimeout: 5m                # Optional. Per-task timeout.
  runTimeout: 30m                # Optional. Whole-run timeout.
  schemaValidation: warn         # Optional. "" | "warn" | "fail"
trigger:
  type: cron                     # "cron" or "http"
  configuration:
    cron: "0 2 * * *"            # POSIX 5-field cron expression
    timezone: "UTC"              # Optional, defaults to UTC
  defaultParams:                 # Optional run parameters
    key: value
steps:
  - name: step-one
    engine: docker               # "docker" (default), "podman", or "kubernetes"
    image: alpine:3.20
    command: ["sh", "-c", "echo hello"]
```

---

## Schema Reference

### Top-Level Fields

| Field | Type | Required | Notes |
|---|---|---|---|
| `apiVersion` | string | yes | Must be `v1` |
| `kind` | string | yes | Must be `Job` |
| `metadata` | object | yes | See Metadata table |
| `trigger` | object | yes | See Trigger section |
| `callbacks` | array | no | Webhook notifications |
| `steps` | array | yes | At least one step |

### Metadata

| Field | Type | Required | Notes |
|---|---|---|---|
| `alias` | string | yes | Unique job identifier |
| `labels` | map | no | Key-value pairs for filtering |
| `annotations` | map | no | Free-form metadata |
| `maxParallelTasks` | int | no | Max concurrent tasks per run |
| `taskTimeout` | duration | no | Per-task timeout (e.g. `5m`, `1h`) |
| `runTimeout` | duration | no | Whole-run wall-clock limit |
| `schemaValidation` | string | no | `""` (disabled), `"warn"`, or `"fail"` |

### Trigger — Cron

```yaml
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"            # Required. 5-field POSIX cron.
    timezone: "America/New_York" # Optional. IANA timezone, defaults to UTC.
  defaultParams:                 # Optional. Injected as run parameters.
    logical_date: "{{ .LogicalDate }}"
```

### Trigger — HTTP

```yaml
trigger:
  type: http
  configuration:
    path: "/hooks/my-job"        # Required. Route path.
    secret: "webhook-secret"     # Optional. Shared secret for validation.
```

### Steps

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | Unique within the job |
| `engine` | string | no | `docker` (default), `podman`, `kubernetes` |
| `image` | string | yes | Container image reference |
| `command` | array[string] | no | Container command |
| `env` | map | no | Environment variables |
| `workdir` | string | no | Container working directory |
| `mounts` | array[Mount] | no | Bind mounts |
| `next` | string or array | no | Successor step name(s) — fan-out |
| `dependsOn` | string or array | no | Predecessor step name(s) — fan-in/join |
| `retries` | int | no | Retry attempts after failure |
| `retryDelay` | duration | no | Delay between retries |
| `retryBackoff` | bool | no | Exponential backoff |
| `triggerRule` | string | no | `all_success` (default), `all_done`, `all_failed`, `one_success`, `always` |
| `outputSchema` | object | no | JSON Schema for structured output |
| `inputSchema` | map | no | Maps predecessor names to required schemas |

### Mount

| Field | Type | Required |
|---|---|---|
| `type` | string | no (default: `bind`) |
| `source` | string | yes — host path |
| `target` | string | yes — container path |
| `readOnly` | bool | no |

### Callbacks

```yaml
callbacks:
  - type: notification
    configuration:
      webhook_url: "https://hooks.slack.com/services/..."
      channel: "#pipelines"
      user_agent: "caesium"
```

---

## DAG Wiring Rules

1. **Implicit sequential**: When NO step declares `next` or `dependsOn`, steps link in order automatically.
2. **Explicit mode**: Once ANY step uses `next` or `dependsOn`, ALL edges must be declared explicitly.
3. **Fan-out**: Use `next: [step-a, step-b]` on a step.
4. **Fan-in/Join**: Use `dependsOn: [step-a, step-b]` on the joining step.
5. **Validation**: Cycles, self-references, unknown step names, and duplicate names are rejected.

---

## Data Contracts (Output/Input Schemas)

Enable runtime validation of structured data flowing between pipeline steps.

### Emitting Output

Tasks emit structured output by printing a magic line to stdout:

```sh
echo '##caesium::output {"row_count": 5000, "source": "warehouse"}'
```

### Consuming Output

Downstream steps receive predecessor outputs as environment variables:

```
$CAESIUM_OUTPUT_<STEP_NAME>_<KEY>
```

Keys are uppercased: `row_count` → `ROW_COUNT`, step name `extract` → `EXTRACT`.

### Declaring Schemas

```yaml
steps:
  - name: extract
    image: alpine:3.20
    outputSchema:
      type: object
      properties:
        row_count: { type: integer }
        source: { type: string }
      required: [row_count, source]
    command: ["sh", "-c", "echo '##caesium::output {\"row_count\": 5000, \"source\": \"warehouse\"}'"]

  - name: transform
    image: alpine:3.20
    dependsOn: [extract]
    inputSchema:
      extract:
        required: [row_count]
        properties:
          row_count: { type: integer }
    command: ["sh", "-c", "echo Processing $CAESIUM_OUTPUT_EXTRACT_ROW_COUNT rows"]
```

Set `metadata.schemaValidation` to `"warn"` (log violations) or `"fail"` (fail task on violation).

---

## Secret References

Use `secret://` URIs instead of hardcoding credentials:

| Provider | URI Pattern | Example |
|---|---|---|
| Environment | `secret://env/VAR_NAME` | `secret://env/DB_PASSWORD` |
| Kubernetes | `secret://k8s/<secret>/<key>` | `secret://k8s/db-creds/password` |
| Vault | `secret://vault/<path>?field=<key>` | `secret://vault/secret/data/db?field=password` |

---

## Local Development Commands

```sh
caesium job lint --path jobs/           # Validate YAML schemas and DAG topology
caesium job preview --path job.yaml     # ASCII DAG visualization
caesium dev --once --path job.yaml      # Run job locally against Docker
caesium dev --path job.yaml             # Watch mode — re-run on file save
caesium job diff --path jobs/           # Preview creates/updates/deletes vs server
caesium job apply --path jobs/          # Deploy definitions to running server
caesium test --path jobs/               # Full validation suite
caesium test --scenario harness/        # Execute harness scenarios against the local runtime
```

---

## Harness Scenarios

Use harness scenarios when you want executable assertions instead of schema-only validation. Scenario files should use a `.scenario.yaml` suffix and the `Harness` kind.

```yaml
apiVersion: v1
kind: Harness
scenarios:
  - name: nightly-etl-smoke
    path: ./nightly-etl.job.yaml
    expect:
      runStatus: succeeded
      tasks:
        - name: extract
          status: succeeded
          logContains:
            - extracting
        - name: load
          status: succeeded
```

Supported assertions:

- `expect.runStatus`: expected final run status (`succeeded`, `failed`, etc.)
- `expect.errorContains`: substring match against the run error
- `expect.tasks[].status`: expected task status
- `expect.tasks[].output`: expected output key/value subset
- `expect.tasks[].logContains`: required log substrings
- `expect.tasks[].schemaViolationCount`: exact number of runtime schema violations
- `expect.tasks[].cacheHit`: expected cache-hit boolean
- `expect.tasks[].errorContains`: substring match against the task error
- `expect.metrics[]`: Prometheus assertions with exact `value` or `delta`
- `expect.metrics[].labels`: metric labels, including `$job_id`, `$job_alias`, and `$task_id:<task-name>` placeholders
- `expect.lineage.totalEvents`: exact emitted OpenLineage event count
- `expect.lineage.eventTypes`: exact OpenLineage event-type counts (`START`, `COMPLETE`, `FAIL`, `ABORT`)
- `expect.lineage.jobNames`: required OpenLineage job names present in the emitted stream

Example with observability assertions:

```yaml
apiVersion: v1
kind: Harness
scenarios:
  - name: nightly-etl-observability
    path: ./nightly-etl.job.yaml
    expect:
      runStatus: succeeded
      metrics:
        - name: caesium_job_runs_total
          labels:
            job_id: $job_id
            status: succeeded
          value: 1
        - name: caesium_lineage_events_emitted_total
          labels:
            event_type: COMPLETE
            status: success
          delta: 4
      lineage:
        totalEvents: 8
        eventTypes:
          START: 4
          COMPLETE: 4
        jobNames:
          - nightly-etl
```

---

## Common Patterns

### Sequential ETL Pipeline
```yaml
steps:
  - name: extract
    image: python:3.12-slim
    command: ["python", "extract.py"]
  - name: transform
    image: python:3.12-slim
    command: ["python", "transform.py"]
  - name: load
    image: python:3.12-slim
    command: ["python", "load.py"]
```

### Fan-Out / Fan-In
```yaml
steps:
  - name: setup
    image: alpine:3.20
    command: ["sh", "-c", "echo setup"]
    next: [branch-a, branch-b]
  - name: branch-a
    image: alpine:3.20
    command: ["sh", "-c", "echo a"]
    dependsOn: setup
  - name: branch-b
    image: alpine:3.20
    command: ["sh", "-c", "echo b"]
    dependsOn: setup
  - name: join
    image: alpine:3.20
    command: ["sh", "-c", "echo join"]
    dependsOn: [branch-a, branch-b]
    triggerRule: all_success
```

### HTTP-Triggered with Retries
```yaml
trigger:
  type: http
  configuration:
    path: "/hooks/deploy"
    secret: "deploy-webhook-secret"
steps:
  - name: deploy
    image: myorg/deployer:latest
    command: ["deploy", "--env", "staging"]
    retries: 3
    retryDelay: 30s
    retryBackoff: true
    env:
      DEPLOY_TOKEN: secret://vault/secret/data/deploy?field=token
```

### Data Contract Pipeline
```yaml
metadata:
  alias: contract-pipeline
  schemaValidation: fail
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: produce
    image: alpine:3.20
    outputSchema:
      type: object
      properties:
        count: { type: integer }
        status: { type: string }
      required: [count, status]
    command: ["sh", "-c", "echo '##caesium::output {\"count\": 42, \"status\": \"ok\"}'"]
  - name: consume
    image: alpine:3.20
    dependsOn: [produce]
    inputSchema:
      produce:
        required: [count]
        properties:
          count: { type: integer }
    command: ["sh", "-c", "echo Got $CAESIUM_OUTPUT_PRODUCE_COUNT items"]
```

---

## Generation Checklist

When generating a Caesium job definition, verify:

- [ ] `apiVersion: v1` and `kind: Job` are present
- [ ] `metadata.alias` is a unique, lowercase, hyphenated identifier
- [ ] Trigger type is `cron` (with valid 5-field cron expression) or `http` (with path)
- [ ] Every step has a unique `name` and an `image`
- [ ] DAG edges are consistent: if any step uses `next`/`dependsOn`, all edges are explicit
- [ ] `next` references only step names that exist; same for `dependsOn`
- [ ] No cycles in the DAG
- [ ] `outputSchema`/`inputSchema` use valid JSON Schema types
- [ ] `inputSchema` keys reference actual predecessor step names
- [ ] Secrets use `secret://` URIs, never hardcoded values
- [ ] Durations use Go format: `30s`, `5m`, `1h`
- [ ] File is saved with `.job.yaml` extension for Git sync glob matching

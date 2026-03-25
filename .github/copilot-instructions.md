# Caesium — GitHub Copilot Instructions

## About Caesium

Caesium is a distributed job scheduler using declarative YAML DAG pipelines with Docker/Podman/Kubernetes runtimes.

## Generating Job Definitions

When generating or completing Caesium job definition YAML files (`.job.yaml`), follow these rules:

### Required Structure

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: <unique-lowercase-hyphenated-name>
trigger:
  type: cron | http
  configuration: { ... }
steps:
  - name: <unique-step-name>
    image: <container-image>
    command: [...]
```

### Trigger Types

**Cron**: `configuration.cron` is a 5-field POSIX cron expression. Optional `timezone` (IANA, defaults to UTC). Optional `defaultParams` map.

**HTTP**: `configuration.path` is the route path. Optional `secret` for webhook validation.

### Step Fields

| Field | Type | Notes |
|---|---|---|
| `name` | string | Required, unique within job |
| `engine` | string | `docker` (default), `podman`, `kubernetes` |
| `image` | string | Required container image |
| `command` | string[] | Container command |
| `env` | map | Environment variables |
| `workdir` | string | Container working directory |
| `mounts` | array | Bind mounts: `{type, source, target, readOnly}` |
| `next` | string/array | Fan-out successor(s) |
| `dependsOn` | string/array | Fan-in predecessor(s) |
| `retries` | int | Retry attempts |
| `retryDelay` | duration | e.g. `30s`, `5m` |
| `retryBackoff` | bool | Exponential backoff |
| `triggerRule` | string | `all_success`, `all_done`, `all_failed`, `one_success`, `always` |
| `outputSchema` | object | JSON Schema for step output |
| `inputSchema` | map | Maps predecessor names to required schemas |

### DAG Rules

- No `next`/`dependsOn` anywhere → steps link sequentially (implicit)
- Any step uses `next`/`dependsOn` → all edges must be explicit
- No cycles, self-references, or unknown step names

### Data Contracts

Steps emit output: `echo '##caesium::output {"key": "value"}'`
Downstream access: `$CAESIUM_OUTPUT_<STEP>_<KEY>` (uppercased)

Set `metadata.schemaValidation: "warn"` or `"fail"` to enable runtime validation.

### Metadata Options

| Field | Type | Notes |
|---|---|---|
| `labels` | map | Key-value pairs for filtering |
| `annotations` | map | Free-form metadata |
| `maxParallelTasks` | int | Max concurrent tasks |
| `taskTimeout` | duration | Per-task timeout |
| `runTimeout` | duration | Whole-run timeout |
| `schemaValidation` | string | `""`, `"warn"`, `"fail"` |

### Callbacks

```yaml
callbacks:
  - type: notification
    configuration:
      webhook_url: "https://hooks.slack.com/..."
      channel: "#pipelines"
```

### Secrets

Use `secret://` URIs: `secret://env/VAR`, `secret://k8s/<secret>/<key>`, `secret://vault/<path>?field=<key>`

### Local Dev Commands

```sh
caesium job lint --path jobs/       # Validate
caesium dev --once --path job.yaml  # Run locally
caesium job diff --path jobs/       # Preview changes
caesium job apply --path jobs/      # Deploy
```

## Go Development

- Build: `just build` (containerized, not bare `go build`)
- Test: `just unit-test`
- Lint: `just lint`
- Job definition types: `pkg/jobdef/definition.go`
- Examples: `docs/examples/*.job.yaml`

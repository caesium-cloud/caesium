# Caesium — Gemini Code Assist Instructions

## About Caesium

Caesium is a distributed job scheduler with declarative YAML DAG pipelines supporting Docker, Podman, and Kubernetes runtimes. Job definitions are YAML manifests stored in source control.

## Generating Caesium Job Definitions

For the full schema reference, see `docs/caesium-job-llm-reference.md`. The following is a condensed guide.

### Required Structure

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: <unique-lowercase-hyphenated-name>
trigger:
  type: cron    # or "http"
  configuration:
    cron: "0 2 * * *"
steps:
  - name: <unique-step-name>
    image: <container-image>
    command: ["sh", "-c", "echo hello"]
```

### Trigger Types

| Type | Required Config | Optional Config |
|---|---|---|
| `cron` | `cron` (5-field POSIX) | `timezone` (IANA, default UTC), `defaultParams` |
| `http` | `path` (route) | `secret` (webhook validation) |

### Step Fields

Required: `name` (unique within job), `image` (container reference).

Optional: `engine` (docker/podman/kubernetes, default: docker), `command` (string array), `env` (map), `workdir`, `mounts` (bind mounts with type/source/target/readOnly), `next` (fan-out), `dependsOn` (fan-in), `retries`, `retryDelay`, `retryBackoff`, `triggerRule`, `outputSchema`, `inputSchema`.

### DAG Rules

1. No `next`/`dependsOn` on any step → steps auto-link sequentially
2. Any step uses explicit edges → all edges must be explicit
3. No cycles, self-references, or unknown step references
4. `triggerRule` values: `all_success` (default), `all_done`, `all_failed`, `one_success`, `always`

### Data Contracts

Steps produce structured output:
```sh
echo '##caesium::output {"row_count": 100, "source": "warehouse"}'
```

Downstream steps consume via environment variables:
```
$CAESIUM_OUTPUT_<STEP_NAME>_<KEY_NAME>   # Both uppercased
```

Declare schemas:
- `outputSchema`: JSON Schema on producing step
- `inputSchema`: Map of predecessor name → required schema on consuming step
- `metadata.schemaValidation`: `"warn"` (log violations) or `"fail"` (fail task)

### Secrets

Never hardcode credentials. Use `secret://` URIs:
- Environment: `secret://env/VAR_NAME`
- Kubernetes: `secret://k8s/<secret>/<key>`
- Vault: `secret://vault/<path>?field=<key>`

### Metadata Options

- `labels` / `annotations`: Key-value maps
- `maxParallelTasks`: Max concurrent tasks per run
- `taskTimeout` / `runTimeout`: Go-style durations (`5m`, `1h`)
- `schemaValidation`: `""`, `"warn"`, `"fail"`

### Callbacks

```yaml
callbacks:
  - type: notification
    configuration:
      webhook_url: "https://hooks.slack.com/services/..."
      channel: "#pipelines"
```

### File Naming

Save job definitions with `.job.yaml` extension for Git sync compatibility.

### Validation Commands

```sh
caesium job lint --path jobs/           # Validate schemas and DAG
caesium job preview --path job.yaml     # ASCII DAG visualization
caesium dev --once --path job.yaml      # Run locally against Docker
caesium job diff --path jobs/           # Preview changes vs server
caesium job apply --path jobs/          # Deploy to running server
```

## Go Development

- Build: `just build` (containerized builds required)
- Test: `just unit-test` (race detector + coverage)
- Lint: `just lint` (go fmt + go vet + golangci-lint)
- Job definition Go types: `pkg/jobdef/definition.go`
- Example manifests: `docs/examples/*.job.yaml`
- CI: `.circleci/` (CircleCI), not GitHub Actions

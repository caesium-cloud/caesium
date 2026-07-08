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
  priority: normal               # Optional. high | normal | low.
  concurrency:                   # Optional. Run-level admission policy.
    maxRuns: 1
    strategy: queue              # queue | replace | skip | fail
  rateLimits:                    # Optional. Shared resource budgets.
    - resource: warehouse-api
      limit: 120
      window: 1m
  schemaValidation: warn         # Optional. "" | "warn" | "fail"
trigger:
  type: cron                     # "cron", "http", or "event"
  configuration:
    cron: "0 2 * * *"            # POSIX 5-field cron expression
    timezone: "UTC"              # Optional, defaults to UTC
  defaultParams:                 # Optional run parameters
    key: value
volumes:                         # Optional BYO storage declarations.
  - name: work
    sources:
      docker: {bind: /mnt/nfs/caesium-work}
      kubernetes: {pvc: ci-shared-rwx}
steps:
  - name: step-one
    engine: docker               # "docker" (default), "podman", or "kubernetes"
    image: alpine:3.23
    command: ["sh", "-c", "echo hello"]
    rateLimit:                   # Optional. Consumes a metadata.rateLimits resource.
      resource: warehouse-api
      units: 1
    volumeMounts:
      - {volume: work, path: /work}
```

---

## Schema Reference

> The complete, authoritative field-by-field schema is generated from `pkg/jobdef` in **[job-schema-reference.md](job-schema-reference.md)** and kept in sync by CI. This section is a quick-reference of the fields you reach for most — consult the generated reference for the full set (including `$schema`, job- and step-level `cache`, `type: branch`, `nodeSelector`, and every mount/callback option).

### Structure at a glance

- `apiVersion: v1` and `kind: Job` (both required)
- `metadata` (required): `alias` (required, unique) · `labels` · `annotations` · `maxParallelTasks` · `taskTimeout` · `runTimeout` · `priority` (`high` | `normal` | `low`) · `concurrency` (`maxRuns` + `strategy`) · `rateLimits` · `schemaValidation` (`""` | `"warn"` | `"fail"`) · `replaySafe` · `cache` · Kubernetes defaults (`serviceAccountName`, `podAnnotations`, `automountServiceAccountToken`)
- `trigger` (required): `type` (`cron` | `http` | `event`) + `configuration` + optional `defaultParams` — see the snippets below
- `volumes` (optional): named BYO storage sources mounted by steps
- `steps` (required, ≥1): see the step quick-reference below
- `callbacks` (optional): post-run notification hooks

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
    path: "my-job"               # Required. Served at POST /v1/hooks/my-job.
    secret: "secret://env/WEBHOOK_SECRET" # Optional. Shared secret for validation.
    signatureScheme: hmac-sha256 # Optional. hmac-sha256, hmac-sha1, bearer, basic.
    signatureHeader: X-Hub-Signature-256  # Optional. Header containing the signature.
    paramMapping:                # Optional. JSON body -> run params.
      branch: "$.ref"
      commit: "$.after"
```

### Trigger — Event

```yaml
trigger:
  type: event
  configuration:
    events:                     # Required. One or more event patterns.
      - type: "deployment.*"    # Required. Exact event type or glob.
        source: "github-actions" # Optional. Exact source match.
        filter:                 # Optional. Dot-path string comparisons over event data.
          environment: "production"
          "repository.full_name": "acme/warehouse"
    paramMapping:               # Optional. JSON event data -> run params.
      commit: "$.commit"
      actor: "$.actor"
    defaultParams:              # Optional. String defaults merged before extracted params.
      triggered_by: event
```

For trigger chaining, match lifecycle events from `source: caesium`, for example `type: "run_completed"` with `filter.job_alias: "upstream-job"`. Caesium injects and increments the scheduler-owned `_trigger_depth` run param to stop runtime loops; do not set it in authored manifests.

### Run Scheduling Controls

Use `metadata.priority` to order pending work when the cluster is saturated. Valid values are `high`, `normal`, and `low`. Priority is ordering only; it does not preempt running tasks.

Use `metadata.concurrency` to control new runs of the same job when prior runs are still active:

```yaml
metadata:
  alias: nightly-import
  concurrency:
    maxRuns: 1
    strategy: skip              # queue, replace, skip, or fail
```

Use `metadata.rateLimits` to declare shared resource budgets and `steps[].rateLimit` to consume units from one of those resources:

```yaml
metadata:
  alias: api-ingest
  rateLimits:
    - resource: warehouse-api
      limit: 120
      window: 1m
steps:
  - name: extract
    image: alpine:3.23
    rateLimit:
      resource: warehouse-api
      units: 2
```

`priority`, `concurrency`, `rateLimits`, and `steps[].rateLimit` are scheduling metadata, not execution inputs, so changing only these fields does not change the task cache identity.

### Steps (most-used fields)

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | Unique within the job |
| `image` | string | yes | Container image reference |
| `engine` | string | no | `docker` (default), `podman`, `kubernetes` |
| `command` | array[string] | no | Container command |
| `env` | map | no | Environment variables (values may be `secret://` URIs) |
| `next` / `dependsOn` | string or array | no | DAG edges — fan-out / fan-in (see [DAG Wiring](#dag-wiring-rules)) |
| `retries` / `retryDelay` / `retryBackoff` | int / duration / bool | no | Retry policy |
| `triggerRule` | string | no | `all_success` (default), `all_done`, `all_failed`, `one_success`, `always` |
| `outputSchema` / `inputSchema` | object / map | no | Data contracts (see [Data Contracts](#data-contracts-outputinput-schemas)) |
| `datasets` | object | no | Freshness and contract surface: `consumes` (dataset names or `{name, schema}` objects) and `produces` (datasets with `freshness`/`maxStaleness`/`watermark` SLOs plus optional `schema`/`schemaFrom`/`version`). Excluded from the cache hash. See [Datasets & Freshness](#datasets--freshness-opt-in) |
| `replaySafe` | bool | no | Durable mark that allows this step to be re-executed by quarantined what-if replay. Job-level `metadata.replaySafe: true` marks all steps; step-level `replaySafe: true` marks one. Recorded on the baseline task run; excluded from the cache hash |
| `rateLimit` | object | no | Consume units from a job-level `metadata.rateLimits` resource: `{resource, units}`. Excluded from the cache hash |
| `cache` | bool or object | no | Task caching — `true`, `{ttl: "12h", version: 2}`, or `{pinDigests: true}` to resolve the image tag to its content digest and fold the digest (not the mutable tag) into the cache key so a moved tag misses instead of serving a stale hit (default `CAESIUM_CACHE_PIN_DIGESTS`). The resolved tag→digest mapping is a perf cache reused for `digestTTL` (default `CAESIUM_CACHE_DIGEST_TTL`, 5m); a moved tag is re-detected only after that window, or immediately with `{pinDigests: true, digestTTL: 0}` |
| `type` | string | no | `task` (default) or `branch` for conditional fan-out |
| `workdir` / `mounts` / `nodeSelector` | string / array / map | no | Working dir, bind mounts (`source`/`target`/`readOnly`), and distributed-mode node labels — full shape in the [generated reference](job-schema-reference.md) |
| `volumeMounts` | array | no | Mount a declared job volume: `{volume, path, readOnly?, subPath?}` |
| `serviceAccountName` / `podAnnotations` / `automountServiceAccountToken` | string / map / bool | no | Kubernetes workload-identity passthrough |
| `kueue` | object | no | Delegate admission to a [Kueue](https://kueue.sigs.k8s.io/) LocalQueue (kubernetes engine only): `{queueName: <local-queue>}`. Caesium stamps `kueue.x-k8s.io/queue-name` on the pod; Kueue gates scheduling against the queue's quota. Pure scheduling metadata — excluded from the cache hash. See [Delegating scheduling to Kueue](#delegating-scheduling-to-kueue) |

### Marking Replay-Safe Tasks

`replaySafe` is an operator-reviewed mark for quarantined what-if replay. Set it only on jobs or steps whose real command is safe to re-execute under replay. A job-level mark applies to every step:

```yaml
metadata:
  alias: backfill-preview
  replaySafe: true
```

A step-level mark applies to one task:

```yaml
steps:
  - name: summarize
    replaySafe: true
    image: alpine:3.23
```

Caesium records the effective value on the baseline `TaskRun` when that task runs. Later applies cannot retroactively authorize an older unsafe baseline, and `replaySafe` is excluded from the cache identity hash because it is control-plane metadata, not an execution input.

### Delegating scheduling to Kueue

Caesium does not bin-pack, prioritize, or gang-schedule — it delegates that to [Kueue](https://kueue.sigs.k8s.io/), the Kubernetes-native queueing controller. Set `kueue.queueName` on a `kubernetes` step and Caesium stamps the `kueue.x-k8s.io/queue-name` label on the pod; Kueue's webhook then gates the pod (via the `kueue.x-k8s.io/admission` scheduling gate it injects) until the named LocalQueue has quota, and un-gates it on admission.

```yaml
steps:
  - name: train
    engine: kubernetes          # kueue is rejected on docker/podman
    image: ghcr.io/acme/trainer:1.4
    kueue:
      queueName: data-eng       # an existing Kueue LocalQueue in the pod namespace
```

The queue is scheduling metadata, **not** an execution input, so it is excluded from the cache identity hash (like secrets and workload identity): changing the queue never busts the cache. The cluster must have Kueue installed with the LocalQueue and its backing ClusterQueue provisioned — see [`kubernetes-deployment.md`](kubernetes-deployment.md#delegating-scheduling-to-kueue).

### Volumes

Declare storage once and mount it from steps. Caesium mounts user-provided storage; it does not create external storage or copy file contents.

```yaml
volumes:
  - name: work
    sources:
      docker: {bind: /mnt/nfs/caesium-work}
      podman: {bind: /mnt/nfs/caesium-work}
      kubernetes: {pvc: ci-shared-rwx}
steps:
  - name: produce
    image: alpine:3.23
    command: ["sh", "-c", "echo data > /work/out.txt && echo '##caesium::output {\"path\":\"/work/out.txt\"}'"]
    volumeMounts: [{volume: work, path: /work}]
  - name: consume
    image: alpine:3.23
    dependsOn: [produce]
    command: ["cat", "$CAESIUM_OUTPUT_PRODUCE_PATH"]
    volumeMounts: [{volume: work, path: /work, readOnly: true}]
```

Docker/Podman source kinds are `bind`, `volume`, and `tmpfs`. Kubernetes source kinds are `pvc`, `claimTemplate`, and generic `volumeSource`.

### Callbacks

```yaml
callbacks:
  - type: notification
    configuration:
      webhook_url: "https://hooks.slack.com/services/..."
      channel: "#pipelines"
      user_agent: "caesium"
```

### Datasets & Freshness (opt-in)

Declare the datasets steps produce and consume plus a freshness SLO, and Caesium can schedule on data arrival and staleness instead of a cron guess. The same dataset block can carry producer and consumer JSON Schemas for cross-job contract enforcement. All of this is scheduling or apply-time metadata, excluded from the cache identity hash. Freshness is feature-gated behind `CAESIUM_FRESHNESS_ENABLED=true`; contract enforcement is server-gated by `CAESIUM_CONTRACT_ENFORCEMENT`.

```yaml
metadata:
  alias: freshness-vendor-orders
  datasets:
    sources:                        # external upstreams nobody in Caesium produces
      - name: raw.vendor_orders
        expectedEvery: 24h          # cadence expectation; a late drop is stale-upstream, not an error
        external: true
        arrival:                    # advance the source watermark on a matching event
          event: {type: "webhook.s3", filter: {bucket: vendor-drops}}
          watermark: "$.object.last_modified"    # JSONPath into the event payload
    skipWhenFresh: true             # default when datasets are declared: skip the cron run when already fresh
trigger:
  type: cron
  configuration: {cron: "*/30 * * * *"}
steps:
  - name: extract-orders
    image: alpine:3.23
    command: ["sh", "-c", "... && echo '##caesium::output {\"processed_through\": \"2026-07-04T00:00:00Z\"}'"]
    datasets:
      consumes: [raw.vendor_orders]
      produces:
        - name: analytics.orders_daily
          freshness: 6h             # target staleness SLO
          maxStaleness: 12h         # hard bound; a breach emits freshness_violated
          watermark: {key: processed_through}    # the ##caesium::output key that advances the dataset (an output key, not a JSONPath)
```

- `steps[].datasets.consumes` / `produces[]` — a consumed name may resolve to a dataset produced by another job; `produces[].name` is required, `freshness`/`maxStaleness` are Go durations, `watermark.key` names the emitted output key.
- Contract schemas: producers use `produces[].schema` for an inline JSON Schema or `produces[].schemaFrom: output` to reuse the step `outputSchema`; `produces[].version` is bumped for intentional breaking changes. Consumers may use the object form `consumes: [{name, schema}]` for the subset they require. Scalar consumes remain valid and create name-level edges only.
- `metadata.datasets.sources[]` — external upstreams; `arrival.watermark` is a JSONPath into the event payload; `external: true` tells cross-job lint not to demand a producing job.
- A purely data-derived job can drop cron with `trigger: {type: freshness}` (needs at least one consumed dataset and one produced dataset with `freshness`; declare it on the job, not via the trigger API). See the [generated reference](job-schema-reference.md#datasets--freshness).

### Cross-Job Contract Enforcement (server-gated)

Set `CAESIUM_CONTRACT_ENFORCEMENT=warn` or `fail` on the server; the empty default disables contract graph derivation and leaves `GET /v1/contracts/graph` unregistered. `CAESIUM_CONTRACT_DEPRECATION_WINDOW` defaults to `336h`. In fail mode, `POST /v1/jobdefs/apply` returns HTTP 409 for breaking findings scoped to the incoming apply. An unrelated pre-existing break does not block the apply, and a producer plus consumer migration in one batch passes without an acknowledgement.

Operator loop:

- `caesium job lint --server` and `caesium contract check --path jobs/ [--json]` check local manifests against persisted jobs.
- `caesium contract graph [--dataset ns/name] [--json]`, `GET /v1/contracts/graph`, and the Console `/contracts` route show the derived graph.
- `POST /v1/jobdefs/diff` returns per-job `contractFindings`; the Console JobDefs diff tab shows compatible/unknown/breaking badges with named consumers and teams.
- Intentional breaks use `caesium job apply --allow-breaking dataset=<name> --reason ...`. The acknowledgement is digest-scoped; producer and consumer applies warn during the deprecation window, then re-block after expiry because the window is evaluated at check time. The Console apply flow requires an ack reason before sending that request.

### Remediation (opt-in)

`metadata.remediation` opts a job into agent-in-the-loop incident remediation: a failing run opens an incident that a bounded, tiered, server-enforced policy (and a container-native agent) can retry, snooze, patch, or escalate — tier-3 actions always gated behind a human approval. Policy metadata; excluded from the cache identity hash. Feature-gated behind `CAESIUM_AGENT_REMEDIATION_ENABLED=true` with an active auth mode.

```yaml
metadata:
  alias: remediation-vendor-extract
  remediation:
    profile: vendor-extract-agent           # server-side AgentProfile (/v1/agentprofiles)
    classes: [transient_infra, data_unavailable, schema_violation, auth_failure]
    maxAttempts: 3
    autonomy:
      allow: [auto_retry_backoff, snooze_until_cron, notify, suppress_downstream_alerts]
      paramOverrides: {mode: [full, incremental]}    # keys must exist in trigger.defaultParams
      perClass: {auth_failure: {allow: [notify, escalate]}}
      requireApproval: [apply_jobdef_patch, override_schema_gate]
    escalation: {channel: data-oncall, after: 2h}
trigger:
  type: cron
  configuration: {cron: "15 3 * * *"}
  defaultParams: {mode: incremental}          # top-level; paramOverrides is validated against these
```

- `profile` (required) names a server-side `AgentProfile`; offline lint emits a scope note, server-side lint/apply verify it.
- `classes` (≥1): `transient_infra`, `schema_violation`, `sla_risk`, `data_unavailable`, `auth_failure`, `oom`, `quota`, `unknown`.
- Action names for `allow` / `perClass[].allow` / `requireApproval`: `auto_retry_backoff`, `snooze_until_cron`, `snooze_retry`, `retry_from_failure`, `retry_callbacks`, `notify`, `quarantine_replay`, `rerun_with_params`, `pause_job`, `unpause_job`, `clear_cache_entry`, `suppress_downstream_alerts`, `extend_sla_once`, `skip_task`, `override_schema_gate`, `apply_jobdef_patch`, `escalate`. A tier-3 action always creates an ApprovalRequest regardless of `allow`. See the [generated reference](job-schema-reference.md#remediation).

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
    image: alpine:3.23
    outputSchema:
      type: object
      properties:
        row_count: { type: integer }
        source: { type: string }
      required: [row_count, source]
    command: ["sh", "-c", "echo '##caesium::output {\"row_count\": 5000, \"source\": \"warehouse\"}'"]

  - name: transform
    image: alpine:3.23
    dependsOn: [extract]
    inputSchema:
      extract:
        required: [row_count]
        properties:
          row_count: { type: integer }
    command: ["sh", "-c", "echo Processing $CAESIUM_OUTPUT_EXTRACT_ROW_COUNT rows"]
```

Set `metadata.schemaValidation` to `"warn"` (log violations) or `"fail"` (fail task on violation).

### Large-Object Reference Passing

Scalar `##caesium::output` payloads are capped at 64 KB total. To pass something
larger — a Parquet file, a model artifact, a dataframe — do **not** inline it.
Instead, write the payload to a **mounted volume** (BYO shared storage; see
`volumes` / `volumeMounts`) and emit a *reference* with the content digest:

```
##caesium::output-ref {"key":"frame","path":"/data/out.parquet","digest":"sha256:<64-hex>","size":734003200}
```

- `key` (required): the output key, exactly like a scalar output key.
- `path` (required): where the producer wrote the payload (a path inside a
  mounted volume the downstream step also mounts).
- `digest` (required): `sha256:` + the lowercase-hex SHA-256 of the payload
  bytes. Compute it deterministically, e.g. `sha256sum /data/out.parquet`.
- `size` (optional): payload size in bytes (advisory; used only by the
  operator-side `CAESIUM_OUTPUT_REF_MAX_BYTES` guard).

Only the reference (path + digest) crosses the step boundary and lands in the
database — never the payload. The digest is folded into the downstream step's
cache key, so a producer that re-emits a **byte-identical** payload (hence an
identical digest) yields a **cache hit** downstream; a changed payload changes
the digest and forces a re-run. This is content-verified, not path-heuristic.

Downstream steps receive:

```
$CAESIUM_OUTPUT_<STEP>_<KEY>          # the volume path to read
$CAESIUM_OUTPUT_<STEP>_<KEY>_DIGEST   # sha256:… to re-verify the bytes
```

Example — producer writes to a shared volume and references it; consumer reads
it back:

```yaml
volumes:
  - name: shared
    source: { bind: { path: /srv/caesium/shared } }   # BYO storage
steps:
  - name: extract
    image: alpine:3.23
    volumeMounts:
      - { volume: shared, path: /data }
    command: ["sh", "-c", "dd if=/dev/zero of=/data/out.bin bs=1M count=128 && echo \"##caesium::output-ref {\\\"key\\\":\\\"frame\\\",\\\"path\\\":\\\"/data/out.bin\\\",\\\"digest\\\":\\\"sha256:$(sha256sum /data/out.bin | cut -d' ' -f1)\\\",\\\"size\\\":134217728}\""]

  - name: transform
    image: alpine:3.23
    dependsOn: [extract]
    volumeMounts:
      - { volume: shared, path: /data }
    command: ["sh", "-c", "test \"$(sha256sum $CAESIUM_OUTPUT_EXTRACT_FRAME | cut -d' ' -f1)\" = \"${CAESIUM_OUTPUT_EXTRACT_FRAME_DIGEST#sha256:}\""]
```

Notes:
- The reference protocol is **opt-in and BYO**: nothing breaks if you never use
  it, and it adds no mandatory storage dependency. The volume is yours.
- A malformed reference line (missing `key`/`path`, or a non-`sha256:<64-hex>`
  digest) is skipped like any malformed output line; the step's other outputs
  still apply.
- Caesium does not move bytes for you (no SDK): the container writes the file and
  computes the digest; Caesium records and propagates the reference.

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
caesium blame <job-id-or-alias>          # Attribute topology/image/command changes to commits/snapshots
caesium test --path jobs/               # Full validation suite
caesium test --scenario harness/        # Execute harness scenarios against the local runtime
```

`caesium blame` is intentionally scoped to the data stored in `dag_snapshot`:
topology, step image, and step command. It does not track behavior-only changes
to `env`, `spec`/node selectors, `retries`, `cache`, schemas, `sla`, or
`triggerRules`, so do not describe it as full behavior blame.

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
- `expect.lineage.impact[]`: assertions over the **persisted** dataset graph (what the `/lineage/impact` query reads), not just the emitted events. Each entry sets `dataset` (the root dataset name, e.g. `<job-alias>.<step>.output`), a `downstream` list of dataset names that must be reachable from it, an optional `maxDepth` (0 = unbounded), and an optional `namespace` (defaults to the harness namespace). Catches the class of bug where datasets are emitted but never stored, so impact comes back empty.

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
        impact:
          - dataset: nightly-etl.extract.output
            downstream:
              - nightly-etl.transform.output
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
    image: alpine:3.23
    command: ["sh", "-c", "echo setup"]
    next: [branch-a, branch-b]
  - name: branch-a
    image: alpine:3.23
    command: ["sh", "-c", "echo a"]
    dependsOn: setup
  - name: branch-b
    image: alpine:3.23
    command: ["sh", "-c", "echo b"]
    dependsOn: setup
  - name: join
    image: alpine:3.23
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
    image: alpine:3.23
    outputSchema:
      type: object
      properties:
        count: { type: integer }
        status: { type: string }
      required: [count, status]
    command: ["sh", "-c", "echo '##caesium::output {\"count\": 42, \"status\": \"ok\"}'"]
  - name: consume
    image: alpine:3.23
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
- [ ] Cross-job contract schemas use `produces[].schema` or `schemaFrom: output`; consumer requirements use `consumes[].schema` object entries when enforcement should compare fields
- [ ] Secrets use `secret://` URIs, never hardcoded values
- [ ] Durations use Go format: `30s`, `5m`, `1h`
- [ ] File is saved with `.job.yaml` extension for Git sync glob matching

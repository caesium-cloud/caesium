# Caesium Job Definitions

Caesium jobs can be authored as YAML manifests that follow the `job.v1` schema. Manifest files are typically stored in source control and applied to the platform via the importer tooling (see `internal/jobdef`).

To seed a local development environment with the examples referenced in this guide, start the server (`just run`) and execute:

```sh
just hydrate
```

The command mounts `docs/examples/` into a short-lived CLI container and applies each manifest via the REST API.

## Minimal Example

```yaml
$schema: https://yourorg.io/schemas/job.v1.json
apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
    timezone: "UTC"
steps:
  - name: extract
    image: alpine:3.20
    command: ["sh", "-c", "echo extracting data"]
  - name: transform
    image: alpine:3.20
    command: ["sh", "-c", "echo transforming data"]
  - name: load
    image: alpine:3.20
    command: ["sh", "-c", "echo loading data"]
```

## Explicit DAG with Callbacks

```yaml
$schema: https://yourorg.io/schemas/job.v1.json
apiVersion: v1
kind: Job
metadata:
  alias: csv-to-parquet
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
    timezone: "America/New_York"
callbacks:
  - type: notification
    configuration:
      webhook_url: "https://hooks.slack.com/services/T000/B000/XYZ"
      channel: "#data-pipelines"
      mention: "@oncall"
steps:
  - name: list
    engine: docker
    image: busybox:1.36
    command: ["sh", "-c", "echo listing s3://demo/csv/*.csv > /out/files.json"]
  - name: convert
    engine: docker
    image: busybox:1.36
    command: ["sh", "-c", "echo converting /in/files.json > /out/manifest.json"]
  - name: publish
    engine: docker
    image: busybox:1.36
    command: ["sh", "-c", "echo publishing /out/manifest.json to s3://demo/parquet/"]
```

## DAG Branching & Joins

```yaml
$schema: https://yourorg.io/schemas/job.v1.json
apiVersion: v1
kind: Job
metadata:
  alias: fanout-job
trigger:
  type: http
  configuration:
    path: "/hooks/run"
    secret: "redacted"
steps:
  - name: start
    engine: docker
    image: alpine:3.20
    command: ["sh", "-c", "echo run"]
    next:
      - branch-a
      - branch-b
  - name: branch-a
    engine: docker
    image: alpine:3.20
    command: ["sh", "-c", "echo task a"]
    dependsOn: start
  - name: branch-b
    engine: docker
    image: alpine:3.20
    command: ["sh", "-c", "echo task b"]
    dependsOn: start
  - name: join
    engine: docker
    image: alpine:3.20
    command: ["sh", "-c", "echo join"]
    dependsOn:
      - branch-a
      - branch-b
```

## Authoring Guidelines

- `apiVersion`/`kind` are fixed (`v1`, `Job`).
- `metadata.alias` must be unique per Caesium installation.
- `engine` defaults to `docker` if omitted.
- `next` accepts either a single string or a list, enabling fan-out to multiple successors. Use `dependsOn` to express joins/fan-in; both fields accept the step name(s) they reference.
- When no step declares `next` or `dependsOn`, the importer preserves the historical behaviour of linking each step to the following entry automatically. Once you opt into DAG fields, you are responsible for specifying the required edges explicitly.
- `callbacks.configuration` is stored as JSON. The built-in `notification` callback accepts `url`/`webhook_url` plus optional `headers` and `user_agent` keys.
- Callback payloads POST a JSON body containing job/run metadata (`job_id`, `job_alias`, `run_id`, `status`, `error`, `started_at`, `completed_at`) and task entries (`task_id`, `engine`, `image`, `command`, `status`, `runtime_id`, `error`).
- Callback attempts are recorded with status/error/timestamps so failed hooks can be inspected and retried (via `caesium run retry-callbacks --job-id <job> --run-id <run>` or the REST endpoint `POST /v1/jobs/:id/runs/:run_id/callbacks/retry`).
- `metadata.labels`/`metadata.annotations` are persisted and exposed through the REST API and CLI tooling.
- Steps can set container options directly on the manifest via `env`, `workdir`, and `mounts`. Environment values are passed to every runtime, while bind mounts map host paths (`source`) into the container at `target` (set `readOnly: true` when needed). These fields are optional and default to the runtime image configuration.

## Git-Based Synchronisation (Phase 2)

- Store job manifests under a repository path (e.g. `jobs/`).
- The importer (`internal/jobdef/git.Source`) clones the repository, walks the directory tree, and applies manifests. Provide `Source.Globs` to filter files (e.g. `**/*.job.yaml`); when omitted, all `.yaml`/`.yml` files are considered.
- Multi-document YAML files are supported—each document must describe a single Job.
- Duplicate aliases are rejected to prevent accidental overwrites (future work: allow `--force`).
- A `Watch` helper reuses a local working clone and performs periodic `fetch/pull` cycles. Configure `WatchOptions{Interval, Once}` to control frequency, optionally providing `Source.LocalDir` when you want to persist the checkout. Provide `Source.SourceID` to tag imported jobs with provenance metadata, and configure either Basic Auth credentials or SSH credentials backed by a secret resolver for private remotes.
- Imported jobs persist provenance information (source ID, repository URL, ref, commit, and manifest path) which will power future drift detection and pruning workflows.
- Triggers and atoms inherit the same provenance metadata; step-level records append `#step/<name>` and the trigger appends `#trigger` to the manifest path for precise drift tracking.

## Diffing Job Definitions

- Use `caesium job diff --path <dir>` to preview creates, updates, and deletes between local manifests and the database.
- Use `caesium job apply --path <dir>` to persist definitions into the database.
- Updated jobs include a unified diff showing the fields that will change.
- Run the diff command before applying changes to confirm the preview matches the expected plan.

## Secret References

- Sensitive values should be referenced using `secret://` URIs rather than inlining credentials inside manifests.
- Environment variables: `secret://env/VAR_NAME` (or `secret://env/path/to/name`, which expands to `path_to_name`). Use the `name` query parameter to override the derived variable name (`secret://env/foo?name=MY_VAR`).
- Kubernetes Secrets: `secret://k8s/<secret>/<key>` uses the default namespace; include the namespace as the first segment to target another namespace (`secret://k8s/infra/git-creds/token`). Query parameters `namespace`, `name`, and `key` override each component when needed.
- Vault KV paths: `secret://vault/<path>?field=<key>` resolves using the configured Vault client. If `field` is omitted, the final path segment is treated as the key (e.g. `secret://vault/secret/legacy/password`). Both KV v1 and v2 responses are supported.
- Secret resolvers are pluggable; Git sync and CLI tooling load values via the configured resolver chain so credentials never persist inside job manifests.

## Linting Definitions

- Run `caesium job lint --path <dir>` to validate manifests locally using the same semantic checks as the importer.
- Add `--check-secrets` to resolve every `secret://` reference using the configured resolvers. The command returns a non-zero exit code if any secret cannot be resolved.
- Secret resolution includes environment variables by default. Provide Kubernetes context through `--enable-kubernetes`/`--kubeconfig` (or `KUBECONFIG`) and Vault connectivity using `--vault-address`, `--vault-token`, and related flags or matching environment variables.
- Use the lint command in CI to gate manifest changes before pushing them to Git sync or applying them directly.
- Reference manifests live under `docs/examples/`; conformance tests load these files to ensure the documentation stays in sync with the schema.
- Example scenarios under `docs/examples/` include:
  - Minimal sequential Cron pipeline (`minimal.job.yaml`).
  - Callback-enabled workflow (`callbacks.job.yaml`).
  - Explicit edge wiring (`explicit-links.job.yaml`).
  - Fan-out/fan-in DAG (`fanout-join.job.yaml`).
  - HTTP-triggered debugging workflow (`http-ops-debug.job.yaml`).
  - Multi-document run history samples with success and failure cases (`run-history.job.yaml`).
  - Callback failure handling (`callback-failure.job.yaml`).

The CLI surfaces both `caesium job apply` and `caesium job lint`; REST automation is available via `POST /v1/jobdefs/apply`.

## Schema Tooling

- Run `caesium job schema --doc` to print the generated schema reference (also stored in `docs/job-schema-reference.md`).
- Append `--summary --path <dir>` to produce a conformance report that aggregates trigger types, engines, and callbacks used in the supplied manifests. Add `--markdown` to emit the report as Markdown for CI artifacts.

## Git Sync Configuration

- Enable continuous Git ingestion by setting `CAESIUM_JOBDEF_GIT_ENABLED=true` in the scheduler environment.
- Describe repositories via `CAESIUM_JOBDEF_GIT_SOURCES`, which accepts a JSON array of objects matching the importer fields (example below). Each entry supports optional per-source `interval` and `once` overrides, path filtering (`globs`), and credential configuration (`auth` for HTTPS, `ssh` for SSH remotes). When unspecified, `CAESIUM_JOBDEF_GIT_INTERVAL` (default `1m`) and `CAESIUM_JOBDEF_GIT_ONCE` provide global defaults.
- Secret providers are configured through dedicated variables: enable environment lookup with `CAESIUM_JOBDEF_SECRETS_ENABLE_ENV` (`true` by default), Kubernetes secrets with `CAESIUM_JOBDEF_SECRETS_ENABLE_KUBERNETES` plus optional `CAESIUM_JOBDEF_SECRETS_KUBECONFIG`/`CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE`, and Vault with `CAESIUM_JOBDEF_SECRETS_VAULT_ADDRESS` / `CAESIUM_JOBDEF_SECRETS_VAULT_TOKEN` / `CAESIUM_JOBDEF_SECRETS_VAULT_NAMESPACE` / `CAESIUM_JOBDEF_SECRETS_VAULT_CA_CERT` / `CAESIUM_JOBDEF_SECRETS_VAULT_SKIP_VERIFY`.
- Example `CAESIUM_JOBDEF_GIT_SOURCES` payload:

```json
[
  {
    "url": "https://github.com/yourorg/caesium-jobs.git",
    "ref": "refs/heads/main",
    "path": "jobs",
    "globs": ["**/*.job.yaml"],
    "source_id": "jobs-main",
    "interval": "2m",
    "auth": {
      "username_ref": "secret://env/GIT_USERNAME",
      "password_ref": "secret://vault/secret/data/git?field=password"
    }
  }
]
```

The scheduler now bootstraps the Git watcher automatically using these settings, reusing the same resolver chain as the CLI lint command so secret references remain out of manifests.

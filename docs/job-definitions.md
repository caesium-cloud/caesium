# Caesium Job Definitions

Caesium jobs can be authored as YAML manifests that follow the `job.v1` schema. Manifest files are typically stored in source control and applied to the platform via the importer tooling (see `internal/jobdef`).

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
    image: ghcr.io/yourorg/extract:2.0
    command: ["extract"]
  - name: transform
    image: ghcr.io/yourorg/transform:1.7
    command: ["transform"]
  - name: load
    image: ghcr.io/yourorg/load:0.9
    command: ["load"]
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
    image: ghcr.io/yourorg/s3ls:1.2
    command: ["s3ls", "s3://demo/csv/*.csv", "--out", "/out/files.json"]
  - name: convert
    engine: docker
    image: ghcr.io/yourorg/csv2pq:0.5
    command: ["csv2pq", "--in", "/in/files.json", "--out", "/out/manifest.json"]
  - name: publish
    engine: docker
    image: ghcr.io/yourorg/uploader:0.3
    command: ["upload", "--manifest", "/out/manifest.json", "--dest", "s3://demo/parquet/"]
```

## Explicit `next` Links

```yaml
$schema: https://yourorg.io/schemas/job.v1.json
apiVersion: v1
kind: Job
metadata:
  alias: explicit-links
trigger:
  type: http
  configuration:
    path: "/hooks/run"
    secret: "redacted"
steps:
  - name: build
    engine: docker
    image: ghcr.io/yourorg/build:1.0
    command: ["make", "build"]
    next: "test"
  - name: publish
    engine: docker
    image: ghcr.io/yourorg/publish:1.0
    command: ["publish"]
  - name: test
    engine: docker
    image: ghcr.io/yourorg/test:1.0
    command: ["make", "test"]
    next: "publish"
```

## Authoring Guidelines

- `apiVersion`/`kind` are fixed (`v1`, `Job`).
- `metadata.alias` must be unique per Caesium installation.
- `engine` defaults to `docker` if omitted.
- `next` links are optional; if absent, the importer links each step to the next item in the list.
- `callbacks.configuration` is stored as JSON and surfaced to callback handlers unchanged.
- `metadata.labels`/`metadata.annotations` are persisted and exposed through the REST API and CLI tooling.

## Git-Based Synchronisation (Phase 2)

- Store job manifests under a repository path (e.g. `jobs/`).
- The importer (`internal/jobdef/git.Source`) clones the repository, walks the directory tree, and applies manifests. Provide `Source.Globs` to filter files (e.g. `**/*.job.yaml`); when omitted, all `.yaml`/`.yml` files are considered.
- Multi-document YAML files are supported—each document must describe a single Job.
- Duplicate aliases are rejected to prevent accidental overwrites (future work: allow `--force`).
- A `Watch` helper reuses a local working clone and performs periodic `fetch/pull` cycles. Configure `WatchOptions{Interval, Once}` to control frequency, optionally providing `Source.LocalDir` when you want to persist the checkout. Provide `Source.SourceID` to tag imported jobs with provenance metadata, and configure either Basic Auth credentials or SSH credentials backed by a secret resolver for private remotes.
- Imported jobs persist provenance information (source ID, repository URL, ref, commit, and manifest path) which will power future drift detection and pruning workflows.

## Diffing Job Definitions

- Use `caesium job diff --path <dir>` to preview creates, updates, and deletes between local manifests and the database.
- Use `caesium job apply --path <dir>` to persist definitions into the database.
- Updated jobs include a unified diff showing the fields that will change.
- Run the diff command before applying changes to confirm the preview matches the expected plan.

Future work will expose CLI entrypoints (`caesium job apply`, `caesium job lint`) and optional REST endpoints once the Git sync workflow is battle-tested.

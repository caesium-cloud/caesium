# Job Definition Ingestion Roadmap & Checklist

## Goals
- [x] Support authoring, validation, and storage of job definitions in YAML format.
- [x] Provide user-friendly tooling (CLI/API) for pushing and managing job definitions.
- [x] Enable Git-based synchronization of job definitions.
- [x] Deliver metadata support for filtering and UI integration.

## Source Format
- [x] YAML documents conforming to the v1 schema with fields: `$schema`, `apiVersion`, `kind`, `metadata`, `trigger`, `callbacks`, `steps`.
- [x] Steps define DAGs of Atoms and Tasks with fields: `engine` (default: docker), `command` (JSON-encoded), and optional `next`.
- [x] Metadata labels and annotations captured for future use.

## Implementation Phases

### Phase 0 – Schema & Validation
- [x] Define Go structs in `pkg/jobdef` reflecting the schema.
- [x] Parse YAML into typed definitions with defaults.
- [x] Add semantic validation: API version/kind, trigger types, step uniqueness, DAG sanity.
- [x] Create unit tests covering valid and invalid manifests.

### Phase 1 – Import Pipeline (Push Model)
- [x] Implement importer translating definitions into persisted jobs, triggers, atoms, tasks, and callbacks with transactional guarantees.
- [x] Document usage and extend schema examples in `docs/job-definitions.md`.
- [ ] Implement safe update semantics with provenance enforcement and optional `--force` override (requires DB columns: jobs.provenance_source_id, jobs.deleted_at).
- [ ] Implement pruning workflow (`--prune`, protect labels, soft delete/GC) leveraging provenance metadata.
- [x] Add CLI commands:
  - [x] `caesium job apply` to push definitions from file/dir/stdin.
  - [x] `caesium job lint` for validation.
- [x] Expose REST endpoint `POST /v1/jobdefs/apply` for automation.

### Phase 2 – Git Source Integration (Optional)
- [x] Implement Git sync (`internal/jobdef/git.Source`) maintaining a working clone.
- [x] Walk manifests and apply them via the importer with deterministic ordering and per-document transactions.
- [x] Support configuration for credentials, polling interval, one-time sync, persistent working directory, and HTTPS Basic Auth.
- [x] Add secret resolver integration for Git credentials (PAT/SSH) and known_hosts enforcement.
- [x] Expose secret resolver configuration through runtime wiring (env/config) so Git sync can resolve secrets in production deployments.
- [ ] Explore webhook-triggered sync to complement polling (GitHub/GitLab/Bitbucket signatures).
- [x] Support path globs (e.g., `**/*.job.yaml`) and provenance recording (repo/ref/commit/path/source_id).
- [x] Persist imported provenance metadata onto jobs for Git-sourced definitions.

### Phase 3 – Enhancements
- [x] Add diff/preview tooling (`caesium job diff`) and status reporting.
- [x] Persist metadata labels and annotations (DB migrations and API updates).
- [x] Integrate imported job metadata into console UI.
- [x] Add secret resolver implementations (k8s/vault/env) with optional `--check-secrets` lint mode.
- [x] Wire a production-ready `SecretResolver` into Git sync configuration (env/config plumbing).
- [x] Extend provenance persistence to triggers/atoms once relevant drift tooling is in place.
- [x] Schema conformance reporting & documentation generation.

### Phase 4 – DAG Execution

*Objective:* upgrade the job engine from linear `next` chains to full DAG semantics so definitions can express fan-out, fan-in, and conditional branches while preserving provenance guarantees.

#### Schema & Validation
- [x] Update the v1 schema to allow multiple successors (`next`) and an explicit `dependsOn` list; keep backward compatibility with string `next`.
 - [x] Regenerate schema tooling and docs (`pkg/jobdef`, `cmd/job/schema.go`, `docs/job-definitions.md`) to surface the new shape.
- [x] Extend validation to detect cycles, missing dependencies, duplicate edges, and mixed fan-in/fan-out hazards.

#### Storage Model
- [x] Design and apply migrations introducing an edge table (e.g., `job_task_edges`) or adjacency representation that supports many-to-many relationships.
- [x] Update ORM/query helpers in `pkg/db` and related models to fetch DAGs efficiently with provenance metadata.

#### Importer & Provenance
- [x] Teach `internal/jobdef/importer` to materialize the expanded graph, creating edge records and validating joins atomically.
- [x] Ensure provenance columns capture edge-level source data so drift detection can reason about DAG shape.
- [x] Add unit tests covering multi-branch manifests and failure cases (cycle, orphan, duplicate).

#### Execution Engine
- [x] Update task scheduling (podman/docker engines) to respect dependency counts, enqueue successors only when all predecessors succeed, and record join failures.
- [x] Persist runtime state necessary to resume DAG execution after restarts (e.g., outstanding predecessor counts).
- [x] Persist `job_runs`/`task_runs` tables and a run service so restart-safe history can be queried via the API.
- [x] Cover new behaviour with executor-focused unit tests and integration scenarios.

#### API, CLI, & Console
- [x] Expose DAG structure via REST (`/v1/jobs/:id/tasks`, `/v1/jobs/:id/runs/:run_id/tasks`) and ensure CLI output (`caesium job get`, `caesium console`) reflects branching-specific successors and run history.
- [ ] Add serializer helpers so clients can render adjacency information cleanly (e.g., topological order, grouped successors).
- [ ] Update console UI planning docs and Bubble Tea surfaces to visualise branches.

- [x] Extend integration tests to import, execute, and verify multi-branch jobs (parallel success, join wait, failure short-circuit).
- [x] Add scenario coverage for Git-sourced definitions to ensure DAG edges survive sync/diff workflows.
- [x] Document authoring patterns (fan-out, join, conditional) with YAML examples and executor guarantees.

## Validation & Testing
- [x] Unit tests for parsing and validation.
- [x] Integration tests for importer verifying DB state.
- [x] Git sync suites covering single/multi-doc imports, watch loop, and update detection.
- [x] CLI lint command tests.
- [x] Schema conformance tests against example manifests.
- [x] Secret resolver mock tests and provenance/prune integration tests (once implemented).

## Open Questions & Decisions
- [x] Update vs replace strategy: default safe update with provenance/exec drift guards, `--force` override.
- [x] Pruning and deletion: prune by provenance with soft delete, protect labels, no deletion by default.
- [x] Secrets handling: store references only (secret:// URIs), resolve at runtime via pluggable resolvers.
- [x] Multi-job files: multi-document YAML supported; per-doc transactions with deterministic ordering and per-doc error reporting.
- [x] Git integration: use go-git with PAT/SSH support, polling + future webhook acceleration.

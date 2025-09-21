# Job Definition Ingestion Roadmap & Checklist

## Goals
- [ ] Support authoring, validation, and storage of job definitions in YAML format.
- [ ] Provide user-friendly tooling (CLI/API) for pushing and managing job definitions.
- [ ] Enable Git-based synchronization of job definitions.
- [ ] Deliver metadata support for filtering and UI integration.

## Source Format
- [x] YAML documents conforming to the v1 schema with fields: `$schema`, `apiVersion`, `kind`, `metadata`, `trigger`, `callbacks`, `steps`.
- [x] Steps define DAGs of Atoms and Tasks with fields: `engine` (default: docker), `command` (JSON-encoded), and optional `next`.
- [ ] Metadata labels and annotations captured for future use.

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
- [ ] *(Deferred)* Add CLI commands:
  - [x] `caesium job apply` to push definitions from file/dir/stdin.
  - [x] `caesium job lint` for validation.
- [ ] *(Deferred)* Expose REST endpoint `POST /v1/jobdefs/apply` for automation.

### Phase 2 – Git Source Integration (Optional)
- [x] Implement Git sync (`internal/jobdef/git.Source`) maintaining a working clone.
- [x] Walk manifests and apply them via the importer with deterministic ordering and per-document transactions.
- [x] Support configuration for credentials, polling interval, one-time sync, persistent working directory, and HTTPS Basic Auth.
- [x] Add secret resolver integration for Git credentials (PAT/SSH) and known_hosts enforcement.
- [x] Expose secret resolver configuration through runtime wiring (env/config) so Git sync can resolve secrets in production deployments.
- [ ] Explore webhook-triggered sync to complement polling (GitHub/GitLab/Bitbucket signatures).
- [x] Support path globs (e.g., `**/*.job.yaml`) and provenance recording (repo/ref/commit/path/source_id).
- [x] Persist imported provenance metadata onto jobs for Git-sourced definitions (triggers/atoms TBD).

### Phase 3 – Enhancements
- [x] Add diff/preview tooling (`caesium job diff`) and status reporting.
- [x] Persist metadata labels and annotations (DB migrations and API updates).
- [x] Integrate imported job metadata into console UI.
- [x] Add secret resolver implementations (k8s/vault/env) with optional `--check-secrets` lint mode.
- [x] Wire a production-ready `SecretResolver` into Git sync configuration (env/config plumbing).
- [x] Extend provenance persistence to triggers/atoms once relevant drift tooling is in place.
- [x] Schema conformance reporting & documentation generation.

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

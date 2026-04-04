# Design: Composable Task Templates

> Status: Proposed. This document covers reusable step templates with parameterized configuration.

## Problem Statement

Every step in a Caesium job is defined inline in the job YAML. Common patterns — running dbt, submitting Spark jobs, syncing S3 buckets, sending Slack notifications — are copy-pasted across job definitions. This creates:

- **Boilerplate**: Teams duplicate 10-20 lines of YAML per common operation across dozens of jobs.
- **Inconsistency**: Each copy drifts over time (different image tags, different env vars, different retry settings).
- **No reuse primitive**: There is no way to package a well-tested step configuration and share it across jobs or teams.

---

## Design

### Template Definition

A template is a YAML file that defines a parameterized step:

```yaml
apiVersion: v1
kind: Template
metadata:
  name: dbt-run
  version: "2"
  description: "Run dbt with project directory and target"
parameters:
  - name: project_dir
    type: string
    required: true
    description: "Path to dbt project"
  - name: target
    type: string
    default: "dev"
    description: "dbt target profile"
  - name: select
    type: string
    default: ""
    description: "dbt node selection"
  - name: image_tag
    type: string
    default: "latest"
    description: "dbt image tag"
spec:
  image: "dbt-runner:{{ .image_tag }}"
  command: ["dbt", "run", "--project-dir", "{{ .project_dir }}", "--target", "{{ .target }}"]
  env:
    DBT_PROFILES_DIR: "{{ .project_dir }}/profiles"
  retries: 2
  retryBackoff: true
  outputSchema:
    type: object
    properties:
      models_run: { type: integer }
      errors: { type: integer }
```

### Template Usage in Job Definitions

Steps reference templates via `templateRef` and provide parameter values via `with`:

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: daily-analytics
trigger:
  type: cron
  configuration:
    cron: "0 6 * * *"
steps:
  - name: run-dbt
    templateRef: templates/dbt-run:v2
    with:
      project_dir: /dbt/analytics
      target: prod
      select: "+orders"

  - name: sync-output
    templateRef: templates/s3-sync:v1
    with:
      source: s3://staging/output/
      destination: s3://warehouse/input/
    dependsOn: [run-dbt]

  - name: notify
    image: slack-notify:v1
    command: ["notify.sh"]
    dependsOn: [sync-output]
```

A step that uses `templateRef` must not also specify `image` or `command` — those come from the template. DAG fields (`dependsOn`, `next`, `triggerRule`) and overrides (`retries`, `cache`, `env`) are allowed and merge with template defaults.

### Template Resolution

Templates are resolved at **parse time** — during `caesium job lint`, `caesium job apply`, and `caesium dev`. The resolved step is a fully expanded inline step with no remaining template references. This means:

- The scheduler never sees templates — only resolved steps
- Templates don't need to be present at runtime
- YAML diffs show the fully resolved definition

Resolution order:
1. Load the template from the configured source
2. Validate that all required parameters are provided
3. Apply default values for optional parameters
4. Render the template spec using Go `text/template` with the parameter values
5. Merge step-level overrides (env, retries, cache, etc.) onto the rendered spec
6. Return the resolved step

### Template Sources

Templates can be loaded from three sources, in priority order:

#### 1. Local Files

Templates in the same directory or a relative path:

```yaml
templateRef: templates/dbt-run:v2
# Resolves to: ./templates/dbt-run.v2.template.yaml
```

File naming convention: `<name>.v<version>.template.yaml` or `<name>.template.yaml` (unversioned).

#### 2. Git Repositories

Templates in a remote git repository:

```yaml
templateRef: git://github.com/caesium-cloud/templates//dbt-run:v2
# Resolves to: clone repo, read dbt-run.v2.template.yaml from root
```

Git templates use the same clone/fetch mechanism as git-based job sync (`CAESIUM_JOBDEF_GIT_SOURCES`). Templates are cached locally after first fetch.

#### 3. Template Registry (Future)

A shared registry of community and organization templates:

```yaml
templateRef: registry://dbt-run:v2
```

The registry is deferred — local and git sources cover the immediate need. The registry is a future HTTP API that serves template YAML by name and version.

### Template Resolution Configuration

```bash
# Search paths for local templates (colon-separated)
CAESIUM_TEMPLATE_PATHS="./templates:../shared-templates"

# Git sources for remote templates
CAESIUM_TEMPLATE_GIT_SOURCES='[{"url":"https://github.com/org/templates.git","ref":"refs/heads/main"}]'

# Cache directory for fetched templates
CAESIUM_TEMPLATE_CACHE_DIR="/var/caesium/template-cache"
```

### Merge Semantics

When a step uses a template, step-level fields merge with template defaults:

| Field | Merge Behavior |
|-------|---------------|
| `image` | Template wins (step must not set) |
| `command` | Template wins (step must not set) |
| `args` | Template wins (step must not set) |
| `env` | Deep merge (step values override template values for same key) |
| `retries` | Step overrides template |
| `retryDelay` | Step overrides template |
| `retryBackoff` | Step overrides template |
| `cache` | Step overrides template |
| `outputSchema` | Template wins (defines the contract) |
| `inputSchema` | Step wins (depends on the job's DAG) |
| `mounts` | Append (step mounts added to template mounts) |
| `workdir` | Step overrides template |
| `dependsOn` / `next` | Step only (DAG is job-specific) |
| `triggerRule` | Step only |
| `nodeSelector` | Deep merge |

### Validation

Template validation happens at two levels:

**Template definition** (standalone validation):
- All `parameters` must have a `name` and `type`
- Required parameters must not have `default`
- `spec.image` is required
- Template expressions must parse without errors

**Template usage** (in-context validation during lint/apply):
- All required parameters must be provided in `with`
- Parameter types must match declared types
- The resolved step must pass all standard step validation rules

### Template Expression Language

Templates use Go `text/template` with a restricted function set:

- `{{ .param_name }}` — parameter substitution
- `{{ default .param_name "fallback" }}` — default values
- `{{ if .param_name }}...{{ end }}` — conditional sections
- `{{ .param_name | upper }}`, `{{ .param_name | lower }}` — string transforms

No arbitrary code execution. No file I/O. No shell commands. The template language is deliberately limited to parameter substitution and simple conditionals.

---

## Built-In Templates

Ship a set of common templates in the Caesium repository under `templates/`:

| Template | Description |
|----------|-------------|
| `dbt-run` | Run dbt with configurable project, target, and selection |
| `spark-submit` | Submit a Spark job to a cluster |
| `s3-sync` | Sync files between S3 paths |
| `pg-dump` | Dump a PostgreSQL database |
| `pg-restore` | Restore a PostgreSQL dump |
| `slack-notify` | Send a Slack notification with templated message |
| `http-request` | Make an HTTP request (webhook, API call) |
| `shell` | Run a shell script with configurable interpreter |

These serve as reference implementations and cover the most common use cases. They are included in the Caesium binary via embed and available without configuration.

---

## CLI Changes

```bash
# List available templates (local + git + built-in)
caesium template list

# Inspect a template (show parameters, description, spec)
caesium template inspect dbt-run:v2

# Validate a template definition
caesium template lint templates/my-template.yaml

# Render a template with parameters (for debugging)
caesium template render dbt-run:v2 --set project_dir=/dbt/project --set target=prod
```

### Integration with Existing Commands

`caesium job lint` and `caesium job apply` resolve templates automatically. The `--show-resolved` flag outputs the fully expanded YAML:

```bash
caesium job lint --path jobs/ --show-resolved
# Shows the job definition with all templateRef steps expanded inline
```

---

## Implementation Plan

### Phase 1: Core Template Engine (P2)

1. **Template schema**: `pkg/jobdef/template.go` — `Template` struct with metadata, parameters, spec
2. **Template parser**: Parse `*.template.yaml` files, validate parameter declarations
3. **Template resolver**: `internal/jobdef/template/resolve.go` — parameter substitution, merge semantics
4. **Local source**: Load templates from configured paths and embedded built-ins
5. **Job definition changes**: Add `templateRef` and `with` fields to step schema
6. **Lint integration**: Resolve templates during `caesium job lint` and `caesium job apply`
7. **Tests**: Resolution, merge semantics, parameter validation, missing required params

### Phase 2: Git Sources & Caching (P2)

8. **Git source**: Reuse `internal/jobdef/git/` cloning infrastructure for template repos
9. **Template cache**: Local file cache with version-keyed entries
10. **Periodic refresh**: Background fetch on configurable interval (reuse job sync pattern)
11. **Tests**: Git fetch, cache invalidation, version resolution

### Phase 3: CLI & Observability (P2)

12. **`caesium template list`**: Enumerate available templates across all sources
13. **`caesium template inspect`**: Show template details and parameter documentation
14. **`caesium template render`**: Debug aid for template expansion
15. **`caesium job lint --show-resolved`**: Output fully expanded job definition
16. **Template provenance**: Track which template (and version) was used in each step's resolved definition

### Phase 4: Built-In Templates (P2)

17. **Embed templates**: `templates/` directory with `//go:embed`
18. **dbt-run, s3-sync, slack-notify**: Initial set of 3-4 high-value templates
19. **Documentation**: Template authoring guide, parameter reference per built-in template

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Template injection (malicious parameter values) | Malformed or unexpected YAML | Go `text/template` does **not** auto-escape (only `html/template` does). Parameter values containing quotes, newlines, or YAML-significant characters can produce malformed output. Mitigations: (1) re-validate the fully rendered step as a legal step definition before use; (2) restrict allowed characters in parameter values via the parameter type system; (3) prefer structured YAML field injection over free-form string interpolation where possible. |
| Version conflicts (same name, different sources) | Wrong template used | Priority order is explicit: local > git > built-in. Warn on shadowing during lint. |
| Template drift (git source changes between lint and apply) | Unexpected behavior | Pin templates by version. Version is part of the cache key. `caesium job apply` re-resolves and warns if resolved output differs from lint. |
| Complexity creep in template language | Hard-to-debug templates | Deliberately limited expression set. No loops, no complex logic. Templates should be thin wrappers, not programs. |

---

## Examples

### Template Definition: `dbt-run.v2.template.yaml`

```yaml
apiVersion: v1
kind: Template
metadata:
  name: dbt-run
  version: "2"
  description: "Run dbt models with configurable project, target, and selection"
parameters:
  - name: project_dir
    type: string
    required: true
  - name: target
    type: string
    default: "dev"
  - name: select
    type: string
    default: ""
  - name: full_refresh
    type: boolean
    default: false
  - name: image_tag
    type: string
    default: "1.7"
spec:
  image: "dbt-runner:{{ .image_tag }}"
  command:
    - "dbt"
    - "run"
    - "--project-dir"
    - "{{ .project_dir }}"
    - "--target"
    - "{{ .target }}"
    {{ if .select }}- "--select"
    - "{{ .select }}"{{ end }}
    {{ if .full_refresh }}- "--full-refresh"{{ end }}
  env:
    DBT_PROFILES_DIR: "{{ .project_dir }}/profiles"
  retries: 2
  retryBackoff: true
  outputSchema:
    type: object
    properties:
      models_run: { type: integer }
      warnings: { type: integer }
      errors: { type: integer }
    required: [models_run, errors]
```

### Job Using Templates

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: daily-analytics
  cache: true
trigger:
  type: cron
  configuration:
    cron: "0 6 * * *"
    timezone: "America/New_York"
steps:
  - name: run-models
    templateRef: dbt-run:v2
    with:
      project_dir: /dbt/analytics
      target: prod
      select: "+orders +customers"

  - name: run-snapshots
    templateRef: dbt-run:v2
    with:
      project_dir: /dbt/analytics
      target: prod
      select: "snapshot:*"
    dependsOn: [run-models]

  - name: notify
    image: slack-notify:v1
    command: ["notify.sh"]
    env:
      CHANNEL: "#data-pipeline"
    dependsOn: [run-snapshots]
```

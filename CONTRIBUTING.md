# Contributing to Caesium

Thanks for your interest in contributing. This guide covers everything you need to get the project running locally, understand the codebase, and get a pull request merged.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Project Structure](#project-structure)
- [Getting Started](#getting-started)
- [Development Workflow](#development-workflow)
- [Code Conventions](#code-conventions)
- [Testing](#testing)
- [Submitting a Pull Request](#submitting-a-pull-request)
- [Adding a Feature](#adding-a-feature)

---

## Prerequisites

| Tool | Purpose | Install |
|---|---|---|
| Go ≥ 1.25 | Backend development | [go.dev](https://go.dev/dl/) |
| Node.js ≥ 20 | UI development | [nodejs.org](https://nodejs.org/) |
| Docker | Building images, running integration tests | [docs.docker.com](https://docs.docker.com/get-docker/) |
| just | Task runner for all build/test commands | `brew install just` or [casey/just](https://github.com/casey/just) |
| golangci-lint | Go linting | [golangci-lint.run](https://golangci-lint.run/usage/install/) |

---

## Project Structure

```
caesium/
├── cmd/                  # CLI entrypoints (start, job, run)
├── internal/             # Private application packages
│   ├── executor/         # Trigger polling and job queuing
│   ├── worker/           # Task claimer and concurrent worker pool
│   ├── jobdef/           # Manifest parsing, linting, DAG construction
│   ├── atom/             # Container runtime adapters (Docker/Podman/Kubernetes)
│   ├── trigger/          # Trigger interfaces (cron, HTTP)
│   ├── callback/         # Webhook notification execution
│   ├── lineage/          # OpenLineage event emission
│   ├── models/           # GORM ORM models
│   └── event/            # Internal event bus
├── pkg/                  # Public, reusable packages
│   ├── jobdef/           # Exported job definition schema
│   ├── client/           # HTTP client for the REST API
│   ├── db/               # Database connection pooling
│   ├── dqlite/           # dqlite consensus wrapper
│   └── log/              # Structured logging (Zap)
├── api/                  # HTTP server, REST controllers, GraphQL
├── ui/                   # React + TypeScript + Vite frontend
├── build/                # Dockerfiles for builder and runtime images
├── helm/                 # Kubernetes Helm chart
├── test/                 # Integration tests (build tag: integration)
└── docs/                 # User-facing guides and schema reference
```

Key files:

- `caesium.go` — binary entry point
- `justfile` — all build, test, and dev commands
- `.golangci.yml` — Go linter configuration
- `.circleci/config.yml` — CI/CD pipeline (CircleCI)

---

## Getting Started

### 1. Fork and clone

```bash
git clone https://github.com/<your-fork>/caesium.git
cd caesium
git remote add upstream https://github.com/caesium-cloud/caesium.git
```

### 2. Install UI dependencies

```bash
cd ui && npm install && cd ..
```

### 3. Build the project

```bash
# Build the Docker image for the host platform
just build
```

### 4. Run the server

```bash
just run
```

The server starts on `http://localhost:8080`. The web UI is at the root path.

### 5. Seed example jobs

```bash
just hydrate
```

---

## Development Workflow

Always branch from `develop`:

```bash
git fetch upstream
git checkout -b feat/my-feature upstream/develop
```

Use `develop` as the base for all PRs. `master` is the stable release branch and is only updated via merge from `develop` at release time.

### Useful commands

| Command | Description |
|---|---|
| `just run` | Start the server (Docker, port 8080) |
| `just rm` | Stop and remove the server container |
| `just unit-test` | Run Go unit tests (race detector + coverage) |
| `just ui-test` | Run React unit tests (Vitest) |
| `just lint` | Run `go fmt`, `go vet`, and `golangci-lint` |
| `just ui-lint` | Run ESLint on the React codebase |
| `just integration-test` | Run integration tests (requires Docker-in-Docker) |
| `just helm-lint` | Validate the Helm chart |
| `just helm-template` | Render Helm manifests for inspection |

---

## Code Conventions

### Go

- Format all code with `gofmt` (enforced by `just lint`).
- Package names are short, lowercase, single words — no underscores or camelCase.
- Unit tests live alongside the code they test in `_test.go` files.
- Integration tests live under `test/` and use the build tag `//go:build integration`.
- Use the structured logger from `pkg/log` — avoid `fmt.Println` or the standard `log` package.
- Configuration is read from environment variables via `pkg/env`. Do not hard-code values.
- Do not commit secrets. Use environment variables or reference a secrets manager.

### TypeScript / React

- All UI code lives under `ui/src/`.
- Components use Radix UI primitives and Tailwind CSS for styling.
- State management uses TanStack Query for server state and React state for local UI state.
- Routing uses TanStack Router.
- Run `just ui-lint` before committing — ESLint errors will fail CI.

### Git

- Use the imperative mood for commit subjects: `Add cron trigger validation`, not `Added...`
- Keep the subject under 72 characters.
- Reference related issues in the PR description, not necessarily in every commit.
- Squash fixup commits before requesting review.

---

## Testing

### Unit tests

```bash
just unit-test      # Go (with race detector)
just ui-test        # React (Vitest)
```

Aim for unit tests on any new logic in `internal/` or `pkg/`. Tests should not require external processes; mock or stub at the boundary.

### Integration tests

```bash
just integration-test
```

Integration tests start a real server and require Docker to be running. They use the `-tags=integration` build tag and live under `test/`. Add integration tests for anything that touches the database, container runtimes, or the HTTP API.

### Linting

```bash
just lint       # Go
just ui-lint    # React/TypeScript
just helm-lint  # Helm chart
```

All lint checks must pass before a PR can merge. The CI pipeline runs them automatically.

---

## Submitting a Pull Request

1. **Open an issue first** for non-trivial changes. Describe the problem and your proposed approach. This avoids wasted effort if the direction needs adjustment.

2. **Branch from `develop`** (see [Development Workflow](#development-workflow)).

3. **Write tests** for your changes. PRs without tests for new behavior will be asked to add them.

4. **Run the full check suite locally** before pushing:

   ```bash
   just lint && just ui-lint && just unit-test && just ui-test
   ```

5. **Open a PR against `develop`**, not `master`. Include:
   - A clear description of what the change does and why
   - A link to the related issue (if applicable)
   - Evidence of testing (test output, screenshots for UI changes)

6. **Respond to review feedback** promptly. Maintainers will typically respond within a few days.

---

## Adding a Feature

Here is a walkthrough for common contribution types.

### Adding a new trigger type

1. Define the trigger interface in `internal/trigger/`.
2. Implement the new type (e.g., `internal/trigger/webhook/`).
3. Register the trigger in the executor (`internal/executor/executor.go`).
4. Add the trigger type to the job definition schema (`pkg/jobdef/`).
5. Update the schema reference in `docs/job-schema-reference.md`.
6. Add unit tests alongside the implementation and an integration test under `test/`.

### Adding a new container runtime (atom)

1. Implement the atom interface in `internal/atom/<runtime>/`.
2. Register the runtime in the worker's runtime executor (`internal/worker/runtime_executor.go`).
3. Add any new configuration fields to `pkg/env/`.
4. Document the new runtime in `docs/job-definitions.md`.
5. Add integration tests that exercise the runtime end-to-end.

### Adding a REST API endpoint

1. Add the handler in `api/rest/controller/<resource>/`.
2. Bind the route in `api/rest/bind/bind.go`.
3. Update the client package (`pkg/client/`) if the endpoint should be accessible via the Go client.
4. Add integration tests in `test/`.

### Adding a UI page or component

1. Add the component under `ui/src/`.
2. Register any new routes in `ui/src/router.tsx`.
3. Use TanStack Query for data fetching and Radix UI primitives for accessible components.
4. Run `just ui-lint` and `just ui-test` before pushing.

---

## Questions

If something in this guide is unclear or you run into a problem not covered here, open a [GitHub Discussion](https://github.com/caesium-cloud/caesium/discussions) or file an issue. We are happy to help.

# Planning a Caesium initiative

## Establish the contract

Read the requested design and the relevant part of `docs/roadmap.md`. Name the
source of truth in the plan and explain how conflicting documents are resolved.
The current code describes today's behavior; it does not veto an explicitly
requested schema or product change. If no design exists, state which user
requirements anchor the plan and which decisions remain open.

Use `rg --files` and focused `rg` searches to find current examples. Do not
depend on ignored personal plans, fixed historical commits, or a contributor's
home directory. Read sibling active and completed plans relevant to the same
initiative; cross-link existing ownership instead of copying their backlog.

## Inspect the complete path

Use these as investigation pointers, not a mandate to touch every surface:

| Change | Follow through |
| --- | --- |
| Persistent state | Model, migration/registration in `internal/models/` and `pkg/db/`, catalog versus hot-table routing, write ownership, reload/replay, retention |
| Background processing | Implementation under `internal/`, startup wiring in `cmd/start/start.go`, config gate, cancellation and shutdown |
| REST surface | Controller and service, route binding, authentication and role/scope policy, real HTTP integration scenario |
| CLI command | Cobra registration, client/API wiring, stdout/stderr behavior, binary-driven scenario in `test/` |
| Execution-affecting YAML | `pkg/jobdef/definition.go` including custom unmarshalling, schema generation, runtime conversion, affected Docker/Podman/Kubernetes adapters, `internal/cache/hash.go`, examples and schema docs |
| Metrics | Collector definition and registration in `internal/metrics/`, actual emitter, test of observed metric |
| Configuration | `pkg/env/env.go`, validation, server wiring, integration-server environment, operator docs |
| UI | Feature component, API client, route/navigation and capability gating, observable browser scenario |

Confirm these pointers against the checkout before listing exact `Files:`.
Avoid absolute rules about migrations or APIs inferred from a single old
feature. For job YAML, read `docs/caesium-job-llm-reference.md` as required by
`AGENTS.md`.

For stateful work, acceptance must follow input through persistence and the
public read path, including the relevant retry, restart, or stale-writer case.
For new CLI commands and REST queries, plan integration tests in `test/` that
invoke the binary or live HTTP endpoint. Machine-readable CLI output must be
parsed from stdout captured separately from stderr (`runCLIStdout`). A
config-gated feature must be enabled on the test server so the scenario runs.

## Items and ownership

Use lettered streams (`A`, `B`, ...), item IDs such as `A1`, `A2`, `B1`, and
optional `H-1` for harness work or `N-1` for independent navigation work. Keep
IDs stable in rewrites; references may exist outside this file. Plan stream
letters are distinct from execution labels such as `W1-α`.

An item should be reviewable and have a meaningful result; a stream may ship
several tightly coupled items in one PR. Do not split implementation from its
tests or required behavior docs just to increase parallelism.

```markdown
- [ ] A1. Persist the requested state and expose it through the existing query.
  Files: <confirmed implementation paths>, new test/<scenario>_test.go.
  Depends on: none.
  Verify: <action through a live surface and observable result>.
```

Dependencies may refer to in-plan IDs or explicitly identified external work.
For external work, record the owning plan/PR and a verifiable readiness
condition. A checked box without supporting evidence may need auditing. Do not
schedule an item whose prerequisite is only in an unmerged sibling branch
unless the execution request explicitly calls for stacked PRs.

High-conflict surfaces include `pkg/jobdef/definition.go`, model registration,
`pkg/db/` routing, `cmd/start/start.go`, `api/api.go`, route/CLI registration,
`pkg/env/env.go`, metric registration, UI route/client/navigation files, module
files, and shared documentation. Bundle overlapping semantic edits or specify
an order. Separate append-only edits still need an integration owner and a
merge order. Resolve module version choices deliberately and regenerate sums
with the repository's toolchain; a blind union is not a dependency strategy.

Keep behavior docs with the stream that changes behavior. The orchestrator
owns the shared Progress dashboard; workers own only assigned item checkboxes
and notes. A checked item in an open PR is proposed completion, not proof that
it has shipped.

## Verification and completeness

Use `AGENTS.md`, `justfile`, `.github/workflows/ci.yml`, and `docs/ci.md` to choose
the actual gates. Preserve explicit checks in a user-supplied plan. For runtime
changes the baseline is `just lint`, `just unit-test`, and
`just integration-test`; unit tests do not compile the integration-tagged
suite. Add UI, auth, distributed/owner-memory, Podman, Helm/Kubernetes, or
reagent checks when their paths are affected. Discover nested `go.mod` files:
the root module's `./...` does not cover nested modules.

Build through containerized recipes. Tests must exercise the new behavior,
not merely import a handler, seed its expected DB rows, or skip on a disabled
feature. Infrastructure prerequisites and test-resource conflicts belong in
the plan. See the paired skill's
[verification reference](../../exec-plan-wave/references/verification.md) for
execution details.

Write one or more observable acceptance criteria for each included capability,
plus the cross-cutting requirement that Progress and relevant docs match
verified merged behavior. Deferred work remains visibly deferred; it is not
checked complete or silently dropped to make the plan appear finished.

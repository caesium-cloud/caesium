# Verify the actual candidate

## Select gates from the checkout

Read current `AGENTS.md`, the plan's Verification and Acceptance Criteria,
`justfile`, `.github/workflows/ci.yml`, and `docs/ci.md`. Explicit user and plan
checks take precedence. Use this table to select additional relevant checks;
verify recipe names and lane setup in the current checkout before execution.

| Changed surface | Verification |
| --- | --- |
| Runtime Go/API/CLI/schema, DB, dependency, build or test wiring | `just lint`, `just unit-test`, `just integration-test` |
| `ui/` | `just ui-lint`, `just ui-test`, `just ui-e2e`; add Go baseline for API/embed changes |
| Auth, role/scope, agent API | Baseline plus `just integration-test-agent` and relevant `just ui-e2e-auth` coverage |
| Distributed or owner-memory behavior | Baseline plus affected `just integration-test-distributed` / `just integration-test-owner-memory` lanes |
| Podman adapter | Baseline plus `just integration-test-podman` |
| Helm/Kubernetes | `just helm-lint`, `just helm-template`, and relevant real-cluster scenario from CI; baseline for Go/runtime changes |
| Infrastructure/reagents | Applicable `just reagents-lint`, `just reagents-test`, `just integration-test-infra` and runtime baseline |
| Job YAML contract | Runtime baseline, schema tests, and example lint via the container-built Caesium CLI |
| CI workflow/filter/gate changes | Workflow/config validators and regression commands named in `docs/ci.md` and the workflow; prove selected lanes actually execute |
| Only Markdown/skill metadata | Frontmatter, referenced paths/links and generated template checks; exercise added helpers or discovery as applicable, plus explicit requested/repository gates |

Do not report a Helm render as a cluster test, a focused test as a full suite,
or a docs-only CI path skip as application integration evidence. Discover
nested `go.mod` files and run their relevant recipes separately: the root
module's `./...` does not cover them.

## Drive the real surface

Every new CLI command and REST query needs an integration scenario in `test/`
using the built CLI binary or the live HTTP server. The test must observe the
behavior that could fail in production. An internal-handler test or hand-seeded
expected DB row does not prove wiring or persistence. Stateful features need
the relevant input-to-write-to-read path, including retry/restart cases when
the contract promises them.

For machine output, capture stdout separately from stderr using
`runCLIStdout`, then parse stdout and assert the expected result. Enable gated
features on the integration server and runner wherever the harness requires
them. Check that the intended tests ran and were not all skipped or excluded
by an incorrect filter. A root unit-suite pass does not compile `test/`, which
is behind the integration build tag.

## Own the environment

Run commands in the assigned candidate worktree with an explicit workdir.
Caesium's `justfile` mounts the current checkout; there is no requirement to
switch the main checkout to a PR branch. Use the containerized build/test
recipes, not host `go build`. Format changed Go files explicitly and inspect
any additional changes produced by recipes.

Check `just` and the configured container CLI/daemon before scheduling gates.
Probe actual permissions once; do not assume Codex lacks Docker or network
access. An unavailable daemon, missing dqlite toolchain, or unreachable cluster
is an environment blocker and must be reported separately from code failures.
Keep cluster operations within the authorized local test context.

Integration recipes use shared container names and ports, and builds can share
image tags. Serialize gates/builds that share these resources, including UI and
engine lanes. Coordinate with any existing user/test process before invoking
an `up` recipe that removes or replaces a container. A different worktree alone
does not isolate Docker. Stop/clean only resources this wave owns.

Use fresh candidate images. Do not use `CAESIUM_SKIP_IMAGE_BUILD=true` or a
precompiled integration runner unless its provenance matches the candidate
being tested. A stale cached image can hide the entire change. Record SHA,
worktree, relevant lane/env settings, commands, exit codes, and log locations.
Do not record secrets. If a test creates changes, review and reconcile them
before binding its result to the final commit.

## Interpret results and revalidate

Required checks are passing, failing, or unverified; an unavailable check is
never passing. Capture the actual failure and relevant server logs. Compare a
baseline only when it helps distinguish a patch regression from an environment
issue. Fix real in-scope failures, then rerun the affected check and any gates
invalidated by the fix. Do not delete assertions, lower test floors, disable
feature gates, or bypass CI to obtain green output.

Review fixes, conflict resolution, dependency updates, and relevant base changes
invalidate prior evidence. Revalidate the resulting candidate and its current
CI runs. A final repeated suite is useful when integration changed the code or
an unresolved concern remains; otherwise reuse already valid evidence and
finish the workflow. Check every required architecture/lane from the actual
workflow; one local platform cannot stand in for all CI lanes.

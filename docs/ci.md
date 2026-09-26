# CI Runbook

This is the operator reference for `.github/workflows/ci.yml`: which checks
gate a merge into `master`, which checks gate a `v*` tag publish, the job
matrix and its per-lane server env, and the release procedure. For the
general execution-mode / worker / dqlite env reference (what each
`CAESIUM_*` variable does), see
[parallel-execution-operations.md](parallel-execution-operations.md) — this
doc does not repeat that material, only the CI-specific wiring.

Shipped W1 load reporting and browser evidence; the W2 owner-crash robustness
runner, integration SQL-work budget and scenario evidence validator; the W3
`early-evidence` lane and its promotion into `ci-ok`, pure reference models,
developer-journey CLI scenarios and browser accessibility/visual/scale/recovery
coverage; the W4 targeted-fault harness, open-loop load driver, native fuzz
campaigns, single-node previous-release upgrade qualification, and
merge-candidate identity / `merge_group` wiring; the W5 fenced core-failure
suite, coverage-instrumented CLI/server collection, and fail-closed
base/candidate performance comparator. W6's checker-strength validator,
core-failure acceptance repair, persistent-cluster lifecycle runner, common
performance benchmark harness, same-cap snapshot memory sampler (#574) and
console owner-crash journey (#575) are tracked in
[Distributed Testing and Performance Confidence](exec-plans/active/distributed-testing.md).
The console journey is merged code with no live kind proof. Calibrated budgets
remain planned. The F2 cluster runner and E3 comparator still lack complete
live acceptance.

W3 added one job to the workflow (`early-evidence`) and two dependencies to
`ci-ok` (`early-evidence` and `helm-lint`). W4 added **no jobs and no `ci-ok`
dependencies**: G7 wired the `merge_group` trigger and candidate-identity
checks on the existing aggregate. W5 added **no jobs and no `ci-ok`
dependencies** either: `TestCore`, the coverage collector and
`scripts/performance.sh` are local commands. W6 also added **no jobs or
aggregate dependencies**: the C3 validator and F2 cluster runner remain
standalone. None of these waves changed
repository settings:
`ci-ok` is still absent from master's required status checks, so that
promotion gates `v*` publication rather than PR merge, and `merge_group` is
dormant until a ruleset exists. See §1 and "Early evidence lane and the
promoted gate" / "Candidate identity and merge-group wiring" in §4.

## 1. Required-to-merge checks

`ci-ok` is the aggregate merge gate to enable in branch protection. It
always runs (`if: always()`), requires successful change detection, and
rejects failed, cancelled, missing, or unexpectedly skipped required jobs.
A skip is accepted only when the successful `changes` job explicitly
reports that the job's paths were not touched. Missing filter outputs fail
closed. `scripts/test_ci.py` verifies these selectors against the workflow.

Jobs `ci-ok` evaluates (several may skip on a narrow PR):

- `changes` and `ci-config` (workflow lint and CI regression checks)
- `builder` / `builder-arm64` / `images` / `images-arm64` / `reagents`
  (producers, including both native static CLI smoke tests)
- `lint`, `unit-test`, `unit-test-arm64`
- `ui-test`, `ui-e2e`, `ui-e2e-auth`
- `integration` (all three Docker shards and the agent-auth lane)
- `helm-lint` and `early-evidence` (distributed-testing G5; see §4)

`early-evidence` is not satisfied by a green job result. `ci-ok` downloads the
evidence report that lane uploaded and `scripts/ci-ok.py` re-validates it
against the committed scenario manifest, bound to this run's `github.sha`; an
absent, foreign or hollow report fails the gate. `helm-lint` is in the set
because it is `early-evidence`'s unconditional producer — without it a chart
failure would surface only indirectly, as `early-evidence=skipped`.

**`ci-ok` is not enforced at merge today.** Read at execution time on
2026-09-14, master's protection lists eight required contexts — `lint`,
`unit-test`, `unit-test-arm64`, `ui-test`, `ui-e2e`, `ui-e2e-auth`,
`build-and-integration-test`, `build-and-integration-test-agent-auth` — with
`strict: false`, `enforce_admins: false`, one CODEOWNER review, and no
rulesets. `ci-ok` is absent from that list, so it gates only `publish`
(`publish.needs`, §3) and the aggregate's new evidence checks block a `v*` tag
rather than a merge. Running the PATCH command below is the one change that
makes `ci-ok` — and therefore `early-evidence` — merge-blocking; it is a
repository-settings change for the CODEOWNER, not a workflow change. Note also
that with `enforce_admins: false` an admin merge bypasses the eight required
contexts, so required-check status is a review convention as much as a hard
gate.

The existing `build-and-integration-test` and
`build-and-integration-test-agent-auth` check names remain as lightweight,
always-running checks over `changes`, `images`, and the `integration`
matrix. Both reject an upstream failure or an unexpected skip. Existing
branch protection therefore keeps working before `ci-ok` is enabled; no
repository settings are changed by the workflow.

`ui-e2e-auth` is in that set — and not merely nice-to-have — because it
is the **only** job in the whole workflow that exercises a scoped API key
against `GET /auth/whoami` and asserts the 200
(`ui/e2e/auth/auth-smoke.spec.ts`, "a job-scoped key is denied the global
whoami" pins the *deny* case, and the paired auth-mode assertion pins the
*allow* case for a workspace-scoped key). No unit test, no other
integration lane, and no other e2e project runs that request. If
`ui-e2e-auth` is ever dropped from the `ci-ok` needs list, that assertion
silently stops gating merges.

`strict` controls whether the PR branch must be up to date with its base;
it does not control skipped jobs. GitHub accepts a job skipped by an `if`
condition as successful for branch protection. The aggregate gate checks
whether each skip was expected. During migration, retain the existing
`strict: false` policy; enable strict mode if desired when `ci-ok` becomes
the sole required context. G7 (distributed-testing W4) reconfirmed these
settings on 2026-09-16 and 2026-09-17 and did **not** change them.
`allow_update_branch: true` is an optional convenience for *non-strict*
protection: it lets a behind head be updated even when being up to date is
not required. It is **not** a prerequisite for `strict: true` — once
branches must be current, GitHub's own up-to-date requirement permits the
update flow, subject to permissions and conflicts. A merge queue is the
other Q6 option; `merge_group` is already wired and has never fired — see
"Candidate identity and merge-group wiring" in §4. No settings command in
this document was executed by W4.

### Command

```sh
mkdir -p .tmp
cat > .tmp/required-checks.json <<'EOF'
{
  "strict": false,
  "checks": [
    {"context": "ci-ok"},
    {"context": "lint"},
    {"context": "unit-test"},
    {"context": "unit-test-arm64"},
    {"context": "ui-test"},
    {"context": "ui-e2e"},
    {"context": "ui-e2e-auth"},
    {"context": "build-and-integration-test"},
    {"context": "build-and-integration-test-agent-auth"}
  ]
}
EOF
gh api -X PATCH repos/caesium-cloud/caesium/branches/master/protection/required_status_checks \
  --input .tmp/required-checks.json
```

`checks[].context` (with an optional sibling `app_id` to pin a specific
Checks-API app) is the current GitHub REST shape for
`required_status_checks`; the legacy `contexts` array is deprecated and is
not used here.

End state, after `ci-ok` has been the merge gate on a handful of PRs:

```sh
mkdir -p .tmp
cat > .tmp/required-checks.json <<'EOF'
{
  "strict": true,
  "checks": [
    {"context": "ci-ok"}
  ]
}
EOF
gh api -X PATCH repos/caesium-cloud/caesium/branches/master/protection/required_status_checks \
  --input .tmp/required-checks.json
```

### Verification

```sh
gh api repos/caesium-cloud/caesium/branches/master/protection --jq '.required_status_checks.checks[].context'
```

Expected output includes `ci-ok` plus the existing eight contexts
(transition), or only `ci-ok` (end state). The two legacy integration
contexts aggregate the matrix; compilation lives in `images` / `reagents`.

## 2. Why distributed / owner-memory / podman / helm are not required yet

`helm-lint` is a different job from `helm-integration-test` and **was**
promoted: distributed-testing G5 added it to `ci-ok` because it is the
`early-evidence` lane's unconditional producer. `helm-integration-test` itself
stays unpromoted under the criterion below, as do the other lanes named here.

Acceptance Criterion 4 requires the last 5 master runs after D1/H-2 to have
no failure in the L12 lanes that is not a filed, linked quarantine — not zero
history of flakiness. These four lanes (plus the two arm64 infra lanes) carry
real, recently-fixed flake history and stay out of the required set until
they've proven stable in practice, not just in a single green run:

- **`build-and-integration-test-owner-memory`** — `TestFanOutHTTPRetryPartition`
  ("a reset instance never ran again") was root-caused to
  `OwnerManager.Drop` re-checkpointing a stale "run is complete" snapshot
  after `Store.invalidateRunState` discarded it, so a retry's reset could be
  silently overwritten. Fixed in Stream D1(a) (PR #422) by routing
  invalidation through `OwnerManager.Release` (mark-stale before
  checkpoint-delete) plus a per-run invalidation epoch that refuses a stale
  rebuild. Three regression tests in `internal/run/owner_invalidation_test.go`.
- **`podman-integration-test`** — `TestGenericUnitPipelineCachesPerUnit`
  drifted red because the lane's own inline `docker run -e` block (it does
  not go through `just integration-up`) was missing
  `CAESIUM_DATABASE_SHARDS` and `CAESIUM_FANOUT_MAX_PARTITIONS`, both present
  on `integration-up`. Closed in Stream D1(b) (PR #422) by adding the two
  vars to the podman block, with a comment marking it as a superset of
  `integration-up` going forward.
- **`helm-integration-test`** — the two W1 reds were `TestRunRetryCallbacksCLI`
  failing when the retry callback receiver was unreachable from the kind
  cluster; fixed by skipping that scenario in that topology (PR #393).
- **`unit-test`** — `no such vfs` (empty VFS name) in
  `internal/trigger/event`'s parallel router tests was a real product bug,
  not test flakiness: `go-dqlite`'s `init()` puts SQLite into single-thread
  mode process-wide, which disables the static mutex guarding SQLite's
  one-time `sqlite3_initialize()`, so two `t.Parallel()` tests opening their
  first connection at once could race that initialization. Fixed in Stream
  D1(c) (PR #422) with `ConfigMultiThread()` in `pkg/dqlite/threading.go`,
  which fixes every package's first-open race, not just this test's.

None of these are quarantined — each has a real fix landing in PR #422 (D1),
not a `t.Skip`.

**Promotion criterion:** a lane is promoted from this list once it has 10
consecutive green `master` runs with no failure that is not a filed,
non-quarantined issue. Promotion means three edits in the same PR: add the
job id to the `checks` list in the PATCH body above and re-run the command,
add it to this doc's "Required-to-merge checks" list, and update Acceptance
Criterion 4 in `docs/exec-plans/completed/trust-the-substrate.md` to match —
the three must never drift apart.

`build-and-integration-test-distributed`,
`build-and-integration-test-infra`, `build-and-integration-test-infra-arm64`,
and `build-and-integration-test-arm64` are not required either; they weren't
named in a recent master-red incident, but they haven't accumulated 10
consecutive green runs' worth of evidence as a *required* gate and stay out
under the same criterion until they do.

## 3. Required-to-merge vs. required-to-publish

These are two different, deliberately different-sized sets. A green required
set is **not** proof that a commit is safe to tag and publish.

- **Required-to-merge** (§1 above, `ci-ok`): the fast gate every PR must
  clear. Path filters may skip jobs that a given PR does not touch.
- **Required-to-publish** (`publish.needs` in `.github/workflows/ci.yml`,
  quoted verbatim below): the full lane matrix, including image producers
  and the engine/mode lanes. `publish` only triggers
  `if: startsWith(github.ref, 'refs/tags/v')`, so it isn't itself a
  PR-merge gate. Tag pushes do not path-filter (`changes` forces every
  output to `true`).

```
ci-ok
lint
unit-test
unit-test-arm64
ui-test
ui-e2e
ui-e2e-auth
images
images-arm64
reagents
reagents-arm64
integration
integration-extra
integration-arm64
helm-lint
helm-integration-test
helm-pod-replacement-test
podman-integration-test
```

Concretely: a PR can merge into `master` on the strength of `ci-ok`
alone, while `integration-extra` (distributed / owner-memory / infra),
`integration-arm64`, `helm-lint`, `helm-integration-test`,
`helm-pod-replacement-test`, and `podman-integration-test` — exactly the
lanes §2 leaves non-required —
may have run on that same commit but did not block the merge. Do not
assume a green required set means `master`'s tip is taggable: before
pushing a `v*` tag, check that every job in `publish.needs` is green on
the commit you intend to tag (see §6). Arm64 product, reagents, and
integration run on every PR in parallel with the amd64 twins; they are
required-to-merge for the builder, product/static CLI smoke, and
`unit-test-arm64`; the arm64 integration matrix remains optional.

## 4. Job matrix and artifact flow

Triggers: `pull_request` to `master`, `push` to `master`, `v*` tags, and
`merge_group` (`types: [checks_requested]`). Feature-branch pushes do **not**
run CI (the PR event covers them). A `concurrency` group cancels superseded
PR runs and keys `pull_request` / `merge_group` into disjoint namespaces
(`pr-<number>` vs `mergegroup-<head_sha>`) so a queued run cannot share a
slot with, or be cancelled by, a PR run. No merge queue exists today, so
`merge_group` has never fired — see "Candidate identity and merge-group
wiring" in this section.

Each compiled artifact is produced once per architecture and loaded by
consumers. `CAESIUM_SKIP_IMAGE_BUILD=true` on the integration recipes
reuses the loaded product images. A missing product or explicitly selected
integration runner fails instead of rebuilding. Local runs compile current
sources unless a precompiled runner is explicitly selected.

```
changes
  ├─ ui-test
  ├─ reagents ────────────────────── lint, unit-test (also need builder)
  ├─ builder ─── images ─────────┬─ integration (3 Docker shards + agent-auth)
  │                              ├─ ui-e2e, ui-e2e-auth (also need ui-test)
  │                              ├─ helm/podman (3 full-suite shards each)
  │                              ├─ early-evidence (also needs helm-lint)
  │                              └─ integration-extra (also needs reagents)
  ├─ reagents-arm64 ───────────────────┐
  └─ builder-arm64 ─┬─ unit-test-arm64  │
                    └─ images-arm64 ───┴─ integration-arm64
ci-config → workflow and gate regression checks
ci-ok ← required-to-merge set
publish ← tag only, needs the full matrix and ci-ok
```

| Job id | runs-on | needs | timeout (min) | justfile recipe(s) / inline | Server started |
| --- | --- | --- | --- | --- | --- |
| `changes` | ubuntu-24.04 | — | 5 | path filter (`dorny/paths-filter`) | none |
| `builder` | ubuntu-24.04 | `changes` | 30 | `docker/build-push-action` `builder-full` (GHA cache) | none |
| `builder-arm64` | ubuntu-24.04-arm | `changes` | 30 | arm64 twin of the above | none |
| `images` | ubuntu-24.04 | `builder` | 30 | `build/ci.docker-bake.hcl` `product`, `build-triage-agent`, CLI smoke | none |
| `images-arm64` | ubuntu-24.04-arm | `builder-arm64` | 30 | arm64 twin of `images` (parallel) | none |
| `reagents` | ubuntu-24.04 | `changes` | 20 | `build/ci.docker-bake.hcl` `reagents` (parallel with `images`) | none |
| `reagents-arm64` | ubuntu-24.04-arm | `changes` | 20 | `reagent-roles` bake group (no unused arm64 lint/test toolchain) | none |
| `lint` | ubuntu-24.04 | `builder`, `reagents` | 30 | `lint`, `reagents-lint` (toolchain loaded, not rebuilt) | none |
| `helm-lint` | ubuntu-24.04 | — | 10 | inline `helm lint`/`helm template` (mirrors justfile `helm-lint`/`helm-template`) | none |
| `unit-test` | ubuntu-24.04 | `builder`, `reagents` | 30 | `unit-test`, `reagents-test` | none |
| `unit-test-arm64` | ubuntu-24.04-arm | `builder-arm64` | 30 | `unit-test` only — no `reagents-test` on arm64 | none |
| `ui-test` | ubuntu-24.04 | `changes` | 30 | Node 22 + cached npm downloads; one `npm ci`, lint + test + `build:ci` | none |
| `integration` | ubuntu-24.04 | `images` | 45 | matrix: three Docker full-suite shards + agent-auth (`run-integration` composite) | `integration-up` / `integration-up-agent` |
| `integration-extra` | ubuntu-24.04 | `images`, `reagents` | 45–60 | matrix: distributed / owner-memory / infra (not required-to-merge) | matching `integration-up-*` |
| `integration-arm64` | ubuntu-24.04-arm | `images-arm64`, `reagents-arm64` | 45–60 | matrix: three Docker full-suite shards + infra (parallel with amd64) | matching `integration-up*` |
| `ui-e2e` | ubuntu-24.04 | `[ui-test, images]` | 45 | host-installed Playwright process joined only to the server network namespace, reusing the `product-amd64` artifact | inline `docker run --name caesium-server` |
| `ui-e2e-auth` | ubuntu-24.04 | `[ui-test, images]` | 45 | inline `docker run` | inline `docker run --name caesium-server-auth` |
| `helm-integration-test` | ubuntu-24.04 | `[images, helm-lint]` | 60 | kind + `helm install` + `helm test` + full suite in three shards | kind pod via the Helm chart |
| `helm-pod-replacement-test` | ubuntu-24.04 | `[changes, images, helm-lint]` | 45 | `scripts/helm-pod-replacement.sh` — kind + `helm install` at three replicas with retained PVCs, then replaces every pod and proves it rejoins (issue #493) | three kind pods via the Helm chart |
| `podman-integration-test` | ubuntu-24.04 | `images` | 45 | inline `docker run` + full suite in three shards | inline `docker run --name caesium-server-podman` |
| `early-evidence` | ubuntu-24.04 | `changes`, `images`, `helm-lint` | 60 | `integration-test-sql-budget`, `robustness-test`, `check-evidence` (kind + Helm, three persistent replicas) | `integration-up`, then three kind pods via the Helm chart |
| `ci-config` | ubuntu-24.04 | — | 5 | actionlint + wildcard `scripts/test_*.py` discovery | none |
| `build-and-integration-test` / `build-and-integration-test-agent-auth` | ubuntu-24.04 | `changes`, `images`, `integration` | 5 | legacy required-context adapters | none |
| `ci-ok` | ubuntu-24.04 | see §1 | 5 | `scripts/ci-ok.py` | none |
| `publish` | ubuntu-24.04 | see §3 | 30 | none — direct `docker push`/`docker manifest`, and release asset upload (§6) | none |

Path-filter outputs (`changes.go` / `.ui` / `.helm` / `.reagents` / `.ci` /
`.images`) decide which of the jobs above run on a pull request. `master`
and `v*` tags force every output to `true`.

Docker (amd64 and arm64), Podman, and Helm each retain the complete
integration test package. Each engine uses three isolated servers/runners.
`test/shard_test.go` discovers the same Test* methods as testify and assigns
the longest scenarios first to the shard with the lowest estimated cost;
every scenario belongs to exactly one shard, including newly added tests. Existing
engine-specific skips remain unchanged. Top-level Go tests outside the
suite run on every shard. Invalid or partial shard configuration fails;
sharding cannot be combined with `-run` or `-testify.m` filters.

For a local Docker shard:

```sh
CAESIUM_TEST_SHARD_INDEX=1 CAESIUM_TEST_SHARD_COUNT=3 just integration-test
```

Without those variables, local integration recipes run the full suite.
Distributed / owner-memory / agent-auth / infra keep their existing mode
filters and PASS floors. `-count=1` ensures a cached test result never
replaces a live integration run.

After its full local-mode suite, Helm shard 1 rolls the same single-replica
StatefulSet into distributed owner/worker mode and runs only
`TestDistributedKubernetesDeadlines`. The phase preserves the chart's node
identity, waits for the rollout, and provides the runner with the kind
kubeconfig and a host address reachable by task pods for its execution witness.
The regression must observe actual distributed work and covers task/run
expiry plus a positive control. Its log must contain the scenario's PASS and
no skipped subcases; an empty selection or skipped method fails the job.
The other Helm shards and the existing complete-suite coverage keep their
current execution mode. Helm's existing promotion/merge-gate policy is unchanged.

`test/shard_timings.json` contains scheduling estimates in milliseconds:
the maximum duration across the four full-suite engine/architecture lanes
in [run 34236365746](https://github.com/caesium-cloud/caesium/actions/runs/34236365746),
with a 100 ms minimum. Missing or nonpositive hints receive a 1-second
default; obsolete entries cannot add scenarios. Refresh hints from the
top-level `--- PASS/SKIP: TestIntegrationTestSuite/TestName (Ns)` lines of
all twelve successful shard logs, taking the maximum per method and
updating the source run in `test/shard_test.go`. The timing file controls
placement only; reflection controls coverage. Stable method-name and
shard-index tie breaks keep assignments deterministic.

The `product` Bake group also compiles `go test -c -tags=integration ./test/`
once per native architecture, independently of the release/static CLI
compiles. `build/Dockerfile.integration` packages that binary and its shared
libraries in a small Alpine runner with Git. Its Dockerfile-specific ignore
file includes test sources omitted by the product context. All 17 integration
jobs download this runner instead of the full Go builder. Fixtures and the
actual product CLI still come from the checkout and product image.
`scripts/integration-test.sh` runs the binary from the `test/` directory,
preserves mode filters/timeouts and exit status, and always runs the tests
afresh with `-test.count=1`. The same script uses `go test` for normal local
runs. Set `CAESIUM_INTEGRATION_RUNNER_IMAGE` only when deliberately reusing
a runner built from the checkout being tested.

CI uses `build/ci.docker-bake-cache.hcl` and the `bake-images` composite action
to persist product and reagent layers with separate target/architecture
GHA scopes. A Bake target context reuses the cached lean builder graph,
so image producers no longer download the builder image. Go compilation
and npm cache mounts are restored separately with `actions/cache` and
`buildkit-cache-dance`; GHA layer exports alone do not preserve cache mounts.
Compilation cache keys include the architecture, product/reagent group, and
toolchain/dependency hashes. These snapshots refresh when dependencies or
toolchains change; source-only commits reuse them without exporting another
large Go cache. Prefix restores seed a new snapshot from the previous one.
Go's content-addressed build cache validates source/toolchain changes; no
integration test result cache is reused. Module downloads remain in the
builder layers rather than being hidden by an initially empty cache mount.

Reagent builds start directly after path selection: their Dockerfile uses
its own Go toolchain, so it never needs the Caesium builder artifact.
Runtime roles and the reagent toolchain are uploaded separately. Infra and
publish download only the roles; lint and unit tests download only the
toolchain. Arm64 builds only the runtime roles because no arm64 lane
consumes the reagent lint/test toolchain. UI validation runs directly on Node 22
without waiting for a Go builder. Sharding adds runner setup, but integration
compilation is shared; compare both elapsed and total job minutes when tuning it.

The Go filter includes test definitions, testdata, executable examples,
and `.dockerignore`, so fixture-only changes cannot bypass validation.
Pure Markdown documentation changes still avoid image builds.

Per-lane `-run` filters, `-timeout` values, and PASS-floor variables
(`*_integration_min_pass`) are execution-mode wiring, not CI wiring — see
[parallel-execution-operations.md](parallel-execution-operations.md) and the
justfile recipes named above for the current values; they change more often
than this doc should need to.

### Browser outcomes and diagnostics (distributed-testing W1/G1)

The default browser lane selects the `network-recovery` Playwright project.
Its `default` dependency runs the ordinary scenarios first; recovery scenarios
then run in a separate worker and browser, so deliberate offline transitions
cannot disrupt neighboring tests. Both projects appear in the same report.
`just ui-e2e` uses the same project selection. A failure in either phase fails
the job; a failed default dependency prevents the recovery phase from starting.

The CI default lane keeps the existing Node 22, `npm ci`, and
`npx playwright install --with-deps chromium` setup so it uses the same host
browser binary, font packages, cache, and Linux screenshot baseline as the
normal runner. It gets the running Caesium container PID and uses `sudo nsenter
--net` to run that host browser process in only the server's network namespace.
It then creates a private mount namespace of its own and bind-mounts the
container's inspected resolver file onto `/etc/resolv.conf` there. Recursive
private propagation keeps that mount from changing the runner's resolver. The
mounted file's identity is checked without comparing mutable DNS contents.
The command explicitly retains the runner UID, GID, HOME, Node executable, and
working directory, verifies that the installed browser is executable, checks
the server health endpoint, and probes both Google font hosts from the entered
namespace before the suite. External DNS failures emit a warning naming the
host and allow the browser assertions to run. It logs the host and container
resolver nameservers, so the DNS boundary is observable. This preserves the production-like
`http://127.0.0.1:8080` origin and relative `/v1` requests without placing the
browser on the runner bridge while job task containers are created through the
server's Docker socket. It does not enter the server mount, PID, user, root, or
working-directory namespaces, so the browser retains the host checkout and
renderer environment. The private mount namespace is created by the browser
command; it never enters the server's mount namespace.

The isolation addresses an observed runner `net::ERR_NETWORK_CHANGED` asset-load
interruption that left the React root empty in three retained diagnostic attempts;
host-bridge churn is a suspected trigger, rather than an established cause. It
also observed that the first host-renderer namespace attempt could not resolve a
font host. Hosted diagnostics confirmed the runner used a loopback DNS stub
(`127.0.0.53`), while the server resolver (`168.63.129.16`) worked from the
entered network. It does not retry navigation, relax browser assertions, disable flaky-test failures,
or update screenshot baselines. Existing report/result paths remain available to
the host-side sanitizer and artifact upload because the browser still runs as
the runner user in the checkout.

CI retains two Playwright retries for diagnosis and sets `failOnFlakyTests`:
a test that fails initially and passes on retry still fails the lane. Both
`ui-e2e` and `ui-e2e-auth` always attempt collection and upload before server
cleanup, including after setup or test failure. A collection or upload failure
is also visible as a failing step. Missing reports after an early setup failure
are not evidence of a passing browser suite.

Download `ui-e2e-diag-<run_attempt>` and `ui-e2e-auth-diag-<run_attempt>` from
the run. They contain structured `ci-diagnostics/outcomes.json` (candidate SHA,
run/attempt/job and step outcomes/conclusions), server/container logs, and
available Playwright JSON/HTML reports plus failure traces/screenshots/video.
Only sanitized copies from `ui/ci-artifacts/` are uploaded. Redaction covers
`csk_` API-key tokens in text, trace ZIP entries and the HTML report's embedded
ZIP; malformed archives fail collection without falling back to raw copies.
This token-specific redaction is not a general secret detector. For PR runs,
`candidate_sha` identifies GitHub's tested merge commit; verify its parents
against the intended PR head and base before reusing the evidence.

The local workflow checks are:

```sh
actionlint .github/workflows/ci.yml
python3 -m unittest discover -s scripts -p 'test_*.py' -v
```

Wildcard discovery includes new matching validator modules. Existing validators
exercise setup/test failure outcomes, artifact sanitization, malformed archive
refusal and an additional failing module. W1's native fail-once Chromium proof
returned exit 1 after a successful retry. Its refreshed hosted run
[34499470605](https://github.com/caesium-cloud/caesium/actions/runs/34499470605)
passed 28 default and 8 auth tests without skips or flaky outcomes; both
sanitized artifacts were downloaded and checked. This establishes the current
browser evidence path, not later multi-node fault coverage.

### Load-harness reports and failure handling (distributed-testing W1/E1)

`just load-test` runs the Go harness in the builder image against an
already-running server. It creates synthetic jobs and triggers real work, so
use a dedicated test server with metrics available and the selected runtime
configured. Supply `CAESIUM_MANUAL_TRIGGER_API_KEY` through the environment when
required by the server. The recipe uses host networking: localhost requires
Linux or Docker Desktop host-networking support. Relative output paths resolve
inside the mounted checkout; create their parent directory first.

For example, against a test server reachable on port 18087:

```sh
mkdir -p .tmp/load
CAESIUM_LOAD_SERVER=http://127.0.0.1:18087 \
CAESIUM_LOAD_JOBS=3 CAESIUM_LOAD_FAN_OUT=1 CAESIUM_LOAD_DEPTH=1 \
CAESIUM_LOAD_TASK_DURATION=2s CAESIUM_LOAD_CONCURRENCY=1 \
CAESIUM_LOAD_SAMPLE_RATE=200ms CAESIUM_LOAD_TIMEOUT=90s \
CAESIUM_LOAD_OUTPUT=.tmp/load/report.txt \
CAESIUM_LOAD_JSON_OUTPUT=.tmp/load/report.json \
just load-test
```

The human summary goes to stdout by default. Set `CAESIUM_LOAD_JSON_OUTPUT=-`
for JSON on stdout and the human summary on stderr. Output files are opt-in,
with no automatic dated baseline file. JSON `schema_version` is currently **2**; every schema-1 field is still
emitted and still means what it meant. The report records configuration without
credentials, expected/observed/outcome counts, per-run identity/status/times,
samples, row/statement deltas, failure class and workload interval. Schema 2
adds mode/workload identity, the open-loop admission ledger, backlog and
throughput verdicts, observed lifecycle intervals with explicit unavailability
markers, external container resource observations, API-read and SSE-subscriber
mixes, and the workload-driven / timer-driven split of the existing DB
counters — see "Open-loop load driver and lifecycle measurements" below.
`observed` means an acknowledged run ID; an uncertain trigger response does
not prove that the server rejected the write. Reconcile all expected work,
including untriggered work and uncertain admission, before interpreting
throughput.

Exit 0 requires successful expected work and valid measurements. Failed,
cancelled, skipped, unconfirmed, untriggered or timed-out work; missing/reset
required counters; invalid configuration; and output errors return nonzero.
Sampling runs alongside submission/execution, including serial workloads;
scrapes arriving after execution do not establish workload coverage. Defaults
include concurrency 1, sampling every 5 seconds and a 30-minute overall timeout;
choose a sample interval appropriate to short workloads. Engine and image can
be set with `CAESIUM_LOAD_ENGINE` and `CAESIUM_LOAD_IMAGE`.

The final E1 live proof at `91741796` used the example workload and a fresh
product image: exit 0 with 3/3 successful runs and 37 samples across 7.0212
seconds, including early/middle samples in every run. A deliberately invalid
image returned exit 1 with 2/2 failed runs; an unavailable server returned
exit 1 with both expected runs untriggered and none observed. Full local
lint/unit and all executed checks in
[run 34499466191](https://github.com/caesium-cloud/caesium/actions/runs/34499466191)
passed. These are correctness checks for reporting, not calibrated performance
SLOs or proof of multi-node fault tolerance. The arrival-rate driver is E2
(`mode=open`, schema 2) — see "Open-loop load driver and lifecycle
measurements" below. E3/E4 still own comparison and budgets.

### Owner-crash robustness runner (distributed-testing W2/B1)

**Superseded by W3/G3 and W3/G5: this is now a CI lane.** The W2 wording this
subsection used to carry — "not a CI job", run by hand, nothing in `ci-ok`
executes it — no longer holds. `scripts/robustness.sh` is wired into the
workflow, the bake file and the scenario selectors; it runs inside
`early-evidence` on every `go`/`helm`/`ci` pull request, and `ci-ok` fails
closed on the evidence it produces. Prefer `just robustness-test` (or the
umbrella `just early-evidence`) over the raw invocation below — see "Early
evidence lane and the promoted gate" in this section for the lane, its
artifacts and its enforcement. The rest of this subsection remains the
reference for the runner itself and for driving it directly when debugging.

One caveat survives in a narrower form: because `ci-ok` is not one of master's
required status checks (§1), a green *required* set is still not evidence that
owner-crash recovery works. A green `ci-ok` is.

It needs `kind`, `kubectl`, `helm`, `docker` and `python3` on the host and
builds a 4-node kind cluster (1 control plane + 3 workers) with three
persistent Caesium replicas from `helm/caesium/ci/test-values-robustness.yaml`,
so budget several minutes and a few GB. It never touches the caller's kube
context: `KUBECONFIG` is unset and every `kubectl`/`helm` call passes
`--kubeconfig "$ARTIFACTS/kubeconfig"` explicitly. Teardown removes only the
clusters recorded in `$ARTIFACTS/owned-clusters.txt`; set
`CAESIUM_ROBUSTNESS_KEEP_CLUSTER=1` to keep them for debugging.

`just robustness-test` performs the build and invocation below using the
repository's own tags and artifact directory, then redacts and collects the
evidence. Use the raw form when you need to pin a specific image or reuse an
existing artifact directory:

```sh
docker build --build-arg "BUILDER_IMAGE=caesiumcloud/caesium-builder:$CANDIDATE_SHA" \
  --build-arg "CAESIUM_IMAGE=caesiumcloud/caesium:$CANDIDATE_SHA" \
  -f build/Dockerfile.robustness -t "caesiumcloud/caesium-robustness:$CANDIDATE_SHA" .
CAESIUM_ROBUSTNESS_ID="$ROBUSTNESS_ID" CAESIUM_ROBUSTNESS_ARTIFACTS="$ARTIFACTS" \
  CAESIUM_ROBUSTNESS_IMAGE="caesiumcloud/caesium-robustness:$CANDIDATE_SHA" \
  CAESIUM_ROBUSTNESS_SERVER_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
  CAESIUM_ROBUSTNESS_KIND_IMAGE="$KIND_IMAGE" CAESIUM_ROBUSTNESS_TASK_IMAGE="$TASK_IMAGE" \
  bash scripts/robustness.sh
```

All six `CAESIUM_ROBUSTNESS_*` variables are required. `ROBUSTNESS_ID` must be a
lowercase DNS-1123 name of at most 47 characters — it names both the kind
cluster and the namespace, which is what lets two instances coexist.
`CANDIDATE_SHA`, `ROBUSTNESS_ID` and `ARTIFACTS` default from the exported
`CAESIUM_*` values, so `CAESIUM_ROBUSTNESS_SERVER_IMAGE` must be tagged
`caesiumcloud/caesium:<sha>` unless `CANDIDATE_SHA` is exported too.
`build/Dockerfile.robustness` compiles `go test -tags=integration -c
./test/robustness` in the builder image; the root `./test` binary does not
contain that package, so it has to be built separately. The runner pod executes
`/bin/robustness.test -test.v -test.run "$CAESIUM_ROBUSTNESS_RUN" -test.timeout …`.
`CAESIUM_ROBUSTNESS_RUN` defaults to `^TestOwnerCrash$`. Neither the workflow
nor `just robustness-test` sets it, so the `early-evidence` lane is unchanged.
B2's `TestTargetedFaults` is an explicit opt-in — see "Targeted cluster faults"
below. A selection with no declared required subtests is refused rather than
passed vacuously.

Host-side logic lives in `test/robustness/hostlogic.py`, and the script aborts
unless `python3 "$HOSTLOGIC" self-test` passes first. The Go helpers are in
`test/robustness/cluster/` (HTTP, dqlite membership, job fixtures, image
identity, topology), `test/robustness/recorder/` (the effect sink) and
`test/robustness/{dag_order,kill_evidence}.go`; the hermetic parts of those have
ordinary `_test.go` neighbours that `just unit-test` already runs.

What a pass proves: three real dqlite voters agree on a leader; a blocked
two-step fixture is admitted with a run UUID; the mapped owner worker is
cordoned *before* the fixture is triggered and then SIGKILLed via
`systemctl stop kubelet` plus `ctr -n k8s.io tasks kill`, with the container ID
required to appear in `ctr tasks list` and then stop or disappear; the lease
generation increases and a surviving owner completes the same accepted run;
the old member restarts and rejoins with its retained PVC; two isolated harness
instances coexist. `owner_is_leader` and `owner_is_not_leader` are both
required subtests. Raw sink records are copied into `$ARTIFACTS`.

What it does not prove: nothing about partitions, clock skew, storage loss,
quorum-loss rejection bounds, or exactly-once external effects — duplicate task
attempts are *retained as legal*, not treated as failures. The recovery
watchdog is a finite regression limit, not a production SLO. Missing recorder
data is inconclusive, never a pass. `POST /v1/database/query` issues
`PRAGMA query_only`, which native dqlite may reject; if the lease read 500s,
treat it as a product-console gap rather than a reason to skip the observation.

### Integration SQL-work budget (distributed-testing W2/E5)

`TestStatementBudgetFixedWorkload` in `test/statement_budget_test.go` is an
`IntegrationTestSuite` method, so the existing required Docker integration lanes
run it with no workflow change; `test/shard_test.go` reflects the same `Test*`
methods testify discovers and gives an unlisted scenario a default 1000 ms
cost when balancing shards. Run it locally with the usual recipe:

```sh
just integration-up
just integration-test
```

It applies a 2-step sequential `alpine:3.23` HTTP job through the CLI, waits for
the server to go quiet, starts the run with `run start` (stdout captured apart
from stderr so the run UUID parses), waits on HTTP until the run and both tasks
succeed, and compares per-category deltas of `caesium_db_statements_total` and
`caesium_db_writes_total` against `test/statement_budget_testdata/baseline.json`.

Each category in that baseline picks one of three modes. `bound` sets expected
writes/statements plus `min_*` floors and a `write_slack` allowance for leftover
work from earlier suite methods, and keeps the write:statement ratio pinned so
an unbatched INSERT cannot hide behind a matching completion count. `zero`
(`command`, `checkpoint`) must not move at all. `skip` records why a category is
not bounded: `lease_renewal` is timer-driven on a shared server and
`callback` can carry leftover traffic, so both are logged with their deltas
instead of getting a false exact bound. Refresh the baseline by re-measuring on
the lane, not by widening slack until it passes.

`TestStatementBudgetComparator` feeds the recorded profiles in
`test/statement_budget_testdata/cases.json` through the same checker so its
detection is itself tested: lost batching, extra statements with matching
completions, leftover work concealing unbatched inserts, over-budget counts,
missing evidence and a non-zero reserved category all have to fail, while good
and good-with-leftover profiles have to pass.
`TestStatementBudgetParseCounterRejectsPrefixOnlyName` pins the labeled scrape
against `test/statement_budget_testdata/metrics_prefix.prom` so a prefix-only
metric name is not accepted as the counter.

Limits: this budgets the instrumented SQL-work classes for one fixed workload on
the default local executor. It is not a latency check, it does not see
uninstrumented queries, and it does not replace E4's calibrated performance
budgets. Since W3/G3 it **is** registered in the scenario manifest as
`e5-sql-work-budget` with `gates: ["early"]`: `just integration-test-sql-budget`
runs it as a focused scenario that emits evidence with an observed
topology/mode/flag set and a retained `/metrics` counter artifact, while the
full sharded suite still runs it in the `integration` lane.

### Scenario manifest and evidence validator (distributed-testing W2/A2)

`test/contracts/scenarios.json` is the machine-readable catalog of the
real-surface scenarios that are supposed to cover A1's eleven `DT-*` contract
IDs. Each row carries a selector, contract IDs, owning plan item, registering
item, required topology/mode/feature flags, expected observations, allowed skips
with reasons, artifact identity fields and fault-activation requirements.

**Three of the thirteen committed rows are `status: proven` with
`gates: ["early"]`** — `b1-owner-crash-leader`, `b1-owner-crash-nonleader` and
`e5-sql-work-budget`, registered by W3/G3 and actually executed by the
`early-evidence` lane. **Every other row is `status: absent` with empty
`gates`**: the file names those scenarios and certifies none of them. B2's
targeted-fault scenarios are live-proven on owned kind clusters but remain
`absent` in this file — B3 registers them — then D3 and G6. Do not read an
`absent` row as coverage, and do not read a passing local `TestTargetedFaults`
run as an `early-evidence` result.

`scripts/check-test-evidence.py` validates an evidence report against that
manifest. It is fail-closed and stdlib-only:

```sh
python3 scripts/check-test-evidence.py \
  --manifest test/contracts/scenarios.json \
  --report "$ARTIFACTS/evidence.json" \
  --require early --strict
```

`--require <gate>` narrows the check to scenarios listing that gate (for example
`early` or `full`); omit it to require every scenario. Exit 0 means every
required scenario passed or used an allowed skip. Exit 1 is a hard failure:
invalid schema, duplicate IDs, a missing scenario, an unexpected skip, a
disabled gate, wrong topology/mode/feature flags, wrong or missing artifact
identity, absent fault-activation evidence, failed observations, a `pass`
invented for an `absent` row, or `--strict` over inconclusive evidence. Exit 2
means no hard failure but at least one required scenario is inconclusive —
missing recorder data, a checker timeout, insufficient samples. **Inconclusive
is never a pass**; use `--strict` where a gate must not accept it.

`scripts/test_test_evidence.py` covers the checker, and
`scripts/test_collect_evidence.py` covers the collector described in the next
subsection. Neither needs a separate command: the `ci-config` job already runs
the wildcard discovery documented under "Browser outcomes and diagnostics"
above, which picks up every `scripts/test_*.py` module.

### Early evidence lane and the promoted gate (distributed-testing W3/G3, W3/G5)

`early-evidence` is the first CI lane that executes a real multi-node fault, and
since W3/G5 it is a dependency of the fail-closed `ci-ok` aggregate. It runs on
`ubuntu-24.04` when the `go`, `helm` or `ci` path filters select it — exactly
the condition `scripts/ci-ok.py` records in `SELECTORS["early-evidence"]`, so a
lane that disappears cannot read as an allowed skip.

The job runs three steps, all through `just`:

1. **`integration-test-sql-budget`** — E5's `TestStatementBudgetFixedWorkload`
   against the ordinary `integration-up` server, retaining the `/metrics`
   scrape, a `docker inspect` of the live server and the test log as that
   scenario's evidence.
2. **`robustness-test`** — B1's `TestOwnerCrash` on an owned kind cluster
   (1 control plane + 3 workers) with three persistent Helm StatefulSet
   replicas.
3. **`check-evidence`** — merges the two fragments into one report and
   validates it with `check-test-evidence.py --require early --strict`.

`just early-evidence` is the umbrella recipe that runs all three in order.
Locally it needs `kind`, `kubectl`, `helm`, `docker` and `python3`, several GB
and several minutes, and it occupies the shared Docker integration container, so
hold the host lane lock first:

```sh
just tag=<candidate-sha> early-evidence
```

Artifacts land under `.tmp/evidence/` (override with `CAESIUM_EVIDENCE_DIR`):
`sql-budget/` and `robustness/` hold the raw lane output, `sql-budget.json` and
`robustness.json` are the per-lane fragments, and `evidence.json` is the merged
report. CI uploads the whole directory as the `early-evidence` artifact with
`if-no-files-found: error` and 7-day retention. Before anything can upload it,
`just robustness-test` runs `collect-evidence.py redact` over the artifact
directory: the run's generated `CAESIUM_INTERNAL_WAKEUP_TOKEN` is replaced
everywhere it appears and `internal-token.txt` / `kubeconfig*` are deleted. That
is a targeted scrub of this lane's own generated credentials, not a general
secret detector.

**`scripts/collect-evidence.py`** is the artifact consumer — stdlib-only, like
the checker it feeds. Four subcommands: `robustness` and `sql-work-budget` build
the per-lane fragments, `report` merges fragments into one evidence report for
`check-test-evidence.py --report`, and `redact` performs the scrub above. It
never invents an observation: every field it emits is read back out of a file
the lane actually produced — pod environments, the kind config, bound PVC claim
names, the recorder records, `docker inspect`, the retained `/metrics` scrape
and the Go test logs. A missing record, an empty pod-env dump or an unreadable
`ctr` listing yields `inconclusive`, never `pass`, and env values whose names
look like credentials are dropped before they can reach the report.

**What the promoted gate enforces.** A green job result is not accepted as
evidence. `ci-ok` downloads the report only when
`needs.early-evidence.result == 'success'`, so a lane that ran and left no
report still fails, and then `scripts/ci-ok.py` requires, fail-closed:

- the report exists, parses, and is an object — an absent file is "the lane
  produced no evidence", not a pass;
- `report.candidate_sha` equals this run's `github.sha`, so evidence from
  another commit cannot satisfy this run;
- the `early` gate has at least one registered manifest scenario, so an ungated
  manifest cannot vacuously pass;
- every `early`-gated manifest row is present with `status: "pass"` and, where
  the manifest requires fault activation, `activated: true` with the right kind
  and every required observation;
- `check-test-evidence.py --require early --strict` exits 0 over the same
  report, which carries the deeper topology/mode/feature-flag/artifact-identity
  validation described in the previous subsection.

**Measured cost.** Five green hosted runs took 6m19s, 5m57s, 6m18s, 7m15s and
7m32s (mean ~6m40s), with no lane flake and no lane retry. The promotion moved
`ci-ok`'s verdict about 2m07s later; total workflow wall clock was unchanged on
one attempt (13m12s, with `helm-integration-test` still the critical path) and
+25s on another, where `ci-ok` became the critical path. It costs no new runner
minutes — the lane already ran on every `go`/`helm`/`ci` PR before the
promotion; the change is only that `ci-ok` now waits for it.

**Observed fail-closed behaviour.** On
[run 34868499191](https://github.com/caesium-cloud/caesium/actions/runs/34868499191)
attempt 1, a transient `images`/`bake-images` failure on a docs-only commit
skipped the lane although the path filters had selected it, and the gate refused
for both reasons at once:

```
ci-ok failed: images=failure, ui-e2e=skipped, ui-e2e-auth=skipped, integration=skipped,
              early-evidence=skipped,
              early-evidence evidence: .tmp/evidence/evidence.json is absent; the lane produced no evidence
```

Attempt 2, with `images` green on identical code, passed. The same behaviour is
reproducible locally against a real artifact directory: delete
`.tmp/evidence/sql-budget.json` and `just check-evidence` exits 1 with
`missing evidence fragment …; the lane did not produce it`; drop a scenario from
a fragment and it exits 1 with `missing-scenario=<id>`.

**What this gate does and does not prove.** It proves one owner-crash fault on a
three-member kind/Helm cluster with persistent volumes, plus one SQL-work budget
on a single-process Docker server, both bound to the tested candidate. It is not
partition, clock-skew, storage-loss, upgrade or performance equivalence, and no
timing calibration was waited on. And until `ci-ok` is added to master's
required status checks (§1), it blocks `v*` publication rather than PR merge.

### Reference models and generated property tests (distributed-testing W3/C1)

`test/model/` is an independent, untagged, pure-Go reference model of the run
lifecycle. It re-derives readiness declaratively as a fixpoint over the current
outcome set, deliberately unlike `internal/run`'s incremental predecessor
counters and in-place ready queue, so agreement between the two is evidence
rather than a mirror. `internal/run/model_properties_test.go` drives the real
`RunState` (`NewRunState`, `MarkDispatched`, `ApplyCompletion`, `ReadyTasks`,
`Clone`, `RequeueExpiredRows`, `AnyLeaseOverdue`, `ApplyExpansion`) against it
over generated bounded DAGs, and `internal/run/recovery_properties_test.go`
drives the real `RecoverRunState` / `RecoverRunStateWithFanOut` / `Snapshot` /
`Restore` / `ValidateCheckpointBlob` over every checkpoint index of a generated
execution.

There is no separate command — these are ordinary unit tests:

```sh
just unit-test
```

`test/model/independence_test.go` keeps that honest mechanically: it enforces
the package's import allowlist and its no-build-tag rule, so an "independent"
model that starts a cluster or client, opens a Docker socket, touches the
network, or imports a product decision function is a test failure rather than a
broken convention. Porcupine is used for exactly one thing — the run-status
register, which does have a valid sequential specification. At-least-once event
delivery, liveness and lease safety have separate models (`events.go`,
`lease.go`, `oracle.go`), each with a negative control proving the oracle
rejects a planted defect.

To stress a property harder than the default, raise Rapid's own `-rapid.checks`
(and optionally `-rapid.steps`) inside the builder image the unit lane uses. The
C1 evidence used 4000–5000 checks at 60–80 steps on every new property:

```sh
docker run --rm -v "$PWD":/build -w /build \
  caesiumcloud/caesium-builder:latest-full \
  go test ./test/model/ -rapid.checks=5000 -rapid.steps=80
```

Retained counterexamples are committed as **deterministic source** in
`test/model/regression_test.go`, not as Rapid `.fail` artifacts. That is a
deliberate deviation the plan records: a `.fail` file is an opaque bitstream
tied to the exact sequence of draws a property made, so the first generator
refactor turns it into a "fail file is no longer valid" log line that runs
nothing and explains nothing. `test/model/doc.go` documents the workflow —
commit the `.fail` file while a defect is **open**, so CI reproduces the exact
case, then transcribe it into `regression_test.go` as a named test and delete
the artifact when the fix lands.

Limits: a green run proves a *decision function* agrees with a specification on
hermetic inputs. It proves nothing about wiring — not that the HTTP handler
calls it, not that the transaction commits, not that a real owner on a real
cluster reaches the same state. The `early-evidence` lane and the integration
suites are what establish that. The four counterexamples C1 found were all in
the new model; the product's `RunState` and recovery paths agreed with it under
4000+ generated cases per property.

### Developer-journey CLI scenarios (distributed-testing W3/D1)

`test/developer_journey_test.go` adds ten `IntegrationTestSuite` scenarios that
extend the binary-driven local-dev journey in `test/local_dev_test.go` (which is
unchanged). They need no workflow change — the existing Docker integration lanes
discover them like any other suite method, and `test/shard_test.go` places them
by the same reflection:

```sh
just integration-up
just integration-test
```

They drive the container-built release CLI in an empty temporary workspace and
capture stdout apart from stderr throughout (`runCLISeparate` / `runCLIStdout`,
never the stream-merging `runCLIRaw`). Coverage:

- Unparseable YAML (a tab violating block indentation) rejected by **both**
  collection paths — `internal/jobdef.CollectDefinitions` for `test` and
  `dev --once`, `cmd/job.collectDefinitions` for `lint`, `preview` and `apply`
  — plus a schema-invalid definition rejected by `dev --once`, the command that
  actually executes the DAG.
- A workspace directory *and* job filename that both contain a literal space.
- A step declaring `engine: kubernetes` with no reachable kubeconfig or
  in-cluster config.
- `--run-timeout` cancelling a run: the container has to be stopped and removed,
  not merely reported as a nonzero exit.
- Watch mode: first run, edit-triggered re-run, SIGINT, a graceful "Stopping.",
  and no surviving container from either run.
- `job apply` against the live server followed by `job export` — the CLI's own
  read-back verb — asserting the round-tripped DAG topology and labels.

Two real product defects were found while writing these and **filed rather than
worked around**, because the owning packages are outside that stream's file
scope. [#479](https://github.com/caesium-cloud/caesium/issues/479): `caesium dev
--once` panics inside `internal/atom/kubernetes.NewEngine` (exit 2) on an
unreachable Kubernetes engine instead of returning a clean error, because
`internal/job.buildLocalRunners` calls the engine factory with no `recover()`.
[#480](https://github.com/caesium-cloud/caesium/issues/480): `caesium dev
--once` ignores SIGINT and orphans its container. The affected assertions
require only a bounded nonzero exit, so they stay valid once those are fixed.

### Browser accessibility, visual, scale, and recovery coverage (distributed-testing W3/D2)

Four new spec files run in the existing required `ui-e2e` project against the
live backend, under G1's retry and diagnostic rules. No new job, no new command:

```sh
just ui-e2e
just ui-e2e-auth
```

**`accessibility.spec.ts`** runs `@axe-core/playwright` over the jobs list, run
detail and its task panel, the Trigger Job dialog and the triggers page, plus
keyboard and focus checks. It asserts only on WCAG 2.0/2.1 A+AA rules at
`critical`/`serious` impact; `moderate`/`minor` findings are attached to the
report but not asserted, so a large, actively-developed page does not flap on
cosmetic nuances while missing labels and keyboard traps still fail hard. The
two canvas-rendered widgets that expose no DOM accessibility tree by
construction — the react-flow DAG canvas and the xterm log terminal — are
excluded from the scan itself; the plaintext log mirror and the DAG's own
button/label chrome stay in scope.

`KNOWN_VIOLATIONS` is a **tracked baseline of real, pre-existing product
defects** the scan found (systemic icon-only buttons with no accessible name;
several muted-text/badge tokens below the 4.5:1 contrast ratio), filed as
[#483](https://github.com/caesium-cloud/caesium/issues/483). It is not a pass.
It is tracked **per violating node, not per whole rule id**, so a newly-broken
control under an already-known rule still fails. Node identity is matched
structurally — the leaf element's class set, subset- and order-tolerant,
ignoring ancestor position — because axe computes the shortest selector that is
unique given whatever fixture data other concurrently-running specs happen to
have created, so the same button can be reported under different selectors run
to run. `color-contrast` is instead matched globally by the violation's own
`fgColor` within a small RGB tolerance, since these opacity-composited tokens
report slightly different colors run to run against an unmodified UI; a
genuinely new foreground token is therefore still reported as new. Shrinking
this baseline is product-code work; widening it without a product-side
justification is not.

**`visual.spec.ts`** takes deterministic screenshots clipped to a single
component (the Trigger Job dialog, a fixed-shape branching DAG, a fanned task's
partition table), with a fixed viewport, `animations: "disabled"`, Google Fonts
requests fulfilled empty so the fallback stack is stable, and elapsed-time text
masked by locator rather than compared pixel-for-pixel.

> **Linux-baseline rule.** Playwright names snapshots per platform, and CI only
> ever compares `-linux.png`. Only `-linux.png` baselines are committed, and
> they must be generated inside a Linux container running the exact
> `@playwright/test` version pinned in `ui/package-lock.json` (for example
> `mcr.microsoft.com/playwright:v<version>-noble`). A baseline taken on a
> developer's macOS or Windows machine names a *different* file that CI never
> reads, so the linux baseline would silently stay missing — which fails the
> first CI run rather than passing it. Each test self-skips on any other
> `process.platform`, because `just ui-e2e` runs the browser on whatever host
> invoked `just`, not in a container: a local darwin run correctly reports these
> as skipped, and the real per-pixel comparison is CI's `ui-e2e` check. When a
> baseline legitimately changes, re-take it from the same pinned container (or
> from CI's actual render) — never from a developer host.

**`scale.spec.ts`** proves a real 18-node DAG renders every node, that 24 live
pipelines each stay reachable through the pipeline filter, that the fanned
partition table's virtualization keeps the DOM bounded while every row remains
reachable by scrolling, and that a real 2000-line log stream is fully reachable
through the log viewer's search filter. The large 240-row partition set is
explicitly SYNTHETIC: the e2e server runs with
`CAESIUM_FANOUT_MAX_PARTITIONS=8`, so a real group that size cannot be produced
live — only the partitions list response for one real task is replaced.

**`network-recovery.spec.ts`** covers reload-preserves-state, a real
browser-level network cut via `context.setOffline`, and three explicitly
labelled SYNTHETIC cases (expired credential, denied mutation, and a delayed
stale job-detail response that must not clobber a faster subsequent
navigation). This e2e server runs without `CAESIUM_AUTH_MODE`, so there is no
live credential surface here; real scope-based denial stays covered against an
auth-enabled server in `ui/e2e/auth/`.

`ui/e2e/helpers/fixtures.ts` provides the shared `failOnUnexpectedPageErrors`
guard used by all four files. It fails a test on unexpected console or page
errors, allowing the browser's own automatic "Failed to load resource: status N"
logging so the deliberate synthetic error-response cases do not self-trip it.
The network-level `net::ERR_*` allowance is **file-scoped and opted into only by
`network-recovery.spec.ts`** (`failOnUnexpectedPageErrors({
allowNetworkLevelErrors: true })`), because the real offline cut was observed
producing `net::ERR_NETWORK_CHANGED` noise against later, unrelated tests in
that same file. Every other spec keeps the strict default.

### Candidate identity and merge-group wiring (distributed-testing W4/G7)

G7 did not add a job, a `ci-ok` dependency, or a repository setting. It made
the existing aggregate describe the commit it actually tested, and it wired
`merge_group` so a merge queue is activatable by a settings change alone.

**Settings evidence, unchanged by this item.** Read at execution time on
2026-09-16 and reconfirmed 2026-09-17:

- required contexts: `lint`, `unit-test`, `unit-test-arm64`, `ui-test`,
  `ui-e2e`, `ui-e2e-auth`, `build-and-integration-test`,
  `build-and-integration-test-agent-auth`
- `strict: false`, `enforce_admins: false`, one CODEOWNER review
- `gh api repos/caesium-cloud/caesium/rulesets` → `[]`
- `allow_auto_merge: false`, `allow_update_branch: false`

`ci-ok` remains absent from required contexts (G5's Q6 finding). No PATCH in
§1 was executed.

**What the workflow now does.** Every `actions/checkout@v6` step omits `ref:`,
so each evidence-producing job tests the commit GitHub hands it — on
`pull_request` that is the prospective merge commit (`refs/pull/N/merge`). The
`ci-ok` job passes this run's `github.event_name`, the base/head SHAs from
`github.event.pull_request.*` or `github.event.merge_group.*`, a
`--current-base-sha` from `git ls-remote origin refs/heads/master`
(`continue-on-error: true`, so a transient failure lands as an empty string
and `ci-ok.py` decides what that means), and on `pull_request` the candidate's
real git parents from `git cat-file -p "${{ github.sha }}"` (parent SHA lines
survive a fetch-depth 1 checkout). The two legacy
`build-and-integration-test*` wrappers still omit `--candidate-sha` and stay
exempt; an *explicit* empty `--candidate-sha ''` is the opposite case and
refuses.

`changes` diffs `merge_group` against `github.event.merge_group.base_sha`
(`fetch-depth: 0`; the `'0'`/`'1'` operands are quoted strings because a bare
number `0` is falsy in Actions expressions). A missing or non-boolean filter
output fails the job instead of defaulting to "everything skipped". `push`
still forces every selector true, now explicitly excluding `merge_group`.

**What `scripts/ci-ok.py` refuses.** Supplying any of `--candidate-sha` /
`--event-name` / `--base-sha` / `--head-sha` turns identity on (`is not None`,
not truthiness):

- missing event name, missing candidate SHA, or an unrecognized event
- `pull_request` / `merge_group` missing base or head
- `merge_group` candidate SHA ≠ head SHA (GitHub's contract is that they are
  the same)
- `pull_request` candidate with other than exactly two parents, or a second
  parent that disagrees with the payload head. A first-parent / payload-base
  disagreement is **reported, not refused**, because GitHub can regenerate
  the merge ref after the payload was recorded; freshness then compares the
  first parent
- on `merge_group` only: tested base ≠ current master tip, or either side
  missing. The same comparison on `pull_request` is informational (`strict:
  false`, no queue)

The early-evidence report checks from G5 are unchanged.

**Live vs static proof.** The `pull_request` path is live:
[run 35221628892](https://github.com/caesium-cloud/caesium/actions/runs/35221628892)
logged `ci-ok candidate identity: event='pull_request'` and `base freshness:
tested base … matches current master tip`. `merge_group` has never fired; every
queue-specific path is proven only by `actionlint .github/workflows/ci.yml`
and `python3 -m unittest scripts/test_ci.py`. There is no substitute for a
live queued run once a ruleset exists.

**Q1 cost (informational).** Last 10 green `pull_request` runs as of
2026-09-17: wall-clock 0.60–35.57 min, mean 15.9 min. A representative required
critical path (`changes` start to `ci-ok`) is ~13 min; the full matrix
including optional Helm shards is ~17.3 min. `strict: true` costs one extra
~13-minute required-set rerun each time master advances during review.
A merge queue costs one extra ~13-minute run per merged PR (or per batch).
G7's recommendation to the CODEOWNER, not applied: add `ci-ok` to required
contexts first, then either `strict: true` or a `merge_queue` ruleset
targeting `master`. `allow_update_branch: true` is optional and only
relevant while protection stays non-strict.

### Targeted cluster faults (distributed-testing W4/B2)

B2 extends B1's owned kind/Helm harness. The `early-evidence` lane is
**unchanged**: it still runs `TestOwnerCrash` on the ordinary release image.
Targeted faults are an explicit opt-in on the same host controller.

```sh
# Pause / partition / response-loss / event-history (release image):
CAESIUM_ROBUSTNESS_RUN='^TestTargetedFaults$' just tag="$CANDIDATE_SHA" robustness-test

# Durable-event-before-publication hook (instrumented image required):
# build the server image with the testfault tag, then:
CAESIUM_ROBUSTNESS_RUN='^TestTargetedFaults$' \
CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE="caesiumcloud/caesium:${CANDIDATE_SHA}-testfault" \
  just tag="$CANDIDATE_SHA" robustness-test
```

`scripts/robustness.sh` refuses a `CAESIUM_ROBUSTNESS_RUN` whose required
subtests are undeclared, and it scans the deployed **release** image for
`testfault` markers (`caesium-testfault-control`, `CAESIUM_TESTFAULT_DIR`,
`bus-publish-pause.json`, `internal/testfault`, `testfault`) — a leak fails
the run. When an instrumented image is supplied it must *contain* those
markers, so a build tag that failed to apply cannot make the hook assertions
vacuous. The control surface is a file on the pod's own emptyDir written
through the host controller's container runtime: no listener, port, route,
token or new RBAC. `internal/testfault` is a test-only twin with
`const Enabled = false` in release builds.

What a pass proves, each with independent activation/heal evidence:

- `external_pause_resume` — `ctr tasks pause` of a non-leader after kubelet
  is stopped (so a liveness probe cannot turn resume into replace); runtime
  `PAUSED` plus `/health` ceasing are two observations; held past the 30s
  run lease, then resumed
- `asymmetric_partition` — EXTERNAL iptables inside the owned kind nodes,
  keyed by addresses the dqlite `Cluster` RPC actually returned. A1 forbade
  proxies without a routing spike; none was adopted. The blocked dispatch
  rule's own packet counter must be positive or the route is UNPROVEN
- `response_loss_possibly_committed` — a client-side interposer on the
  runner after an observed 202+UUID; dropped-response and
  delayed-past-deadline variants mark the operation possibly committed and
  reconcile the run by identity on a different member. A client timeout is
  never a rejection
- `event_history_correlation` — SSE vs persisted rows as a **set** inside
  the read scope (DT-EVENT-01); duplicates and out-of-order arrivals are
  legal; a gap-free or high-water-mark check is invalid. A broken or empty
  subscription is inconclusive, not legal at-least-once loss. Catch-up uses
  the run's lowest persisted sequence as `Last-Event-ID`
- `bus_publish_pause` — the A1-justified hook after the event row is
  durably committed and immediately before its first `bus.Publish`, on
  **both** `PublishAndMarkBusDispatched` and `DispatchOnce`. Releases are
  reconciled against the holds activation recorded (`faults.ReconcileReleases`);
  a vanished log is not "never entered"

Limits: this is not a CI lane and not in `ci-ok`. Missing recorder data,
missing hook-release evidence, or a destination API that degrades because
the cut-off member lost Raft leadership, are classified rather than
skipped. Duplicate task attempts remain legal. Clock skew, storage loss and
quorum-loss rejection bounds are still unresolved. B3 registers these
scenarios in `test/contracts/scenarios.json`.

### Open-loop load driver and lifecycle measurements (distributed-testing W4/E2)

E2 extends E1's containerized Go driver in place — no k6, no new toolchain,
no `justfile` edit. Closed-loop `just load-test` still works as documented
above. Open-loop traffic and the versioned catalog live in
`test/performance/`, which the precompiled `./test` runner does **not**
contain, so they are compiled and run explicitly against an already-running
server:

```sh
just builder-full     # produces caesium-builder:latest-full; integration-up does not
just integration-up   # starts caesium-server-test via builder:latest / the -test image
CAESIUM_PERF_WORKLOADS="${CAESIUM_PERF_WORKLOADS:-smoke}" docker run --rm \
  -v "$PWD":/bld/caesium -w /bld/caesium \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e DOCKER_HOST=unix:///var/run/docker.sock \
  -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
  -e CAESIUM_PERF_SERVER_CONTAINER=caesium-server-test \
  -e CAESIUM_PERF_WORKLOADS \
  --network=container:caesium-server-test \
  caesiumcloud/caesium-builder:latest-full \
  sh -c 'go test -tags=integration -count=1 -timeout=30m -v ./test/performance'
```

`CAESIUM_PERF_WORKLOADS` must be forwarded with `-e`; a host export alone is
silently ignored and `selected()` then defaults to the four `smoke` catalog
entries. The command above makes that default explicit. For the full
ten-workload catalog:

```sh
CAESIUM_PERF_WORKLOADS=all docker run --rm \
  # …same flags as above, including -e CAESIUM_PERF_WORKLOADS…
```

A comma list of workload names or tiers is also accepted.

`mode=open` places arrivals on an absolute clock grid computed from
`-rate`/`-arrival-window` before the first request, so the schedule never
waits on a completion. Outcomes are deliberately not conflated:

| Bucket | Meaning |
| --- | --- |
| `dropped` | driver's own in-flight cap / scheduler lag / deadline-before-offer |
| `admitted` | DT-ADMIT-01: 202 **with** a run body carrying a UUID |
| `queued_or_skipped` | a bare 202 — its own outcome, not an admission |
| `rejected` | an application refusal |
| `transport_uncertain` | DT-QUORUM-01: possibly committed, never a rejection |

Every admitted run is polled to a terminal status. Arrivals carrying no run
identity are reconciled against `GET /v1/jobs/:id/runs`; census-discovered
runs are driven to terminal too. The reporter fails `accounting_mismatch`,
`unreconciled_admission`, `census_unavailable`, `queue_unobserved` /
`queue_not_drained` (when bare-202 arrivals exist), and — for
`require-sustained` workloads — `backlog_growth` / `backlog_inconclusive`.
Lifecycle intervals come from `/v1/events` and from the public run read;
anything unobservable emits `"status":"unavailable"` with a counted reason
— never 0, never an absent key.

Hermetic scheduling/report tests stay in `test/load/harness_test.go` and run
inside `just unit-test`. The live catalog at E2's merge: 10/10 workloads plus
two extra tests, `ok` in 350.766s against the `integration-up` server;
`open-tiny-sustained` offered 20 / admitted 20 / completed_ok 20; the
overload case finished in 13.1s with driver drops plus bare-202 skips rather
than hanging. Two catalog rows (`open-tiny-sustained`, `open-api-read-mix`)
gate on the backlog verdict; four others report `backlog_growing` on a
shared laptop-class host and pass only because they omit `require-sustained`
(each carries a mandatory `sustained_rationale`, enforced by
`TestWorkloadCatalogIsValid`).

Limits: this is not a CI job, not a calibrated SLO, and not a substitute for
E3's base/candidate comparison. Production per-task resource telemetry stays
with resource right-sizing; E2's external observation reads the server
container's stats from outside via the runtime API.

### Native fuzzing and synctest concurrency (distributed-testing W4/C2)

Eight fuzz targets across four packages, each asserting a property beyond
"didn't panic" (schema-compat reflexivity, differential agreement between
independent decode paths, descriptor round-trip, secret-ref merge
idempotence, checkpoint/recovery agreement). There is no justfile recipe
(G6 owns later lane recipes). The script re-execs itself inside the same
`caesium-builder:latest-full` image `just unit-test` uses:

```sh
scripts/fuzz-tests.sh
# longer campaigns (G4):
CAESIUM_FUZZ_SECONDS=120s scripts/fuzz-tests.sh
```

It is POSIX `sh` on purpose — that image has no bash. It discovers targets
via `go test -list '^Fuzz'` per declared package **and** a repo-wide sweep
for any top-level `func FuzzX(` in a test file, fails on a declared /
discovered mismatch, runs one target per `go test -run=^Name$ -fuzz=^Name$
-fuzztime=<budget>` invocation, and rejects a seed-only or zero-exec run
(`execs` must exceed Go's own completed-baseline denominator). Corpus
artifacts land under `.fuzz-artifacts/` (override with
`CAESIUM_FUZZ_ARTIFACT_DIR`). A selected `internal/worker` renewal set is
then repeated under `-race -count=3` at `-cpu=1,2,4` (one `go test` per cpu
value); each configuration must report `passed > 0` and `failed == 0`.

`internal/worker/renewal_synctest_test.go` drives the real
`runLeaseRenewal` / `runRunLeaseRenewal` goroutines through
`testing/synctest` against the package's existing fakes — no production
clock seam (A1 deferred that). Renewal fires exactly once per configured
tick in fake time; cancellation stops the goroutine with no leaked ticker
reader. Real dqlite `RenewLeases` SQL, wall-clock jitter and the HTTP
dispatch path stay out of that claim; B-stream's live cluster remains the
source for those.

Measured at C2's merge with `CAESIUM_FUZZ_SECONDS=20s`: 8/8 targets
explored (61k–358k execs, zero crashers) and 9/9 concurrency configurations
passed. `just unit-test` already includes the non-fuzz `_test.go` neighbours.
[#549](https://github.com/caesium-cloud/caesium/issues/549) is a minor
product defect found while writing the secret-ref fuzzer and filed rather
than fixed (out of file scope).

### Single-node previous-release upgrade qualification (distributed-testing W4/F4)

F4 qualifies the one supported adjacent pair `v0.1.0 → candidate` on **one
node**, independently of B3 and of any cluster. It is not a CI job — G6
wires it. The precompiled `./test` runner does not contain
`test/lifecycle`, so the host controller compiles that package explicitly
with `-tags=integration` inside the builder.

```sh
CANDIDATE_SHA=$(git rev-parse HEAD)
CAESIUM_LIFECYCLE_ID="lifecycle-$(uuidgen | tr '[:upper:]' '[:lower:]' | tr -d - | cut -c1-12)" \
CAESIUM_LIFECYCLE_ARTIFACTS="$(mktemp -d)" \
CAESIUM_LIFECYCLE_PREV_IMAGE="caesiumcloud/caesium:v0.1.0" \
CAESIUM_LIFECYCLE_CANDIDATE_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
  bash scripts/lifecycle-tests.sh
```

Leave the candidate image **unbuilt** if you want provenance: the harness
builds it from this checkout (`just tag=$CANDIDATE_SHA build-release`) when
that image is absent, which is what binds `candidate_sha` to the image
actually qualified. A pre-existing or dirty-tree image is
`supplied/unverified` and **blocks** the qualification unless
`CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1` is also set and recorded.
`:latest` is refused outright. `caesiumcloud/caesium:v0.1.0` is pulled and
its repo digest is verified against the pinned index digest in
`test/lifecycle/versions.json`
(`sha256:2e6996f965ab7899ac3f2d80a7607e26a96ff607d24baf8566033d6d7aa73917`)
before anything runs.

Volume and identity rules: a **fresh named volume** only (it inherits
`10001:10001` from the release image); a host bind mount is forbidden. No
fixed host port. Two instances were proven to coexist on separate networks
and volumes. Teardown removes only names carrying this invocation's
`CAESIUM_LIFECYCLE_ID`. Set `CAESIUM_LIFECYCLE_KEEP=1` to keep them.

The qualification record
(`$CAESIUM_LIFECYCLE_ARTIFACTS/qualification.json`) says `pass` only when
every expected case recorded a pass or a recorded-outcome under **this**
invocation's id **and** every phase returned 0. The run writes a
`result: incomplete` placeholder first, so an aborted run can never leave a
stale passing file behind. A case with no record is synthesized as
`blocked`. Recorded-outcome cases are blocked when the observation itself
failed (`status: unknown`, a sentinel exit, an uncaptured log).

What a pass asserts on the candidate, after seeding through v0.1.0's own
public surface and `docker stop -t 60`:

- schema is a superset of the v0.1.0 snapshot plus the pinned table/column
  additions in `versions.json` (additions beyond the pin are recorded, not
  failed — F4 is not in CI until G6)
- every recorded job/run/task-run UUID is readable with unchanged terminal
  status; events replay as a **set** above an explicit resume cursor
  (DT-EVENT-01; no gap-free or high-water-mark check)
- the queued row is dequeued into a started run whose `started_at` is
  after the previous release finished — asserting the row still exists is
  not enough
- candidate `job export` of each alias re-lints and `job diff`s clean
  (v0.1.0 has no `job export`; that assertion is candidate-side only)
- the serving build is the candidate (container image ID, plus
  `data_assertions_enabled` in `GET /v1/system/features`)

**Unsupported-transition re-grounding.** F1 required "restart the candidate
at a different `CAESIUM_NODE_ADDRESS`, expect exit 1". PR #536 (`35bced63`,
closes #493) later made the candidate reconcile `info.yaml` and recover a
genuine sole member's raft configuration, so that case now **starts** on
the candidate. F4 asserts #536's contract there and keeps the **pinned
v0.1.0** image at a changed address, on a copy of the volume, as the
deterministic failing transition (nonzero exit, `address … in info.yaml does not
match`). Upgrade-then-readdress is the only supported order for this pair.

**Recorded-outcome cases**, each on its own volume copy, with no pre-judged
expectation: rollback (v0.1.0 on the migrated volume — a start is not a
supported rollback guarantee); shard-count change (a silent success that
strands no rows at this size is a finding, not a pass, and does not
unfreeze the shard count); retained in-flight run (still `running` on the
candidate and permanently holding a concurrency slot — filed as
[#553](https://github.com/caesium-cloud/caesium/issues/553)).

Limits: one node, one shard, local execution mode. This pair is purely
additive, so it does **not** exercise `MigrateTaskRunUniquePartitionIndex`.
Nothing here qualifies a cluster upgrade, a mixed-version window,
restore-from-snapshot, or rollback as a product guarantee. The published
amd64/arm64 CLI checksums are carried in `versions.json` but
`scripts/ci-cli-smoke.sh` is not re-run by this lane.

### Persistent-cluster lifecycle qualification (distributed-testing W6/F2)

F2 extends the F4 controller with `CAESIUM_LIFECYCLE_MODE=cluster`. Run it only
in the exclusive Docker/kind/Helm lane from a clean, committed candidate;
leave the candidate image tag absent so this invocation builds and verifies
it. The controller creates its own four-node kind cluster, three persistent
StatefulSet members and artifact kubeconfig, and refuses a pre-existing cluster
with the same id. It checks the pinned `v0.1.0` digest and every candidate
archive/config/layer hash and node import before testing the image-only Helm
upgrade. The cluster result is `$ARTIFACTS/cluster-qualification.json`; retain the printed local artifact directory after the command.

```sh
CANDIDATE_SHA=$(git rev-parse HEAD)
ARTIFACTS=$(mktemp -d)
printf 'Lifecycle artifacts: %s\n' "$ARTIFACTS"
CAESIUM_LIFECYCLE_MODE=cluster \
CAESIUM_LIFECYCLE_ID="lifecycle-$(uuidgen | tr '[:upper:]' '[:lower:]' | tr -d - | cut -c1-12)" \
CAESIUM_LIFECYCLE_ARTIFACTS="$ARTIFACTS" \
CAESIUM_LIFECYCLE_CANDIDATE_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
  bash scripts/lifecycle-tests.sh
```

The required cases cover three PVC-backed voters, a protocol-2 mixed-version
window, in-flight and queued work with durable task-run IDs and raw effects,
rolling upgrade, snapshot catch-up, storage-copy restore with an omitted-copy
control, fresh-PVC ordinal-1 replacement, ordinal-0 disk loss and an isolated
rollback observation. A skipped or unobservable case is `blocked`; the overall
record cannot pass on a subset. The latest live probe (`289bb343`, local,
ephemeral artifacts `/tmp/caesium-w6-f2-observe.9DHTV9`) exited 1: five cases passed
(pinned image, three voters, mixed-version dispatch/completion, rolling upgrade
and retained history/effects), and five were blocked. Snapshot catch-up's
batch 13 write 82 returned EOF after 1,400 initial writes and 6,081 acknowledged
updates; the attempted annotation was `006082`. `caesium-0` was OOMKilled
(exit 137) at its 1Gi cap at `2026-09-26T02:24:20Z`. Bounded disputed-write
readback remains unknown: one survivor refused the connection and the other
hit its deadline. The disputed write remains possibly committed. Replacement,
restore, ordinal-0 and rollback were blocked because the fault left the shared
cluster unfit for subsequent destructive cases. A prior head passed restore
and ordinal-1 replacement; that is historical partial evidence. F2 remains
unchecked and is not a CI job or a complete cluster-upgrade qualification.
Merged [failure-evidence repair #569](https://github.com/caesium-cloud/caesium/pull/569),
merge `62c8b2bd`, tested head `05ae4644`, removes the bundled runtime copy and has the same source tree
as diagnostic-only `b45adfdc`. Its unchanged diagnostic code passed 10 focused
Python tests, all 331 scripts tests, eight guard-removal controls and containerized
integration/race readback regressions (1.070 s). Its current-head CI passed all
37 executed jobs. It merged before the planned #571 dependency; this does
not claim that ordering or production readiness was satisfied. The earlier `b8498ac8` green
run included the removed runtime copy and is historical.
[#571](https://github.com/caesium-cloud/caesium/pull/571) merged at
`ec893213f0532513896aba9630d2a737e1431c47`. Candidate `82ca5905` bounds setup
to 10 s and handles empty nonterminal streams plus earlier kubelet readiness
responses. The `82ca5905` UI/`ci-ok` failure is historical. [#572](https://github.com/caesium-cloud/caesium/pull/572)
merged at `073402a1` and is included in that #571 tree. Post-merge Helm
revalidation is push run
[36246186742](https://github.com/caesium-cloud/caesium/actions/runs/36246186742)
on `ec893213`: `helm-integration-test` (1), (2) and (3),
`helm-pod-replacement-test` and `ci-ok` succeeded. That revalidation does not
qualify F2.
[#574](https://github.com/caesium-cloud/caesium/pull/574) merged at
`ddf83af27395fd2397a5cccd2f73c1a483c88b30`, tested head `63ce9768`. It records
cgroup anonymous and file memory, process RSS and Go heap at the same 1Gi cap.
[CI run 36263025543](https://github.com/caesium-cloud/caesium/actions/runs/36263025543)
has 37 successes and 6 skips. No live cluster run of that sampler is recorded,
so it does not explain the OOM or check F2.
[#575](https://github.com/caesium-cloud/caesium/pull/575) merged the console
owner-crash journey; that is not an F2 result, and its manifest row stays
`absent`. The unresolved ordinal-0/bootstrap and isolated rollback
prerequisites remain explicit in the
[plan's F1/F2 record](exec-plans/active/distributed-testing.md).
Native dqlite's 8,192 retained Raft entries are a possible contributor to memory
pressure. The `289bb343` artifacts cannot separate Go heap, native allocations
and file cache; the merged sampler is what a later live run must collect.

### Fenced core failures (distributed-testing W5/B3)

B3 adds `TestCore` on the same kind/Helm harness as B1 and B2. It is **not**
the `early-evidence` lane. That lane still runs `^TestOwnerCrash$`.

```sh
(
set -eu
test -z "$(git status --porcelain)"  # run from a clean candidate checkout
CANDIDATE_SHA=$(git rev-parse HEAD)
PLATFORM=$(just --evaluate platform)
just tag="$CANDIDATE_SHA" build-release
docker build --platform "$PLATFORM" \
  --build-arg BUILDER_IMAGE="caesiumcloud/caesium-builder:${CANDIDATE_SHA}" \
  --build-arg CAESIUM_IMAGE="caesiumcloud/caesium:${CANDIDATE_SHA}" \
  --target instrumented-server \
  -t "caesiumcloud/caesium:${CANDIDATE_SHA}-testfault" \
  -f build/Dockerfile.robustness .
CAESIUM_ROBUSTNESS_RUN='^TestCore$' \
CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE="caesiumcloud/caesium:${CANDIDATE_SHA}-testfault" \
  just tag="$CANDIDATE_SHA" robustness-test
)
```

`scripts/robustness.sh` gives that selection a 70-minute runner timeout and
requires the declared subtests (terminal fence, frozen retry, fan-in, auth,
cancel race, response loss, stale generation, benching, quorum loss). The
durable-event crash subtest is required only when
`CAESIUM_ROBUSTNESS_INSTRUMENTED_IMAGE` is set; without it the case is
inconclusive, not a pass. A 2–1 split must show nonzero iptables drop counters
before any minority result counts. Catalog rows remain `status: absent` with
empty gates; G6 owns their registration and promotion. W6's B3 repair (#564) fixed
terminal completion fencing and the live scenario's public-versus-durable task
identity checks. Its final reviewed head `ee9f7541` passed all 12/12 `TestCore`
subtests on an owned persistent three-member kind cluster; cancellation
returned `409/terminal_run`, with a cancelled first run, successful replacement
and no late task-success event. That proof predates #560's run-start changes.
A fresh run on merged master `f6acf0ea188632e3054f36acfbef015f5e077067`,
which includes #560, built release and instrumented images and passed all
12/12 `TestCore` subtests on an owned persistent three-member kind cluster in
299.68 s. Its raw log is local and ephemeral:
`/tmp/caesium-w6-b3-current.isBpc5/robustness/robustness.test.log`; the durable
summary is in [W6/N-1 #568](https://github.com/caesium-cloud/caesium/pull/568).
This revalidates B3 on merged code. The `early-evidence` selector still runs
`^TestOwnerCrash$`, and the core suite remains a local command.

### Coverage collection with provenance (distributed-testing W5/G2)

`build/Dockerfile.coverage` is a separate image from the release image and
from the performance images. The command is `scripts/integration-coverage.sh`
(no justfile recipe). It refuses a dirty tree, stamps
`org.opencontainers.image.revision`, and deletes previous covdata under the
artifact directory before measuring. `CAESIUM_COVERAGE_SKIP_BUILD=1` reuses an
image and records it as supplied/unverified.

```sh
CAESIUM_COVERAGE_ID="cov-$(uuidgen | tr '[:upper:]' '[:lower:]' | tr -d - | cut -c1-12)" \
CAESIUM_COVERAGE_ARTIFACTS="$(mktemp -d)" \
  bash scripts/integration-coverage.sh
```

`scripts/check-coverage.py` ignores `init()` coverage. A write-to-read pass
needs the named apply/export functions and the server profile, not a merged
profile that only imported those files. A missing provenance file is
incomplete. A baseline is written only when the verdict is `pass`, and a
baseline this script writes can be passed back with `--ratchet`. A killed
server or a nonzero stop other than exit 143 is incomplete, not 0%. This is
not a CI job. The collect that existed before review fix `7dcef7f0` is not
evidence for that commit. A fresh image labelled with reviewed head `7dcef7f0`
later produced `verdict: pass`, complete CLI/server/integration profiles and
covered the apply→export write/read path (7.6% integration coverage). No
browser profile was supplied and no package/diff ratchet was committed, so G2
acceptance remains open.

The later [G2 follow-up #570](https://github.com/caesium-cloud/caesium/pull/570)
collected real Chromium evidence on `00ba7d99`
(`/tmp/caesium-w6-g2-browser.IuzH9B`): both browser scenarios passed on their
first attempt with no skips or flaky outcomes; CLI, server, integration and
browser provenance were complete, and `all_surfaces` coverage was 9.0%.
That collection failed the old percentage floors after already-merged source
changes increased the denominator. The independently reviewed baseline refresh
keeps or raises absolute floors, and replaying the retained profile against it
passed. A historical collection on #570 head `57548a21` at
`/tmp/caesium-w6-g2-final.5sormO` exited 0 with `verdict=pass` and the committed
ratchet applied. Both real Chromium journeys passed on their first attempts;
CLI, server, integration and browser profiles have complete matching
candidate/image provenance. Integration is 7.6%, browser 8.1%, and their union
is 9.0% (4,666/51,628 statements). The actual apply→export request/write/read
path passed. These raw artifacts are local and ephemeral; the linked repair
PR holds the durable summary. Subsequent review found eligibility and immutable-image gaps.
Merged #570 (`38a2e9d3`), tested candidate `74067f98`, repairs those paths
and passed 72 focused Python regressions plus shell syntax/ShellCheck checks. Fresh actual builder/collection at
`/tmp/caesium-w6-round2-g2-collect.IMDFDt` exited 0: two first-attempt Chromium
passes, complete matching provenance, 7.6% integration, 8.1% browser and 9.0%
union coverage (4,666/51,628), with all 67 package floors applied. All runtime
and builder-tool launches use immutable image IDs; Docker FROM uses a verified
named digest reference, failing closed if it becomes unavailable.
The actual apply→persisted alias lookup→manifest export path is covered.
Exported YAML bytes were discarded, so no retained value-equality assertion
is claimed. All 149 package manifests/480 source hashes and image-bound build
contexts match. Missing audited executable profiles remain eligible. Exclusions
now reflect no function body/literal, zero-statement profiles or Go build
constraints; separate-module reagents changes are explicitly unmeasured and
outside the root-module diff scope, not permanently incomplete. Requiring
reagents coverage still fails when its profile is missing.
Diff metadata records base `f6acf0ea`, zero input/eligible Go paths and
`empty_diff=true`; zero uncovered is policy, not a live nonempty diff
measurement. Eight critical-contract coverage gaps remain. Current CI run
36243611300 passes all 37 executed jobs; the prior `609cca31` green run is
historical. #570 merged at `38a2e9d3`; its collector/ratchet and all 480 audited
source hashes match this tested content, so G2 collection/ratchets are now
accepted. Reported contract gaps and G6 promotion remain outstanding.

The max-zero diff floor is explicit policy and is not ready for G6 promotion.
A reproducible replay at master `f6acf0ea` took the last 60 first-parent commit
diffs and evaluated each changed filename list against retained current
integration/browser profiles, audited packages and source inventory. The
prior `609cca31` policy failed 12 of 18 eligible samples (six passed).
After the eligibility fixes, `74067f98` fails 11 of 18 (seven pass; none
incomplete). This maps current coverage onto historical touched filenames;
it is neither execution of historical source nor historical CI outcomes.
Genuinely uncovered changed code still needs journeys before promotion.
Local, ephemeral details: `/tmp/caesium-w6-round2-new-policy-replay.json`.

### Base/candidate performance comparison (distributed-testing W5/E3)

`scripts/performance.sh` builds both SHAs with `just build-release`, records
each side's `caesium-builder:$sha` image ID and `go version`, and refuses a
dirty tree. `log` goes to stderr so a captured build status cannot look
successful when `just` failed. Warm repetitions alternate between the two
servers. Release images must be uninstrumented.

Historical master `f6acf0ea` refused pre-#560 comparison bases, including
W4 `45994929`, because #566's guard compared unrelated test helpers. Merged
#567 (`8f64997b`, tested `253cca28`) now isolates Go-selected production files
and pinned benchmark fixtures, removing that refusal. The command below is
available on current master. The `655c063f` run below is historical.
The later ten-repeat comparison of `ec893213` against W4 `45994929` exited 3,
`overall=inconclusive`, `speed_compared=true`, with noisy
`browser.action_to_render_ms.live` and cold workload duration. Its browser
series predate the #575 events-client change, and `ec893213` is no longer
current master. No E3 acceptance is claimed.

```sh
CAESIUM_PERF_ID="perf-$(uuidgen | tr '[:upper:]' '[:lower:]' | tr -d - | cut -c1-12)" \
CAESIUM_PERF_ARTIFACTS="$(mktemp -d)" \
CAESIUM_PERF_BASE_SHA="<base>" \
CAESIUM_PERF_CANDIDATE_SHA="$(git rev-parse HEAD)" \
CAESIUM_PERF_REPEATS=10 CAESIUM_PERF_BROWSER=1 \
  bash scripts/performance.sh
```

W6's E3 repair (#566) measures the same benchmark source on both sides:
`performance.sh` copies the candidate's two benchmark files into its temporary
base worktree **after** building the release images, hashes them and the shared
test helpers into a manifest, and uses `performance-benchmarks.sh` for one
alternating Go sample per side per repeat. The comparator requires complete
named benchmark rows and zero-exit evidence in every paired repeat, plus
matching source/image/harness provenance. A base harness compile failure is
reported separately from a measured base benchmark failure. Cleanup verifies
the exact linked worktree and overlay before removing it. The command above
enables browser measurements explicitly; the script default is
`CAESIUM_PERF_BROWSER=0`.

`scripts/compare-performance.py` exits 0 only when every metric is `faster` or
`no_significant_difference`. Any inconclusive metric, undersampled series, or
provenance mismatch (including a different `go version`) is inconclusive.
Direction follows Mann-Whitney U and the Hodges–Lehmann shift, not the mean.
`error_rate`, `failure_rate` and `drop_rate` are lower-is-better; an unknown
metric name fails. Bundle bytes come from
`node ui/scripts/check-bundle-size.mjs --json` (same `BUNDLE_*` limits as CI),
not from a Python gzip of the files. Browser rows are keyed by metric, route
and kind. Compare an already-written report without Docker:

```sh
CAESIUM_PERF_ARTIFACTS=/tmp/perf bash scripts/performance.sh compare
```

The prior E3 implementation head `30250322` failed its first live comparison:
the base lacked the new benchmark functions and two browser series were
inconclusive. A later ten-repeat run on repair head `87546de6` used matched
uninstrumented images and complete paired benchmark samples, but returned
`overall=inconclusive` (one noisy browser series and one slower warm workload).
The merged repair head `08b8bd26` changed harness validation after that run.
Follow-up [#567](https://github.com/caesium-cloud/caesium/pull/567) isolates
each benchmark process to its side's Go-selected production files, one pinned
test helper, and the two shared benchmark sources; it merged at `8f64997b`. A full ten-repeat run on `b9c7bf17` against W4 base `45994929`
completed all 20 paired benchmark samples and cold/warm workloads, but the
base's first Chromium repeat logged `net::ERR_INTERNET_DISCONNECTED` and
Playwright exited 1. The comparator returned `overall=fail` with
`speed_compared=false`, correctly withholding a speed verdict.

The bounded retry on `655c063f` completed exit 3 with
`overall=inconclusive`, `speed_compared=true`; artifacts are
`/tmp/caesium-w6-e3-diagnostics.egK8Ux`. All 20 paired benchmark samples,
cold/warm workload correctness, and 20 first-attempt Chromium repeat sets
passed (80 tests, no skips or flaky outcomes). Per-repeat Playwright JSON and
diagnostics are retained separately. Forty-one metrics reported no significant
difference; `browser.route_readiness_ms./jobs.live` was inconclusive because
candidate CV 1.980 exceeded 0.3, with one 3,276 ms sample among ten. That sample's
cause is unproved: it occurred in candidate repeat 2 at
`2026-09-26T03:00:51.586Z`, and passing browser attempts retained no trace;
per-repeat server logs were not saved before removal. Future investigation
needs those logs and a request timeline. No outlier was removed or noise
threshold changed. Merged #567 (`8f64997b`), tested head `253cca28`, fixes a timer-dependent
test fixture with an explicit `ns/op` metric: 18 focused tests and 400/400
repeated fixture rows passed on Go 1.27.1 darwin/arm64, and a package-mode
mutation still triggered `TestMain` exit 99 and failed the isolation test.
This test-only change has no full live comparison of its own; the `655c063f`
result above is historical. The later `ec893213` comparison is the newest
recorded full run and is still inconclusive. Current-head CI run
[36214372032](https://github.com/caesium-cloud/caesium/actions/runs/36214372032)
succeeded, including `ci-ok`; #567 is merged. E3 acceptance
remains open pending conclusive repeatable measurement.
This is not a CI job or a calibrated SLO (E4 / Q2 / Q5).

### Checker-strength mutation validator (distributed-testing W6/C3)

Run the standalone C3 validator on a clean, committed candidate:

```sh
bash scripts/validate-test-oracles.sh
```

It clones the candidate into a temporary isolated checkout, runs named model,
history and robustness probes, then applies eight recorded known-bad patches
one at a time. Each mutant must fail its named test with the expected oracle
assertion; a compile error, timeout, resource failure, missing test or missing
assertion marker cannot count as a successful rejection. Legal duplicate
delivery and ambiguous response timeout histories must still pass on the
candidate. The merged C3 head passed the candidate probes and all eight
mutation rejections. Its ordinary Go/Python probes run in existing unit/config
lanes; the full mutation script is not yet a CI job (G6 owns promotion).

## 5. Server env per lane, and the silent-drift rule

**Rule: a lane that starts its own server goes red silently.** Any lane that
does not go through an `integration-up*` justfile recipe does not
automatically pick up new env a feature adds to those recipes — its
`docker run -e` (or Helm values) block has to be updated by hand, in the same
PR, or the lane drifts until something it depends on breaks.

Lanes that go through `just integration-up*`:
`integration` Docker (`integration-up`) and agent-auth (`integration-up-agent`),
`integration-extra` distributed / owner-memory / infra,
`integration-arm64` Docker / infra.
CI sets `CAESIUM_SKIP_IMAGE_BUILD=true` and docker-loads the `images`
job's artifacts first, so those recipes do not recompile.

Lanes that set server env inline and do **not** go through any
`integration-up*` recipe:

- `ui-e2e` and `ui-e2e-auth` — own inline `docker run` in `ci.yml`, closely
  mirroring but not generated from the `ui-e2e`/`ui-e2e-auth` justfile
  recipes (which exist for local dev and call `build-release`; CI reuses the
  pre-built `product-amd64` artifact instead).
- `podman-integration-test` — own inline `docker run` in `ci.yml`, mirroring
  but not calling the `integration-test-podman` justfile recipe. The Go
  invocation runs the full suite in three shards.
- `helm-integration-test` — server env comes from Helm values
- `helm-pod-replacement-test` — server env comes from
  `helm/caesium/ci/test-values-replacement.yaml`
  (`helm/caesium/ci/test-values-k8s.yaml`), not from any `docker run -e`
  block or justfile recipe; env parity has to be checked in that values file
  separately. After `helm test`, the Go invocation runs the full suite
  across three shards.

### Parity rule and its guardrail

`integration-up-distributed`, `integration-up-owner-memory`,
`integration-up-infra`, and `integration-up-agent` (justfile) must each set
every `CAESIUM_*` feature-gate env var that `integration-up` sets, plus
whatever lane-specific vars that lane legitimately needs (distributed
topology, auth, agent remediation, etc.). Each of these recipes starts its
own server rather than inheriting from `integration-up`, so a feature that
only adds env to `integration-up` silently stops being exercised on the
others unless the same env is copied over by hand, in the same PR.

This was true in practice: `integration-up-distributed` and
`integration-up-owner-memory` were missing `CAESIUM_CACHE_ENABLED=true` (so
every "must not be cached" assertion passed vacuously on those two lanes),
and `integration-up-agent` was additionally missing
`CAESIUM_RUN_QUEUE_ENABLED` (and its dequeuer/interval siblings),
`CAESIUM_RATE_LIMIT_PRUNER_ENABLED` (and its interval sibling), and
`CAESIUM_FANOUT_MAX_PARTITIONS`. Closed by
[issue #425](https://github.com/caesium-cloud/caesium/issues/425); the
`podman-integration-test` instance of this same class of gap
(`CAESIUM_DATABASE_SHARDS`, `CAESIUM_FANOUT_MAX_PARTITIONS`) was closed
earlier by Stream D1(b) (PR #422).

The same guard also catches drift introduced across two PRs merging close
together: while #450 was open, #447 added `CAESIUM_CANCEL_RECONCILE_INTERVAL`
to `integration-up` (and `integration-test-podman`), which the merge-eligibility
review on #450 flagged as a new omission on all four tracking recipes before
either PR landed. #450 picked up the var on all five `integration-up*`
recipes so the two PRs are parity-safe in either merge order.

`TestIntegrationUpRecipesTrackBaselineEnv`
(`internal/guardrails/guardrails_test.go`) enforces this going forward: it
parses `justfile`, diffs each `integration-up-*` recipe's `CAESIUM_*` env vars
against `integration-up`'s, and fails on anything missing that isn't in that
test's explicit `integrationUpEnvAllowlist` (empty today — every recipe
tracks full parity). Add a new feature-gate var to `integration-up` and this
test fails everywhere else until you copy it over or add a justified
allowlist entry explaining why that lane is exempt.

## 6. Release procedure (for `v*` tags)

1. Merge E1–E3 (static per-arch CLI binaries, `just cli`, chart versioning).
2. Confirm every job in `publish.needs` (§3) is green on the commit you're
   about to tag — not just the required-to-merge set.
3. Cut an **annotated** tag on the merge commit and push it:
   ```sh
   git tag -a v0.1.0 -m "v0.1.0"
   git push origin v0.1.0
   ```
4. The `publish` job then:
   - verifies `helm/caesium/Chart.yaml` `appVersion` equals the pushed tag
     before anything is pushed (fails loudly if it doesn't — every `v*` tag
     must bump `appVersion` to the tag, and `version` per semver, in the
     merge PR);
   - verifies the two `.smoke-ok` markers (one per architecture — the CLI
     binary ran natively on its own runner, `ubuntu-24.04` for amd64,
     `ubuntu-24.04-arm` for arm64) and their `sha256sum`s before attaching
     any release asset;
   - writes `SHA256SUMS` over the release binaries;
   - pushes the multi-arch `caesiumcloud/caesium:<tag>` manifest (amd64 +
     arm64) plus multi-arch manifests for the four reagent images
     (`git-source`, `tf-discover`, `tf-warm`, `tf-runner`);
   - creates the GitHub release idempotently (`gh release view` then
     `create` or `upload --clobber`), so re-running `publish` against the
     same tag is safe.
5. After `publish` succeeds, verify:
   - the release page lists `caesium-linux-amd64`, `caesium-linux-arm64`,
     `SHA256SUMS`;
   - `docker manifest inspect caesiumcloud/caesium:<tag>` lists both
     `linux/amd64` and `linux/arm64`;
   - `just tag=<tag> cli` yields a runnable `./.tmp/caesium-cli/caesium --help`
     on the host.

### `v0.1.0` image digests

Recorded from `docker buildx imagetools inspect caesiumcloud/caesium:v0.1.0`
after the tag's `publish` job (run 34180615494) on 2026-09-07. Release:
<https://github.com/caesium-cloud/caesium/releases/tag/v0.1.0>
(`caesium-linux-amd64`, `caesium-linux-arm64`, `SHA256SUMS`).

| Artifact | Digest |
| --- | --- |
| `caesiumcloud/caesium:v0.1.0` (multi-arch manifest) | `sha256:2e6996f965ab7899ac3f2d80a7607e26a96ff607d24baf8566033d6d7aa73917` |
| `caesiumcloud/caesium:v0.1.0` linux/amd64 | `sha256:d93d21e776665039bbf5cc3dc41be7a0b402ae9f2e83d446f2fb05a16d52288b` |
| `caesiumcloud/caesium:v0.1.0` linux/arm64 | `sha256:56569860e7bfc33ca84998648a0d0d72bb87c35194299cb02f54da52104d932d` |

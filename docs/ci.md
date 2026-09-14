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
coverage; plus the remaining distributed failure tests, Console fault journeys
and performance gates, are tracked in
[Distributed Testing and Performance Confidence](exec-plans/active/distributed-testing.md).

W3 added one job to the workflow (`early-evidence`) and two dependencies to
`ci-ok` (`early-evidence` and `helm-lint`). It changed **no repository
settings**: `ci-ok` is still absent from master's required status checks, so
that promotion gates `v*` publication rather than PR merge. See §1 and
"Early evidence lane and the promoted gate" in §4.

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
the sole required context.

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
podman-integration-test
```

Concretely: a PR can merge into `master` on the strength of `ci-ok`
alone, while `integration-extra` (distributed / owner-memory / infra),
`integration-arm64`, `helm-lint`, `helm-integration-test`, and
`podman-integration-test` — exactly the lanes §2 leaves non-required —
may have run on that same commit but did not block the merge. Do not
assume a green required set means `master`'s tip is taggable: before
pushing a `v*` tag, check that every job in `publish.needs` is green on
the commit you intend to tag (see §6). Arm64 product, reagents, and
integration run on every PR in parallel with the amd64 twins; they are
required-to-merge for the builder, product/static CLI smoke, and
`unit-test-arm64`; the arm64 integration matrix remains optional.

## 4. Job matrix and artifact flow

Triggers: `pull_request` to `master`, `push` to `master`, `v*` tags.
Feature-branch pushes do **not** run CI (the PR event covers them). A
`concurrency` group cancels superseded PR runs.

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
| `ui-e2e` | ubuntu-24.04 | `[ui-test, images]` | 45 | inline `docker run` reusing the `product-amd64` artifact | inline `docker run --name caesium-server` |
| `ui-e2e-auth` | ubuntu-24.04 | `[ui-test, images]` | 45 | inline `docker run` | inline `docker run --name caesium-server-auth` |
| `helm-integration-test` | ubuntu-24.04 | `[images, helm-lint]` | 60 | kind + `helm install` + `helm test` + full suite in three shards | kind pod via the Helm chart |
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
with no automatic dated baseline file. JSON `schema_version: 1` records configuration without
credentials, expected/observed/outcome counts, per-run identity/status/times,
samples, row/statement deltas, failure class and workload interval. `observed`
means an acknowledged run ID; an uncertain trigger response does not prove
that the server rejected the write. Reconcile all expected work, including
untriggered work and uncertain admission, before interpreting throughput.

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
SLOs, an arrival-rate driver, or proof of multi-node fault tolerance. Those
remain later plan items.

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
`/bin/robustness.test -test.v -test.run '^TestOwnerCrash$' -test.timeout 15m`.

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
`gates`**: the file names those scenarios and certifies none of them. B2, B3 and
D3 register theirs once their runners exist, and G6 wires the full suite. Do not
read an `absent` row as coverage.

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

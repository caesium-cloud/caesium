# CI Runbook

This is the operator reference for `.github/workflows/ci.yml`: which checks
gate a merge into `master`, which checks gate a `v*` tag publish, the job
matrix and its per-lane server env, and the release procedure. For the
general execution-mode / worker / dqlite env reference (what each
`CAESIUM_*` variable does), see
[parallel-execution-operations.md](parallel-execution-operations.md) — this
doc does not repeat that material, only the CI-specific wiring.

Shipped W1 load reporting and browser evidence, plus remaining distributed
failure tests, developer/Console journeys and performance gates, are tracked in
[Distributed Testing and Performance Confidence](exec-plans/active/distributed-testing.md).
That plan does not change the current required checks described below.

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

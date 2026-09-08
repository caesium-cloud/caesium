# CI Runbook

This is the operator reference for `.github/workflows/ci.yml`: which checks
gate a merge into `master`, which checks gate a `v*` tag publish, the job
matrix and its per-lane server env, and the release procedure. For the
general execution-mode / worker / dqlite env reference (what each
`CAESIUM_*` variable does), see
[parallel-execution-operations.md](parallel-execution-operations.md) — this
doc does not repeat that material, only the CI-specific wiring.

## 1. Required-to-merge checks

Branch protection on `master` requires exactly these checks, `strict: false`:

- `lint`
- `unit-test`
- `unit-test-arm64`
- `ui-test`
- `ui-e2e`
- `ui-e2e-auth`
- `build-and-integration-test`
- `build-and-integration-test-agent-auth`

These names are the GitHub Actions job ids in `.github/workflows/ci.yml` —
verified against a live run's check-runs (`gh api
repos/caesium-cloud/caesium/commits/master/check-runs`), not guessed from the
YAML alone.

`ui-e2e-auth` is required — and not merely nice-to-have — because it is the
**only** job in the whole workflow that exercises a scoped API key against
`GET /auth/whoami` and asserts the 200 (`ui/e2e/auth/auth-smoke.spec.ts`, "a
job-scoped key is denied the global whoami" pins the *deny* case, and the
paired auth-mode assertion pins the *allow* case for a workspace-scoped key).
No unit test, no other integration lane, and no other e2e project runs that
request. If `ui-e2e-auth` is ever removed from required checks, that
assertion silently stops gating merges.

### Command

```sh
cat > .tmp/required-checks.json <<'EOF'
{
  "strict": false,
  "checks": [
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

### Verification

```sh
gh api repos/caesium-cloud/caesium/branches/master/protection --jq '.required_status_checks.checks[].context'
```

Expected output is the eight contexts above, in any order.

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

- **Required-to-merge** (§1 above, branch protection `required_status_checks`):
  8 jobs. This is the fast, cheap gate every PR must clear.
- **Required-to-publish** (`publish.needs` in `.github/workflows/ci.yml`,
  quoted verbatim below): 16 jobs. This is the full lane matrix, and it is
  what actually runs before the `publish` job pushes images and cuts a
  release — `publish` only triggers `if: startsWith(github.ref, 'refs/tags/v')`,
  so it isn't itself a PR-merge gate.

```
lint
unit-test
unit-test-arm64
ui-test
ui-e2e
ui-e2e-auth
build-and-integration-test
build-and-integration-test-distributed
build-and-integration-test-owner-memory
build-and-integration-test-agent-auth
build-and-integration-test-infra
build-and-integration-test-infra-arm64
build-and-integration-test-arm64
helm-lint
helm-integration-test
podman-integration-test
```

Concretely: a PR can merge into `master` on the strength of the 8
required-to-merge checks alone, while `build-and-integration-test-distributed`,
`-owner-memory`, `-infra`, `-infra-arm64`, `-arm64`, `helm-lint`,
`helm-integration-test`, and `podman-integration-test` — exactly the lanes
§2 leaves non-required — ran on that same commit but did not block the
merge. Do not assume a green required set means `master`'s tip is taggable:
before pushing a `v*` tag, check that every job in `publish.needs` is green
on the commit you intend to tag (see §6).

## 4. Job matrix

| Job id | runs-on | needs | timeout (min) | justfile recipe(s) / inline | Server started |
| --- | --- | --- | --- | --- | --- |
| `builder` | ubuntu-24.04 | — | 30 | `builder`, `builder-full` | none |
| `builder-arm64` | ubuntu-24.04-arm | — | 30 | `builder`, `builder-full` | none |
| `lint` | ubuntu-24.04 | `builder` | 30 | `lint`, `reagents-lint` | none |
| `helm-lint` | ubuntu-24.04 | — | 10 | inline `helm lint`/`helm template` (mirrors justfile `helm-lint`/`helm-template`) | none |
| `unit-test` | ubuntu-24.04 | `builder` | 30 | `unit-test`, `reagents-test` | none |
| `unit-test-arm64` | ubuntu-24.04-arm | `builder-arm64` | 30 | `unit-test` only — no `reagents-test` on arm64 | none |
| `ui-test` | ubuntu-24.04 | `builder` | 30 | `ui-lint`, `ui-test` | none |
| `build-and-integration-test` | ubuntu-24.04 | `builder` | 45 | `build`, `integration-test` → `integration-up` | `integration-up` |
| `build-and-integration-test-distributed` | ubuntu-24.04 | `builder` | 45 | `build`, `integration-test-distributed` → `integration-up-distributed` | `integration-up-distributed` |
| `build-and-integration-test-owner-memory` | ubuntu-24.04 | `builder` | 45 | `build`, `integration-test-owner-memory` → `integration-up-owner-memory` | `integration-up-owner-memory` |
| `build-and-integration-test-agent-auth` | ubuntu-24.04 | `builder` | 45 | `integration-test-agent` → `integration-up-agent` (no separate `build` step; the image comes from `build-test` inside the recipe) | `integration-up-agent` |
| `build-and-integration-test-infra` | ubuntu-24.04 | `builder` | 60 | `integration-test-infra` → `integration-up-infra` | `integration-up-infra` |
| `build-and-integration-test-infra-arm64` | ubuntu-24.04-arm | `builder-arm64` | 60 | arm64 twin of the above | `integration-up-infra` (arm64) |
| `build-and-integration-test-arm64` | ubuntu-24.04-arm | `builder-arm64` | 45 | `build`, `integration-test` → `integration-up` | `integration-up` (arm64) |
| `ui-e2e` | ubuntu-24.04 | `[ui-test, build-and-integration-test]` | 45 | none — inline `docker run` reusing the `release-amd64` artifact (mirrors, but is not generated from, the `ui-e2e` justfile recipe) | inline `docker run --name caesium-server` |
| `ui-e2e-auth` | ubuntu-24.04 | `[ui-test, build-and-integration-test]` | 45 | none — inline `docker run` (mirrors, but is not generated from, `ui-e2e-auth`) | inline `docker run --name caesium-server-auth` |
| `helm-integration-test` | ubuntu-24.04 | `[build-and-integration-test, helm-lint]` | 60 | kind cluster + `helm install` with `helm/caesium/ci/test-values-k8s.yaml`, then inline `go test` | kind pod via the Helm chart |
| `podman-integration-test` | ubuntu-24.04 | `[build-and-integration-test]` | 45 | none — inline `docker run` (mirrors, but is not generated from, `integration-test-podman`) | inline `docker run --name caesium-server-podman` |
| `publish` | ubuntu-24.04 | see §3 | 30 | none — direct `docker push`/`docker manifest`, and release asset upload (§6) | none |

Per-lane `-run` filters, `-timeout` values, and PASS-floor variables
(`*_integration_min_pass`) are execution-mode wiring, not CI wiring — see
[parallel-execution-operations.md](parallel-execution-operations.md) and the
justfile recipes named above for the current values; they change more often
than this doc should need to.

## 5. Server env per lane, and the silent-drift rule

**Rule: a lane that starts its own server goes red silently.** Any lane that
does not go through an `integration-up*` justfile recipe does not
automatically pick up new env a feature adds to those recipes — its
`docker run -e` (or Helm values) block has to be updated by hand, in the same
PR, or the lane drifts until something it depends on breaks.

Lanes that go through `just integration-up*`:
`build-and-integration-test` (`integration-up`),
`build-and-integration-test-distributed` (`integration-up-distributed`),
`build-and-integration-test-owner-memory` (`integration-up-owner-memory`),
`build-and-integration-test-infra`/`-infra-arm64` (`integration-up-infra`),
`build-and-integration-test-agent-auth` (`integration-up-agent`).

Lanes that set server env inline and do **not** go through any
`integration-up*` recipe:

- `ui-e2e` and `ui-e2e-auth` — own inline `docker run` in `ci.yml`, closely
  mirroring but not generated from the `ui-e2e`/`ui-e2e-auth` justfile
  recipes (which exist for local dev and call `build-release`; CI reuses the
  pre-built `release-amd64` artifact instead).
- `podman-integration-test` — own inline `docker run` in `ci.yml`, mirroring
  but not calling the `integration-test-podman` justfile recipe.
- `helm-integration-test` — server env comes from Helm values
  (`helm/caesium/ci/test-values-k8s.yaml`), not from any `docker run -e`
  block or justfile recipe; env parity has to be checked in that values file
  separately.

### Known gap (re-verified at this doc's HEAD)

`integration-up-distributed` and `integration-up-owner-memory` (justfile) do
not set `CAESIUM_CACHE_ENABLED=true`, unlike `integration-up` and
`integration-up-infra` (the latter's comment explains why it's needed: without
it, every "must not be cached" assertion passes vacuously). The
`podman-integration-test` instance of this same gap (`CAESIUM_DATABASE_SHARDS`,
`CAESIUM_FANOUT_MAX_PARTITIONS`) was closed by Stream D1(b) (PR #422).

`integration-up-agent` is missing four variables that every other
`integration-up*` recipe sets: `CAESIUM_CACHE_ENABLED`,
`CAESIUM_RUN_QUEUE_ENABLED` (and its dequeuer/interval siblings),
`CAESIUM_RATE_LIMIT_PRUNER_ENABLED` (and its interval sibling), and
`CAESIUM_FANOUT_MAX_PARTITIONS`.

This is a known, tracked gap, not a fixed one — see
[issue #425](https://github.com/caesium-cloud/caesium/issues/425) for the
exact diff and remediation.

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

Filled in by Stream E4 once the tag is cut.

| Artifact | Digest |
| --- | --- |
| `caesiumcloud/caesium:v0.1.0` (multi-arch manifest) | _pending E4_ |
| `caesiumcloud/caesium:v0.1.0` linux/amd64 | _pending E4_ |
| `caesiumcloud/caesium:v0.1.0` linux/arm64 | _pending E4_ |

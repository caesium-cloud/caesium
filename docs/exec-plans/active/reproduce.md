# `caesium reproduce` — Re-Execute a Historical Production Task Locally

Last updated: 2026-07-09

This plan ships `caesium reproduce <run-id> --job-id <id> --task <name>`: a
client-side verb that pulls a completed task's immutable
`TaskExecutionDescriptor` from the server and re-executes that single task on the
operator's laptop under their local Docker daemon — the same runtime `caesium
dev` rides. Where `caesium why` names *which* input changed and `verify` attests
digests, `reproduce` runs the task *with those exact inputs* (recorded image
digest, literal env, `CAESIUM_PARAM_*`, and predecessor `CAESIUM_OUTPUT_*`), lets
the operator tweak one thing (`--set`, `--set-env`, `--image`), and run it again.
It turns a 03:00 "rebuild the container invocation from memory over SSH" loop into
one local command.

The current state is that everything needed already exists but is unreachable: the
descriptor is persisted per `TaskRun` (`internal/models/run.go:121`,
`ExecutionDescriptor datatypes.JSON` with `json:"-"` — never serialized on any
surface today), quarantined replay
([`internal/replay/replay.go`](../../../internal/replay/replay.go)) already proves
the envelope reconstructs a task byte-for-byte, and the env/hash reconstruction
(`pkgtask.BuildOutputEnv` over `descriptor.DAG.PredecessorOutputs`) is written and
tested. The target state is one new read-only GET endpoint that serializes the
stored descriptor under the existing scoped-key auth arm, plus a CLI that fetches,
reconstructs the exact environment, pulls by recorded digest, runs locally, and —
with `--diff` — compares parsed `##caesium::output` markers against the recorded
output. **Nothing runs server-side**: no `JobRun`/`TaskRun` rows, no events, no
metrics, no quarantine machinery. The operator's own machine and locally resolved
credentials are the sandbox boundary, so secrets are left **unresolved by
default** and every best-effort dimension (mutable-tag pull, unmounted output
refs, cross-arch emulation, external state) is warned, never silent.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work backlog,
`## Sequencing & Dependencies` captures cross-stream order, and
`## Acceptance Criteria` lists the gates that close out the entire plan. Any
agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies are
   satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Source-Of-Truth Note

When this plan and [`docs/design-reproduce.md`](../../design-reproduce.md)
disagree, **the design doc wins** — it is authoritative for the feature's intent,
scope, the faithful-vs-best-effort fidelity contract (the table under "What
Reproduces Faithfully vs Best-Effort"), the exit-code semantics, the
default-deny-on-secrets security posture, and the three non-goals (no server-side
execution, no multi-task local DAG re-run, no secret-value transport). No item
may add a new endpoint, CLI flag, or execution mode beyond what the design
enumerates without first amending the design. In particular: `reproduce` makes
**no job-schema change** (it reads existing `task_runs.execution_descriptor`, so
`pkg/jobdef/definition.go` and `internal/cache/hash.go` are untouched), creates
**no new GORM model** (no `internal/models/models.go` registration), and moves
**no metric** server-side (client feature). Strategic priority/status lives in
[`docs/roadmap.md`](../../roadmap.md) Phase 4 "Data-Plane Differentiators" table
(the roadmap wins on priority/status disagreements). Two design **Open Questions**
(#2 localrun-vs-Docker-adapter reuse, #3 attempt/retry semantics) are settled
inline by the items below; the rest (#1 pruned-descriptor messaging, #4
distroless `--shell-image` fallback, #5 explain/mini-`why` integration) are
recorded as deferred and must not be silently pulled into scope.

## Progress (as of 2026-07-08)

### Wave 1 — shipped 2026-07-08 (A1, A2, H-1; 3 of 11 items)

Run via the `exec-plan-wave` skill (codex implementation for Stream A,
orchestrator-implemented harness stream; GHA CI as the verify gate). Merge
order η → α:

- **W1-η (H-1)** — digest pinning was the one missing harness gate:
  `CAESIUM_CACHE_PIN_DIGESTS` was set on NO server-boot site, so descriptors
  recorded no `ResolvedImageDigest`. Now `=true` on every lane in one sweep
  (all justfile lanes, local k8s helm `--set`, the CI helm values file, and
  the three ci.yml server blocks) — degradation-safe because the imagecheck
  resolver falls back to the tag on registry failure. Docker-daemon
  reachability from the test-runner and the split-stream `runCLIStdout`/
  `runCLISeparate` helpers were verified already present. PR #334.
- **W1-α (A1+A2)** — the entire server-side surface:
  `GET /v1/jobs/:id/runs/:run_id/tasks/:task/descriptor` (task resolved by
  name with UUID fallback, both path UUIDs guarded up front, Viewer RBAC under
  the existing scoped-key arm) returns the RAW stored descriptor bytes in a
  wrapper (`task_run_id`, `status`, `result`, recorded `output`,
  `replay_safe`, a log-excerpt pointer) with a stable "descriptor unavailable"
  404 for pre-descriptor rows. Live integration coverage: digest-pinned
  fixture (per-step `cache.pinDigests`, independent of η's env) round-trips
  digest/env/params/`PredecessorOutputs`; scoped-key 200/403 lanes;
  absent-descriptor error. Lane learnings: the resolved-digest assertion is
  gated to the docker engine (podman/k8s resolver paths legitimately fall back
  to the mutable tag — reproduce marks such pulls DEGRADED), and review
  removed a redundant descriptor re-fetch that would have turned future
  schemaVersion bumps into 500s, pinning the no-typed-decode raw-bytes
  contract in code. PR #335.

### Wave 2 — shipped 2026-07-09 (B1 + B2; 5 of 11 items)

One codex stream (B1 → B2 sequentially in one PR), orchestrator
verify/publish/merge:

- **W2-β (B1+B2)** — `caesium reproduce <run-id> --job-id <id> --task <name>`:
  descriptor fetch (API-key hygiene per `cmd/why`), later-wins envelope
  reconstruction (recorded literals → `CAESIUM_PARAM_*` → `CAESIUM_OUTPUT_*`
  via the exact `pkgtask.BuildOutputEnv` mapping replay uses → `--set` →
  `--set-env`; secrets omitted + named), clean-stdout `--dry-run`/`--json`
  envelope with a structured warnings array, and single-shot local execution
  through `internal/localrun` (digest-first pull with DEGRADED tag fallback,
  marker parsing, `--mount` remap with PVC skip, `--timeout`/`--platform`,
  exit codes 0/1/2 with actionable registry-auth guidance). Four CI-driven
  fixes en route, each generalizing beyond the stream: the synthesized
  one-step definition needed an explicit `type: task` (YAML defaults don't
  apply to Go-built definitions); **`internal/localrun` opened its ephemeral
  DB with GORM's default logger, which writes SQL traces to os.Stdout** —
  found by a self-diagnosing stdout capture, fixed by routing through the
  repo's zap logger (also cleans `caesium dev`); the run-mode integration leg
  is docker-engine-gated (podman/k8s test-runners have no docker.sock — the
  daemon-free dry-run/exit-2 legs run everywhere); and review hardening made
  Execute use a locally present image instead of failing a private-registry
  pull (guidance no longer references the not-yet-existing `--image`).
  Review: 1 P1 fixed (+local-image behavior), 1 P1 declined with the
  `store.go:1371` faithfulness citation (prod runs raw command strings as a
  single argv element — reproduce mirrors it), 1 P2 (typed-exit refactor)
  deferred to W3-C alongside exit 3. PR #337.

Wave 3 next: C (C1, C2, C3) then D (D1, D2) serialized on the shared command
files, N-1 last. Carry-in for W3-C: the RunE typed-exit-error refactor.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Read-only descriptor endpoint — `GET /v1/jobs/:id/runs/:run_id/tasks/:task/descriptor` under the existing scoped-key auth arm | **P0** | ✅ Shipped W1 (#335) |
| B | CLI reproduce core — `cmd/reproduce/` + `internal/reproduce/`, envelope reconstruction, `--dry-run`/`--json`, digest pull, run mode + exit codes, `--mount`/`--set`/`--set-env` | **P0** | ✅ Shipped W2 (#337) |
| C | Output-diff compare + fidelity summary + `--shell` | P1 | Next (W3, carries the typed-exit refactor) |
| D | Fix-testing (`--image`) + local secret resolution (`--resolve-secrets`) | P2 | Queued (W3, after C) |
| H-1 | Integration harness — record descriptors with digest pinning, drive the real CLI against the harness Docker daemon | — | ✅ Shipped W1 (#334) |
| N-1 | Docs — design banner, roadmap Phase 4 row, README repoint, CLI reference, sibling cross-links | — | Queued (last) |

## Streams

### Stream A — Read-only descriptor endpoint (P0 server surface)

The entire server-side surface: one authenticated read-only GET that serializes
the stored `TaskExecutionDescriptor` verbatim so the CLI has something to fetch.
It sits under the `/v1/jobs/:id` prefix in `Protected()`, so the existing
scoped-key arm (`api/middleware/auth_scope.go` `authorizeScope` →
`resolveScopedJobAlias` → `auth.CheckScope`, `:47/:120/:137`) covers it with
**zero new middleware** — exactly like the receipt and `why` routes already bound
in [`api/rest/bind/bind.go`](../../../api/rest/bind/bind.go). Mirror the
`api/rest/controller/receipt/` + `api/rest/service/receipt/` package pair; reuse
the existing loader `run.Store.TaskExecutionDescriptor(ctx, runID, taskID)`
(`internal/run/store.go`) — do not re-decode. This stream merges first (everything
downstream fetches from it).

- [x] A1. Add `GET /v1/jobs/:id/runs/:run_id/tasks/:task/descriptor`. Resolve
      `:task` by task **name** within the run (the ergonomic handle; accept a UUID
      too), then return the stored `TaskRun.ExecutionDescriptor` JSON verbatim plus
      a small wrapper (`task_run_id`, `status`, `result`, recorded `output`,
      `replay_safe`, a log-excerpt pointer). The descriptor stores `secret://` refs
      and provider identity metadata only — never values
      (`internal/models/run.go:256`) — so the endpoint cannot leak a credential even
      to a fully privileged caller. Parse and validate BOTH path UUIDs up-front (the
      receipt controller's ownership-bypass guard), and return the clean
      "descriptor unavailable" error when the row predates descriptors so the CLI can
      map it to exit 2.
      Files: new `api/rest/controller/reproduce/descriptor.go`, new
      `api/rest/service/reproduce/descriptor.go`, `api/rest/bind/bind.go` (one route
      line in the `Protected()` `/v1/jobs/:id` group).
      Note: W1-alpha added the reproduce controller/service pair, the protected route,
      viewer RBAC policy, name-first/UUID-fallback task resolution, raw descriptor
      wrapper, log pointer, and stable `descriptor unavailable` 404 body.
- [x] A2. Add the endpoint integration test: run a job with structured outputs and
      digest pinning on the live integration server, fetch the descriptor (200) and
      assert the recorded image/digest, literal env, params, and
      `PredecessorOutputs` round-trip; a **scoped key** fetches an in-scope job's
      descriptor (200) and is refused an out-of-scope job (403), following the
      receipt/`why` scope tests; a run whose descriptor is absent returns the
      "descriptor unavailable" error, not a partial payload.
      Files: new `test/reproduce_endpoint_test.go` (`//go:build integration`).
      Depends on: A1.
      Note: W1-alpha added a live endpoint round-trip with step-level
      `cache.pinDigests`, name and UUID fetch lanes, absent-descriptor refusal, and
      an auth-enabled scoped-key 200/403 route test.

### Stream B — CLI reproduce core (P0 client)

The operator-facing verb and its reconstruction engine. `reproduce` is a new
top-level Cobra command (`cmd/reproduce/`, appended to the `cmds` slice in
[`cmd/execute.go`](../../../cmd/execute.go)) backed by a reconstruction package
(`internal/reproduce/`) so the envelope logic is unit-testable apart from the
command wiring. API-key hygiene follows `cmd/why/why.go`: `CAESIUM_API_KEY`
preferred, `--api-key` accepted with a visible-in-`ps` warning. This is the
largest stream; B1 ships the inspection surface (fetch + reconstruct + print),
B2 adds real local execution.

- [x] B1. Add the `cmd/reproduce/` command + `internal/reproduce/` reconstruction
      package + `--dry-run`/`--json` envelope output. Fetch the descriptor from
      Stream A's endpoint; reconstruct the container env in this order (later wins):
      recorded `ContainerSpec.Env` literals → `CAESIUM_PARAM_<KEY>` from recorded
      `Run.Params` → `CAESIUM_OUTPUT_*` via `pkgtask.BuildOutputEnv` over
      `descriptor.DAG.PredecessorOutputs` (**reuse that mapping exactly as
      `internal/replay/replay.go` does for hash reconstruction — do not
      reimplement**) → `--set` param re-derivation → `--set-env`. Leave `secret://`
      refs **omitted by default** and name them in a warning. `--dry-run` prints the
      fully reconstructed envelope (env, image, command, mounts, warnings) as JSON
      without executing. Per the repo's stream rules, `--json`/`--dry-run` write
      **only** the JSON document to stdout; every log/warning/progress line goes to
      stderr. Append `reproduce.Cmd` to `cmd/execute.go`.
      Files: new `cmd/reproduce/reproduce.go`, new `internal/reproduce/reconstruct.go`
      (+ `reconstruct_test.go`), `cmd/execute.go`.
      Depends on: A1.
      Note: W2-beta added the top-level `caesium reproduce` command, descriptor
      fetch/decode, stdout-clean `--dry-run`/`--json` envelope output, exact
      env layering through `pkgtask.BuildOutputEnv`, secret omission warnings,
      mount remap/skips, and focused reconstruction tests.
- [x] B2. Add run mode (default) with local execution + exit codes. Pull the image
      by recorded `ResolvedImageDigest` (mutable tag → pull by tag, marked
      **DEGRADED**), execute the recorded command in the reconstructed environment,
      and parse `##caesium::output` markers from stdout via `pkgtask.ParseMarkers`.
      **Registry-auth failure is a first-class, actionable error, not a stack
      trace:** the operator's local Docker daemon may lack credentials for the
      private registry the digest lives in, so a failed pull exits `2` with guidance
      naming the registry host and pointing at `docker login <host>` or `--image
      <local-ref>` to run against a locally built/pulled image instead. A
      `execute_test.go` case asserts the pull-auth-failure message includes the
      registry and the `--image` hint.
      Ride `internal/localrun` by synthesizing a one-step definition (design Open
      Question #2 lean — gets timeouts, marker parsing, and log capture at behavioral
      parity with `caesium dev`); run exactly once, single-shot (design Open Question
      #3 lean — recorded retry/backoff is surfaced, not applied). Wire `--mount
      old=new` bind-mount remap (PVC/k8s volumeSource mounts warned + skipped),
      `--set`/`--set-env` into the actual run, `--timeout` (default recorded
      `TaskTimeout`), and `--platform`. Exit codes: `0` succeeded, `1` ran and
      failed, `2` fetch/auth/reconstruction error (incl. missing descriptor).
      Files: `cmd/reproduce/reproduce.go`, new `internal/reproduce/execute.go`
      (+ `execute_test.go`).
      Depends on: B1.
      Note: W2-beta added Docker-SDK pre-pull with registry/`--image` guidance,
      single-shot local execution via an injected `internal/localrun` adapter,
      explicit exit codes 0/1/2 through the command layer, parsed marker output,
      and integration coverage for dry-run JSON, run-mode JSON, and missing-run
      exit 2.

### Stream C — Output-diff compare, fidelity summary, shell mode (P1)

The debug-loop ergonomics layered on the core: compare reproduced output against
what prod recorded, print the honest fidelity summary derived from the design's
faithful-vs-best-effort table, and drop into a shell inside the exact
environment. C extends `cmd/reproduce/` and `internal/reproduce/`, so it
sequences **after** Stream B.

- [ ] C1. Add `--diff` output-compare as a **shared** package
      (`internal/outputdiff/`) so the recorded-vs-reproduced `##caesium::output` map
      comparison is built once and reused by the N-run
      [backtesting](backtesting.md) sibling (design Interplay note: "build it once as
      a shared package"). On success with a mismatch, exit `3`; on a match, `0`.
      Wire the `--diff` flag through `cmd/reproduce/` to compare parsed markers
      against the recorded `TaskRun.Output` from the descriptor wrapper.
      Files: new `internal/outputdiff/outputdiff.go` (+ `outputdiff_test.go`),
      `cmd/reproduce/reproduce.go`.
      Depends on: B2.
- [ ] C2. Add the per-run fidelity summary block derived from the design's
      faithful-vs-best-effort table: emit explicit warnings (never silence) for a
      DEGRADED mutable-tag pull, unmounted output-refs / dangling BYO-storage paths,
      engine & workload-identity dimensions with no local equivalent
      (`ServiceAccountName`, node selector, Kueue queue — listed, not applied),
      cross-arch emulation on a platform mismatch, and the not-reproduced dimensions
      (wall clock, external system state, side effects). Print it to stderr in
      human mode and include it in the `--json` document.
      Files: `internal/reproduce/reconstruct.go`, `internal/reproduce/execute.go`,
      `cmd/reproduce/reproduce.go`.
      Depends on: B2.
- [ ] C3. Add `--shell` interactive mode: same fetch/pull/env reconstruction, but
      `docker run -it --entrypoint <shell>` inside the exact environment instead of
      the recorded command. Distroless images without a shell fail here with a clear
      guidance error (run mode still works); the `--shell-image` busybox:1.36.1 sidecar
      fallback (design Open Question #4) is **out of scope** — recorded as deferred.
      Files: `cmd/reproduce/reproduce.go`, `internal/reproduce/execute.go`.
      Depends on: B2.

### Stream D — Fix-testing + local secret resolution (P2)

The last two modes from the design's phasing. Both extend `cmd/reproduce/` and
`internal/reproduce/`, so D sequences **after** Stream B (and coordinates with C
on the shared command file — see conflicts).

- [ ] D1. Add `--image ref` fix-testing mode: override the image (e.g. a locally
      built candidate fix) while keeping the recorded env, params, and predecessor
      outputs — "does my patch produce the right output *given the exact inputs that
      broke prod*?". Prominently mark the run **OVERRIDDEN** in the output line and
      the `--json` document so it is never mistaken for a faithful reproduction.
      Files: `cmd/reproduce/reproduce.go`, `internal/reproduce/execute.go`.
      Depends on: B2.
- [ ] D2. Add `--resolve-secrets`: resolve `secret://` refs via the operator's
      **local** `secret.Resolver` config (never fetched from the server), and emit a
      best-effort drift warning by comparing the recorded ref string + provider
      identity fields (Vault version / k8s resourceVersion) when the local provider
      matches — the server-keyed HMAC in `SecretRefs.Identity` is deliberately not
      client-verifiable. Default stays omit-and-warn; this flag is the explicit
      opt-in described in the design's Security section.
      Files: `cmd/reproduce/reproduce.go`, `internal/reproduce/reconstruct.go`.
      Depends on: B2.

#### Deferred — recorded, not in scope

- Design Open Question #1 (distinguish a **pruned** descriptor from one that
  **predates descriptors** for better operator messaging) — deferred until a run-
  retention pruner lands (`task_runs` has no pruner today, `pkg/env/env.go`). A1
  returns the generic "descriptor unavailable" → exit 2 for both cases meanwhile.
- Design Open Question #4 (`--shell-image` busybox:1.36.1 sidecar for distroless) —
  deferred; C3 fails distroless `--shell` with guidance instead.
- Design Open Question #5 (return the redacted `HashInputBlob` so the CLI prints an
  inline mini-`why`) — deferred as scope creep past the debug loop; `caesium why`
  already answers "which field differs".

## Harness Strengthening

- [x] H-1. Ensure the integration server produces reproducible descriptors and the
      reproduce scenarios drive the **real CLI** against the harness Docker daemon:
      confirm the integration job runs with digest pinning on (so
      `ResolvedImageDigest` is recorded and pull-by-digest is faithful), that the
      harness Docker daemon is reachable for the local-execution scenarios (design
      Testing scenario 3), and that the CLI is driven via `runCLIStdout` /
      `runCLISeparate` (stdout captured **separately** from stderr — never the
      stream-merging `runCLIRaw`) so the `--json`/`--dry-run` clean-stdout assertion
      is real. Reproduce needs no new `CAESIUM_*` server env (it reuses
      `CAESIUM_API_KEY`); if any harness gate is missing, add it here.
      Files: `justfile`, `.github/workflows/ci.yml`, `test/` harness helpers.
      Note: W1-η found digest pinning was the one missing gate —
      `CAESIUM_CACHE_PIN_DIGESTS` (default false) was set on NO server-boot site, so
      descriptors recorded no `ResolvedImageDigest`. Now set `=true` on every site in
      one sweep (all justfile lanes incl. podman/distributed/agent, the local
      k8s-distributed helm `--set` list as `extraEnv[3]`, the CI helm values file,
      and the three ci.yml server blocks) — degradation-safe because the imagecheck
      resolver falls back to the tag on registry failure. Verified already-present:
      the harness Docker daemon is reachable from the test-runner container (both
      mount `docker.sock`), and `runCLIStdout`/`runCLISeparate` split-stream helpers
      exist in `test/` (used by blame/agent-remediation scenarios). No new helper
      code needed; A2/B scenarios consume the existing surface.

## Navigational / Organizational Improvements

- [ ] N-1. Flip the [`docs/design-reproduce.md`](../../design-reproduce.md)
      `> Status:` banner from "Brainstorm/Design" to shipped/active and point it at
      this plan; update the `docs/roadmap.md` Phase 4 "Data-Plane Differentiators"
      table `caesium reproduce` row (line ~228) to add the plan link
      `exec-plans/active/reproduce.md` and reflect the shipped status; repoint the
      `docs/README.md` design-index bullet (line ~40, currently "(proposed)") to the
      plan and flip its status; add a `caesium reproduce` CLI reference (flags, exit
      codes, the faithful-vs-best-effort fidelity contract) to the CLI/operator
      docs; and cross-link the two consuming siblings —
      [`design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) (the
      escalation repro one-liner) and [`design-backtesting.md`](../../design-backtesting.md)
      (the shared `internal/outputdiff` compare primitive from C1). Keep the
      `docs/README.md` reference in backtick/inline-code form
      (`TestDocsREADMEIndexesEveryTopLevelDoc` rejects clickable subdirectory links).
      Runs last, after A–D ship, so the docs reflect reality.
      Files: `docs/design-reproduce.md`, `docs/roadmap.md`, `docs/README.md`,
      CLI/operator reference docs.
      Depends on: A–D (runs last).

## Sequencing & Dependencies

**Cross-stream order:**

- **Stream A is the foundation** — B fetches from A's endpoint; nothing downstream
  runs without it. A merges first (it is also the only server-side change, so its
  blast radius on `bind.go` clears before the CLI streams touch anything).
- **Stream B depends on A1** (B1 fetches the descriptor). B1 → B2 within the stream.
- **Streams C and D both depend on B2** and both extend the *same* `cmd/reproduce/`
  and `internal/reproduce/` files — sequence **B → C → D**, not parallel (see
  conflicts).
- **H-1** is independent (justfile/CI/test harness) and supports the A/B integration
  scenarios; land it in the first wave so the CLI's end-to-end gate has a live,
  digest-pinning surface and a reachable Docker daemon to drive.
- **N-1** runs last, after A–D ship, so the design banner, roadmap row, README, and
  CLI reference reflect reality.

**Suggested waves:**

- **W1 = A (A1 → A2) + H-1.** A is the server foundation; H-1 is independent harness
  work. Both leaf-eligible.
- **W2 = B (B1 → B2).** Unblocked once A1's endpoint is in.
- **W3 = C (C1, C2, C3) then D (D1, D2), then N-1 last.** C and D share the reproduce
  command/package files, so run C before D (or serialize their PRs); N-1 closes out.

**Within-stream order:** A1 → A2. B1 → B2 (B2's execution needs B1's reconstruction).
C1/C2/C3 each depend only on B2 (independent of each other, but all touch
`cmd/reproduce/reproduce.go` — serialize their edits). D1/D2 likewise depend on B2
and touch the same command file.

**Cross-stream file conflicts:**

- `cmd/reproduce/reproduce.go` — B1 creates it; B2, C1, C2, C3, D1, D2 all extend
  it. This is a **true-conflict file**: sequence B → C → D and serialize the item
  PRs within C and D rather than dispatching them into the same wave in parallel.
- `internal/reproduce/reconstruct.go` — B1 creates it; C2, D2 extend it. Sequence
  after B1.
- `internal/reproduce/execute.go` — B2 creates it; C1(via caller), C2, C3, D1 extend
  it. Sequence after B2.
- `api/rest/bind/bind.go` — **only A1** adds a route line (additive, `Protected()`
  `/v1/jobs/:id` group). No other stream touches it.
- `cmd/execute.go` — **only B1** appends `reproduce.Cmd` to the `cmds` slice
  (additive; rebases mechanically).
- **No `go.mod`/`go.sum` change** expected (reuses existing Docker/localrun/task
  packages) — if a stream adds a dependency, flag the `go.sum` conflict for
  `go mod tidy` at merge, not a hand-merge.
- **No `internal/models/models.go`** change (no new model), **no `pkg/env/env.go`**
  change (no new `CAESIUM_*`), **no `internal/metrics/metrics.go`** change (client
  feature, no metric moves), **no `pkg/jobdef/definition.go` / `internal/cache/hash.go`**
  change (read-only, no YAML-contract change). These usual shared-file hotspots are
  intentionally untouched — flag any item that reaches for them as scope creep
  against the Source-Of-Truth Note.
- `internal/outputdiff/` (C1) is a **new shared package** consumed by the
  [backtesting](backtesting.md) sibling plan; C1 owns its creation, backtesting
  cross-links rather than duplicating the item.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream additions:

- **New REST endpoint (A):** an integration scenario in `test/` that drives the
  **real surface** — fetch `GET /v1/jobs/:id/runs/:run_id/tasks/:task/descriptor`
  against the live server, assert the envelope round-trips, and assert the
  scoped-key 200/403 lanes and the absent-descriptor error. Not a unit test on the
  handler.
- **New CLI verb (B, C, D):** an integration scenario that drives the reproduce
  **binary** via `s.runCLIStdout` / `s.runCLISeparate` (stdout captured SEPARATELY
  from stderr) and asserts observed output: `--dry-run --json` stdout is clean,
  parseable JSON whose reconstructed env exactly equals the recorded envelope
  (literals + `CAESIUM_PARAM_*` + `CAESIUM_OUTPUT_*`, secret refs listed as
  omitted); a full local execution exits `0` and, with `--diff`, matches recorded
  output; a mutated `--set` run exits `3` on deliberate mismatch. A unit test that
  hand-builds an envelope proves the reconstruction, not the wiring — both are
  required.
- **Shared output-diff primitive (C1):** unit-tested in `internal/outputdiff/` AND
  exercised end-to-end through the `--diff` exit-3 integration lane.
- **No new metric / model / schema:** confirm no accidental edit to
  `internal/metrics/metrics.go`, `internal/models/models.go`,
  `pkg/jobdef/definition.go`, or `internal/cache/hash.go` slipped into the PR
  (this feature touches none of them).
- **This plan's checkbox ticked**, the active-wave `## Progress` bullet appended,
  and any cross-linked doc (design banner/roadmap/README) refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — the descriptor endpoint** is a live read-only feature:
   `GET /v1/jobs/:id/runs/:run_id/tasks/:task/descriptor` resolves the task by name
   (or UUID), returns the stored descriptor + wrapper verbatim, and is covered by
   the existing scoped-key auth arm. Closed by `test/reproduce_endpoint_test.go`:
   envelope round-trip + scoped 200/403 + absent-descriptor error, green in CI.
2. **Stream B — the CLI core** works end-to-end: `caesium reproduce … --dry-run
   --json` prints a clean, parseable envelope whose reconstructed env exactly equals
   the recorded literals + `CAESIUM_PARAM_*` + `CAESIUM_OUTPUT_*` (secrets omitted),
   and run mode pulls by recorded digest, executes locally, and returns the correct
   exit code. Closed by integration scenarios (dry-run envelope equality + full
   local execution exit 0) driven via `runCLIStdout`, green in CI.
3. **Stream C — diff, fidelity, and shell** ship: `--diff` compares reproduced
   vs recorded `##caesium::output` and exits `3` on mismatch (via the shared
   `internal/outputdiff` package), the fidelity summary warns on every best-effort
   dimension, and `--shell` drops into the exact env (distroless fails with
   guidance). Closed by a mutated `--set` exit-3 integration scenario + fidelity
   assertions, green in CI.
4. **Stream D — fix-testing + secret resolution** ship: `--image` runs a candidate
   image against the recorded inputs and marks the run OVERRIDDEN; `--resolve-secrets`
   resolves refs from the operator's **local** providers with a best-effort drift
   warning, default staying omit-and-warn. Closed by integration/unit coverage for
   both modes.
5. **H-1 — the integration server** records digest-pinned descriptors and the
   reproduce scenarios drive the real CLI against the harness Docker daemon with
   stdout captured separately from stderr, so the A/B scenarios run against the live
   binary in CI, not an internal call.
6. **N-1 — docs reflect reality:** the `docs/design-reproduce.md` `> Status:` banner
   is flipped and points at this plan, the `docs/roadmap.md` Phase 4 `caesium
   reproduce` row links the plan, the `docs/README.md` design-index bullet is
   repointed (backtick form), the CLI reference documents the flags/exit-codes/
   fidelity contract, and the agent-in-the-loop and backtesting siblings are
   cross-linked.
7. **Cross-cutting:** `docs/roadmap.md`, `docs/design-reproduce.md`, and this plan's
   per-stream `## Progress` entries reflect every shipped stream and match the merged
   PRs. The three deferred design Open Questions (#1 pruned-descriptor messaging, #4
   `--shell-image` fallback, #5 explain integration) remain explicitly recorded as
   deferred, not silently pulled in.

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line is satisfied
   (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the active wave
   subsection in `## Progress` (or open a new wave subsection if none exists yet),
   and update any cross-linked design doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (reproduce <wave>-<stream>)` — e.g.
   `Add the descriptor endpoint (reproduce W1-α)`. GitHub appends `(#NNN)` on
   squash-merge.

## Cross-References

- [`docs/design-reproduce.md`](../../design-reproduce.md) — the design of record.
  Source of truth for intent, scope, the fidelity contract, exit codes, and the
  security posture.
- [`docs/roadmap.md`](../../roadmap.md) Phase 4 "Data-Plane Differentiators" — the
  `caesium reproduce` entry this plan promotes from design to shipped.
- [`docs/design-quarantined-replay.md`](../../design-quarantined-replay.md) — the
  server-side ancestor; reproduce reuses its descriptor decode/validation and
  env/hash reconstruction (`internal/replay/replay.go`) and inherits the
  never-store-secret-values invariant, but discards the quarantine machinery.
- [`design-backtesting.md`](../../design-backtesting.md) /
  [`backtesting.md`](backtesting.md) — the N-run sibling that consumes the shared
  `internal/outputdiff` compare primitive built in C1.
- [`design-agent-in-the-loop.md`](../../design-agent-in-the-loop.md) /
  [`agent-in-the-loop-remediation.md`](../completed/agent-in-the-loop-remediation.md) — every
  diagnosed page appends a `caesium reproduce … --diff` one-liner.
- `internal/models/run.go` (`TaskExecutionDescriptor`), `internal/run/store.go`
  (`TaskExecutionDescriptor` loader), `pkg/task/output.go` (`ParseMarkers`,
  `BuildOutputEnv`), `internal/localrun/` (the local runner), `cmd/why/why.go`
  (API-key hygiene pattern), `api/rest/controller/receipt/` (the endpoint pattern) —
  the shipped substrate this plan builds on.

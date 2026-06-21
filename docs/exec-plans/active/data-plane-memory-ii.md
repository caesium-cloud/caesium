# Data-Plane Memory II — The Causal Query Layer

Last updated: 2026-06-20

This plan ships the three higher-order EXPLAIN verbs that the
[data-plane-memory](../completed/data-plane-memory.md) substrate plan deferred to
a follow-on. That plan's `#### Deferred to a follow-on feature plan` note named
them explicitly — *"Causal `caesium run diff`, the quarantined what-if
`replay --set … --diff`, and `caesium blame` over commit ranges … Draft a
follow-on once A–D land"* — and streams A–D have now shipped (#213–#222: image
digest pinning, persisted decomposed `HashInput`, `caesium why`, reproducibility
receipt + `verify`, append-only `dag_snapshot` topology history, populated
OpenLineage datasets + cross-job impact, large-object reference passing, and a
value-verified short-circuit). The substrate those three verbs consume now
exists, so this plan turns it into operator-facing query surfaces.

Each verb sits on a substrate that already shipped, so the work is read-side
assembly + one runtime mode, not new persistence:

- **`caesium run diff` (causal)** reuses the persisted decomposed `HashInput`
  blobs and the field-by-field differ already built for `caesium why`
  (`internal/run/whydiff.go` → `DiffHashInputBlobs`/`BlobDiff`). The design notes
  this is *the same computation as `why`* — because the hash already contains
  predecessor outputs, "what data changed" and "why did this task re-run" collapse
  into one diff.
- **Quarantined what-if replay (`caesium run replay --set … --diff`)** re-runs a
  baseline run under overridden inputs, re-executing only hash-changed tasks while
  unchanged tasks resolve as provably-identical cache hits, in an isolated run
  whose results never become cache- or lineage-authoritative. This is the one
  net-new runtime mode and the only genuinely under-specified verb, so its stream
  opens with a short design memo.
- **`caesium blame` over commit ranges** walks the append-only `dag_snapshot`
  history and attributes each task/edge to the provenance commit (+ author/ref)
  that introduced it — version-aware EXPLAIN without `git checkout`.

**Strategic frame.** This is the *Retain* layer of
[`differentiation-strategy.md`](../../differentiation-strategy.md) — the
differentiating axis the strategy protects, not the "why-not-Airflow" roadmap.
It deliberately stays honestly scoped: `run diff` does **cache-bust attribution**
(which step/output changed and why a task re-ran), and hands full row/column
value diffs to dbt/Datafold; replay **re-executes identical code** against pinned
digests and does **not** resurrect overwritten source data.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work
backlog, `## Sequencing & Dependencies` captures cross-stream order,
and `## Acceptance Criteria` lists the gates that close out the entire
plan. Any agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies
   are satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every
   PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).
For drafting new plans in this same shape, see
[`.claude/skills/draft-exec-plan/`](../../../.claude/skills/draft-exec-plan/).

## Project Posture

From [`differentiation-strategy.md`](../../differentiation-strategy.md): the
data-plane memory is the **second act** — *"the killer differentiator within
sovereignty … what makes Caesium more than 'Argo with a nicer binary,' once a
user is already inside."* These three verbs are retention hooks no other
zero-dependency scheduler has. Two strategy guardrails are load-bearing for
scope and must survive into the shipped surfaces:

- **Do not over-claim data causality.** We attribute *which step/output changed
  and why a task re-ran* — not full dataset value diffs. Every surface that could
  read as "we diff your data" must point the user at dbt/Datafold for value-level
  comparison.
- **Replay never resurrects data.** It re-executes identical code against the
  same typed inputs + pinned digests. A receipt/replay over an unpinned tag stays
  honestly degraded (the A4 correctness rule), never silently attested.
- **Replay re-executes real container commands — it is not a simulation.**
  "Quarantine" isolates a replay run's *cache and lineage authority*; it does
  **not** sandbox the container, so a re-executed task still runs its real command
  and can write to external systems, deploy, notify, or delete. The word
  "what-if" must never imply side-effect-free. Stream B is therefore designed
  **fail-closed** (per the adversarial review, 2026-06-20): replay defaults to
  callbacks/notifications off, fails rather than silently re-running a task whose
  baseline result is unavailable, and gates side-effecting replay behind explicit
  opt-in (a replay-safe job/step annotation and/or an alternate env namespace).
  The quarantine invariant is **non-bypassable** — there is no API/CLI knob to
  produce an authoritative what-if run. See item B1's design memo.
- **Every new authenticated REST route ships its RBAC policy entry.** Caesium's
  auth middleware (`api/middleware/auth.go`) denies any authenticated route absent
  from `endpointPolicy` (`internal/auth/rbac.go`) as `unknown_route` (403). A
  handler that passes its no-auth integration test but lacks a policy entry is
  broken for every API-key/SSO deployment — the sovereignty buyer's default.
  Read-only `run diff`/`blame` map to `RoleViewer`; `replay` maps to `RoleRunner`
  (matching `run`/`retry`). Item H-1 also backfills the **shipped** data-plane
  endpoints, which this review found are missing from the policy.

## Source-Of-Truth Note

When this plan and [`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md)
disagree, the **design doc wins** — it is the design-of-record for the
data-plane-memory feature family and carries the "What each feature needs"
substrate table and the honest-scope rules these verbs must honor. The scope
deferral that spawned this plan lives in
[`../completed/data-plane-memory.md`](../completed/data-plane-memory.md)
(`#### Deferred to a follow-on feature plan`); that tracking is now owned here.
Stream B additionally defers to its own design memo
(`docs/design-quarantined-replay.md`, authored by item B1) for replay quarantine
semantics once it lands.

## Progress (as of 2026-06-20)

No implementation waves have shipped yet. The plan was published as the
pre-named follow-on to the completed data-plane-memory substrate plan (streams
A–D, #213–#222); the first wave is the next eligible run of the
`exec-plan-wave` skill against this doc. Leaf items eligible for Wave 1:
**A1**, **B1** (design memo), **C1**, **H-1** (independent RBAC backfill).

This plan was revised twice on 2026-06-20 in response to two Codex adversarial
review rounds. Round 1: Stream B is now fail-closed (non-bypassable quarantine,
callbacks-off, fail-on-missing-baseline, explicit opt-in for side-effecting
replay); every new REST route carries an RBAC policy entry. Round 2: Stream C's
blame is scoped to **commit/snapshot-level** attribution keyed by the full task
descriptor (matching what `DagSnapshot` actually persists — no historical
author/ref; same-name content mutations counted correctly); and the RBAC work was
split into **H-1** (a full, explicit backfill of all ~19 unpolicied `Protected()`
routes — the auth trust boundary) and **H-2** (the completeness guard, which lands
after H-1 so it goes green). The RBAC gap is a **latent** bug — the project is
pre-alpha with no users, so nothing is broken in production today.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Causal `caesium run diff` — read-side blob-diff across two runs | **P1** | Not started |
| B | Quarantined what-if replay (`caesium run replay --set … --diff`) — fail-closed | P2 | Not started |
| C | `caesium blame` — commit/snapshot attribution, descriptor-keyed | **P1** | Not started |
| H | Full RBAC policy backfill (H-1) + completeness guard (H-2) | P2 | Not started |
| N | Plan-level cross-links (roadmap §3.4, README, strategy doc) | — | Not started |

## Streams

### Stream A — Causal `caesium run diff`

Ship a read-side diff of two runs of the same job that attributes, per task, why
it re-ran (or why it would have cache-hit) by diffing the two persisted
decomposed `HashInput` blobs. This is the smallest stream and the highest
leverage-to-effort: the differ already exists for `caesium why`
(`internal/run/whydiff.go`), so Stream A is mostly endpoint + CLI assembly over a
substrate that shipped in data-plane-memory A2. It also unblocks Stream B's
`--diff` output. Honest scope, enforced in the rendered output: cache-bust
attribution only — hand value-level row/column diffs to dbt/Datafold.

- [ ] A1. Add the run-diff read-side core: given two run IDs of the same job,
      pair terminal (latest-attempt) task-runs by task name and diff each pair's
      persisted `HashInput` blob via `DiffHashInputBlobs`, assembling a `RunDiff`
      (per-task verdict + `[]FieldChange` + run-level param/trigger deltas and a
      tasks-added/removed set). Reuse the `whydiff.go` differ verbatim — do not
      fork it. Degrade gracefully when a task's blob is missing/oversized/version-
      mismatched (mirror `why`'s degraded handling). Emit a machine-readable
      struct.
      Files: new `internal/run/rundiff.go`, new `internal/run/rundiff_test.go`;
      reuses `internal/run/whydiff.go` (read-only).
- [ ] A2. Expose `GET /v1/jobs/:id/runs/diff?left=<run>&right=<run>` returning the
      `RunDiff` as JSON. Validate both runs belong to `:id`; 404 on unknown run,
      400 on missing query params. Register the route in `endpointPolicy`
      (`GET /v1/jobs/:id/runs/diff` → `RoleViewer`) or the middleware denies it
      `unknown_route` (403) under auth; add an RBAC assertion. Note the echo
      routing order: `/runs/diff` is a static segment that must register so it
      resolves ahead of the `/runs/:run_id` param route — verify the diff path is
      not shadowed.
      Files: new `api/rest/controller/rundiff/`, new `api/rest/service/rundiff/`,
      route + import in `api/rest/bind/bind.go`, `internal/auth/rbac.go`
      (policy entry) + `internal/auth/rbac_test.go` (RoleViewer assertion).
      Depends on: A1.
- [ ] A3. Add the `caesium run diff <left-run> <right-run> [--job-id <id>] [--json]`
      subcommand: human-readable per-task table by default, machine JSON with
      `--json`. Follow the `cmd/why/why.go` stdout discipline — all machine and
      table output via `fmt.Fprint*(cmd.OutOrStdout(), …)`, never cobra's
      `cmd.Print*` (which leaks to stderr and breaks `--json` piping).
      Files: new `cmd/run/diff.go` (its own `func init()` calling
      `Cmd.AddCommand(diffCmd)` on the existing `run.Cmd`).
      Depends on: A2.
- [ ] A4. Add an integration scenario driving `caesium run diff --json` end-to-end
      against the live server: two runs of a cacheable job differing by one run
      param, asserting the discriminating `runParams.*` field appears in the diff
      and that stdout is clean parseable JSON (captured via `runCLIStdout`, not the
      stream-merging `runCLIRaw`). Flip the design doc's `run diff` feature-table
      row from honest-scope-until-then to shipped.
      Files: `test/data_plane_e2e_test.go` (add `TestRunDiffAttributesChangedField`),
      `docs/design-data-plane-memory.md`.
      Depends on: A3.

### Stream B — Quarantined what-if replay

Ship `caesium run replay <run-id> --set k=v [--diff]`: re-run a baseline run
under overridden inputs in an **isolated** run that re-executes only hash-changed
tasks (unchanged tasks resolve as provably-identical cache hits) and whose
results never become cache- or lineage-authoritative. This is the only net-new
runtime mode in the plan and the one verb the substrate plan flagged as
"under-specified … needs its own design pass," so the stream opens with a focused
design memo before any runtime code. Replay reuses Stream A's diff for its
`--diff` output and the shipped value-verified short-circuit (data-plane-memory
Stream D) for unchanged-task reuse.

**Safety is the dominant design constraint here, not a footnote** (adversarial
review, 2026-06-20). Re-executing a hash-changed task runs its **real container
command** — quarantine isolates cache/lineage *authority*, it does not sandbox
the container, so a naive replay of a job that deploys, sends, or deletes will do
exactly that against production. Stream B is therefore **fail-closed end to
end**: (1) the quarantine invariant is non-bypassable — no request/CLI field can
produce an authoritative what-if run; (2) external callbacks/notifications are
**off by default** for replay runs; (3) replay **fails rather than silently
re-executes** a task whose baseline cache/result is unavailable (a pruned cache
entry must not turn a "what-if" into an unannounced production re-run); and (4)
re-executing tasks of a job not marked replay-safe requires explicit operator
opt-in. The B1 memo is the gate that makes these binding before any runtime code
lands.

- [ ] B1. Author the replay design memo, which must resolve the safety model
      **before** B2–B6 implement anything. Nail: (a) **quarantine isolation** — how
      a quarantined run reuses the cache for hash-unchanged tasks yet is excluded
      from becoming cache-authoritative and from authoritative lineage emission;
      (b) the `--set key=value` override plumbing into run params + `HashInput`
      (overrides must fold into the hash so changed tasks miss and unchanged tasks
      hit — the cache-correctness invariant); (c) the `--diff` comparison contract
      (replay-vs-baseline via Stream A); (d) the honest-scope boundary (no data
      resurrection; degraded over unpinned tags); and the **fail-closed safety
      contract** the review made load-bearing:
      - **Side-effect honesty.** State plainly that a re-executed task runs its
        real command; replay is not a sandbox. Decide and specify the containment
        mechanism — a `replaySafe`/`idempotent` opt-in on the job/step schema,
        and/or replay against an alternate env namespace (reusing the
        volumes/workload-identity BYO-env abstraction) — and what replay does when
        a job is **not** marked safe (default: refuse, or require an explicit
        `--force-side-effects` acknowledgement).
      - **Callbacks off by default.** Quarantined runs do not fire external
        callbacks/notifications unless explicitly re-enabled.
      - **Fail-closed on missing baseline.** If an "unchanged" task's baseline
        cache/result is pruned or absent, replay **fails** rather than silently
        re-executing it (an unannounced re-run could have side effects).
      - **Non-bypassable quarantine.** There is no field/flag to make a what-if
        run authoritative; non-quarantined re-execution is `caesium run retry`, a
        separate existing command.
      Carry a `> Status:` banner.
      Files: new `docs/design-quarantined-replay.md`, `docs/README.md` (index the
      new top-level doc — `internal/guardrails`'s
      `TestDocsREADMEIndexesEveryTopLevelDoc` requires every `docs/*.md` to be
      linked from the README; the `> Status:` banner satisfies
      `TestPlanningAndHistoricalDocsCarryStatusBanner`).
- [ ] B2. Add quarantine to the run model + executor: an additive `Quarantine bool`
      (and a captured override-params blob) on `JobRun`; the executor honors cache
      **reads** for unchanged tasks but skips cache-authoritative **writes**,
      authoritative lineage emission, **and external callbacks/notifications** when
      the run is quarantined. AutoMigrate picks up the additive column; no
      `hotTables` change (no new table). The flag is internal-only — set by the
      replay path (B3), never from a request body (see B4).
      Files: `internal/models/run.go` (`JobRun` struct, additive column),
      `internal/run/store.go`, `internal/job/job.go` (skip cache-write + lineage +
      callback dispatch when quarantined).
      Depends on: B1.
- [ ] B3. Implement the replay construction + dispatch path: from a baseline run +
      `--set` overrides, build a quarantined `JobRun` and dispatch it through the
      executor so only hash-changed tasks re-run and the rest are cache hits.
      **Fail-closed**: if a task is hash-unchanged but its baseline cache entry /
      result is unavailable (pruned/expired), abort the replay with a clear error
      rather than silently re-executing it; and refuse to re-execute tasks of a job
      not marked replay-safe unless the operator explicitly opted in (per B1).
      Re-execution is identical-code-against-pinned-digests — it does not resurrect
      overwritten source data.
      Files: new `internal/replay/` (the replay constructor over `run.Store` +
      dispatch; the fail-closed guards), `internal/run/store.go`.
      Depends on: B2.
- [ ] B4. Expose `POST /v1/jobs/:id/runs/:run_id/replay` (body: `{ "set": {k:v} }`
      only) returning the new run id. **Replay is always quarantined** — the body
      carries no `quarantine` field; the handler sets quarantine internally and a
      request attempting to disable it is rejected (the invariant cannot be turned
      off over the wire). Validate the baseline run belongs to `:id`. Register the
      route in `endpointPolicy` (`POST /v1/jobs/:id/runs/:id/replay` →
      `RoleRunner`, matching `run`/`retry`) or the middleware denies it
      `unknown_route` (403) under auth.
      Files: new `api/rest/controller/replay/`, new `api/rest/service/replay/`,
      route + import in `api/rest/bind/bind.go`, `internal/auth/rbac.go` (policy
      entry) + `internal/auth/rbac_test.go` (RoleRunner assertion).
      Depends on: B3.
- [ ] B5. Add `caesium run replay <run-id> --set k=v [--diff] [--json]`: fires a
      quarantined replay; with `--diff`, awaits the replay's terminal state and
      renders the run-diff vs the baseline by calling the Stream A endpoint.
      `cmd.OutOrStdout()` stdout discipline as in A3.
      Files: new `cmd/run/replay.go` (its own `func init()` calling
      `Cmd.AddCommand(replayCmd)` on `run.Cmd`).
      Depends on: B4 + A2 (the `--diff` path consumes the run-diff endpoint).
- [ ] B6. Add an integration scenario: replay a completed run with a changed
      `--set` param; assert (1) only the affected task re-ran while unchanged tasks
      report cache hits, (2) `--diff --json` reports the discriminating field on
      clean stdout (`runCLIStdout`), and (3) the quarantined run did **not** mutate
      the baseline's cache entry or authoritative lineage. Add **safety
      regression** assertions: (4) a `POST …/replay` body attempting `quarantine:
      false` (or any quarantine override) is rejected, not honored; (5) replay of a
      run whose unchanged task's cache entry has been pruned **fails closed** rather
      than re-executing; and (6) a quarantined run fires no external callbacks. Flip
      the design doc's `Quarantined what-if replay` feature-table row to shipped.
      Files: new `test/replay_test.go` (`//go:build integration`; reuses the
      existing suite helpers), `docs/design-data-plane-memory.md`.
      Depends on: B5.

### Stream C — `caesium blame` over commit ranges

Ship `caesium blame <job> [--task t] [--from <commit> --to <commit>]`: attribute
each task/edge in a job's DAG to the **commit/snapshot** that introduced its
current form, by walking the append-only `dag_snapshot` history shipped in
data-plane-memory Stream B. Independent of Streams A and B (different substrate:
topology history vs. hash blobs), so it runs fully in parallel.

**Scope honestly to what `DagSnapshot` actually persists** (adversarial review,
2026-06-20). The shipped snapshot stores `GitCommit` (the apply-time commit SHA,
possibly empty), a per-task descriptor `{name, image, command}`, and per-edge
`{from, to, provenance_commit}` — and the dedup `ContentHash` already folds in
each task's image+command. It does **not** persist historical **author** or
**ref**; those live only on the live `Job`/`Atom` rows and are overwritten on
every apply, so they are unrecoverable for past snapshots. Blame therefore
attributes to **commit + snapshot identity only**; surfacing author/ref is a
deferred enhancement requiring an additive `DagSnapshot` change (capture
author/ref at write time) and is recorded as out of scope here, not promised.

- [ ] C1. Add the blame query: walk `dag_snapshot` rows for a job in commit/time
      order and attribute each task/edge to its **most recent introduction** — the
      snapshot at which its *current descriptor* transitioned from absent → present
      — not the earliest-ever containment. **Key identity by the full descriptor,
      not the name**: a same-name task whose `image`/`command` changed is a content
      transition (the prior descriptor went absent, a new one appeared → a new
      snapshot with a new `ContentHash`), so blame must attribute it to the
      mutating snapshot, not the original introduction. This also handles the
      append-only churn cases: delete-and-readd is blamed on the re-adding commit,
      and a `[from, to]` range that begins after the original introduction is
      computed relative to the range. Surface the introducing snapshot's
      `GitCommit` and the per-edge `provenance_commit` (commit-only — no author/ref,
      per the scope note above). Support a single-task filter.
      Files: new `internal/blame/` (query package), new `internal/blame/*_test.go`
      (cover delete/readd, **same-name image/command mutation**, and
      range-start-after-introduction explicitly).
- [ ] C2. Expose `GET /v1/jobs/:id/blame[?task=<name>&from=<commit>&to=<commit>]`
      returning per-element attribution as JSON. Register the route in
      `endpointPolicy` (`GET /v1/jobs/:id/blame` → `RoleViewer`) or the middleware
      denies it `unknown_route` (403) under auth; add an RBAC assertion.
      Files: new `api/rest/controller/blame/`, new `api/rest/service/blame/`,
      route + import in `api/rest/bind/bind.go`, `internal/auth/rbac.go` (policy
      entry) + `internal/auth/rbac_test.go` (RoleViewer assertion).
      Depends on: C1.
- [ ] C3. Add the `caesium blame <job-id-or-alias> [--task t] [--from c] [--to c]
      [--json]` top-level command: per-element table by default, machine JSON with
      `--json` via `cmd.OutOrStdout()`.
      Files: new `cmd/blame/`, append `blame.Cmd` to the `cmds` slice in
      `cmd/execute.go`.
      Depends on: C2.
- [ ] C4. Add an integration scenario: apply a job, then apply a topology change
      (add an edge/step) that writes a second `dag_snapshot`, then drive
      `caesium blame --json` and assert the new element is attributed to the later
      snapshot/commit while the unchanged elements stay attributed to the first;
      assert clean stdout via `runCLIStdout`. Add two regressions for C1's
      descriptor-keyed attribution: (a) a **same-name image/command mutation** —
      change a step's image (fifth snapshot) and assert blame attributes it to the
      mutating snapshot, not the original; and (b) a **delete-and-readd** — remove
      an edge/step then re-add it, and assert blame attributes it to the re-adding
      snapshot.
      Files: new `test/blame_test.go` (`//go:build integration`).
      Depends on: C3.
      Note: whether the integration apply path stamps a distinct `GitCommit`
      (provenance is normally set by git-sync) is an open harness question — see
      `## Sequencing & Dependencies`. If apply cannot stamp a commit, the assertion
      falls back to per-snapshot attribution (snapshot id/`created_at`) rather than
      commit SHA — which is fine, since snapshot identity is the attribution key.

## Harness Strengthening

> Urgency note: the project is **pre-alpha with no users**, so the RBAC gap below
> is a **latent correctness bug**, not an active outage — these endpoints only
> 403 when auth is enabled, and no real auth-gated deployment exists yet. It is
> worth fixing now because the policy map is the auth trust boundary and the fix
> is cheap, but it does not warrant emergency sequencing.

- [ ] H-1. **Full RBAC policy backfill** for every currently-unpolicied route in
      `bind.go`'s `Protected()` group, with the trust boundary made explicit. The
      adversarial review (2026-06-20) found that `api/middleware/auth.go`'s
      `RequiredRole` returns `!ok` for any route absent from `endpointPolicy` and
      the middleware then denies it `unknown_route` (403) under auth — and that
      **~19 Protected routes are unpolicied today**, well beyond the data-plane
      ones. Add an entry for each, with these proposed minimum roles (confirm with
      the maintainer; consistent with the existing Viewer=read / Operator=manage
      semantics):
      - Data-plane reads → `RoleViewer`: `GET …/runs/:id/why`, `GET …/runs/:id/receipt`,
        `POST …/runs/:id/receipt/verify` (read-only re-derivation), `GET …/topology`,
        `GET …/topology/history`, `GET /v1/lineage/impact`.
      - Other reads → `RoleViewer`: `GET /v1/stats/summary`, `GET /v1/system/features`,
        `GET /v1/system/nodes`, `GET /v1/notifications/channels`(+`/:id`),
        `GET /v1/notifications/policies`(+`/:id`), `POST /v1/jobdefs/lint`,
        `POST /v1/jobdefs/diff` (both read-only previews — no state mutation).
      - Notification management → `RoleOperator`: `POST/PATCH/DELETE
        /v1/notifications/channels` and `…/policies`.
      `POST /hooks/*` is **out of scope** — it is registered by `bindWebhooks` on
      the outer group (its own webhook HMAC auth), not inside `Protected()`, so it
      is intentionally not RBAC-gated. Add `RequiredRole` assertions for the new
      entries.
      Files: `internal/auth/rbac.go` (backfill entries), `internal/auth/rbac_test.go`
      (assertions).
- [ ] H-2. Add the **completeness guard**: a test asserting every route registered
      under `bind.go`'s `Protected()` group has an `endpointPolicy` entry, so a new
      authenticated route without a policy fails CI instead of 403-ing at runtime.
      This is what makes A2/B4/C2's entries non-optional. It must land **after**
      H-1 (or it goes red against the existing gap) and must exclude the outer-group
      public routes (`/hooks/*`, anything in `skipPaths`).
      Files: new `internal/auth/rbac_policy_completeness_test.go` (enumerate the
      `Protected()` routes and diff against `endpointPolicy`), plus a `test/`
      integration assertion that an authenticated viewer can reach the data-plane
      reads (`why`/`receipt`/`topology`/`impact`).
      Depends on: H-1.
      Note: H-1+H-2 fix shipped code and are independent of A/B/C; given no users
      this is low-urgency, but they can land as a small standalone PR whenever
      convenient — see the cover note on PR #229.

## Navigational / Organizational Improvements

- [ ] N-1. Cross-link the shipped trio at the plan level: record the causal
      `run diff` / quarantined `replay` / `blame` reimagining under
      `docs/roadmap.md` §3.4 (Live DAG Debugging — already flagged "partially
      shipped via data-plane-memory"; extend it to mark the causal half shipped),
      add this plan to the `docs/README.md` active-records index, and note the
      Retain-layer progress in `docs/differentiation-strategy.md`. Concentrating
      all roadmap/README/strategy edits here keeps Streams A/B/C from colliding on
      those shared docs.
      Files: `docs/roadmap.md`, `docs/README.md`, `docs/differentiation-strategy.md`.
      Depends on: A4 + B6 + C4 (runs last, once all three verbs have shipped).

## Sequencing & Dependencies

**Cross-stream order.**

- Streams **A**, **B**, **C**, and **H** are independent at their cores and may
  all start in Wave 1 (leaf items A1, B1, C1, H-1).
- **B5 depends on A2** — the replay `--diff` output reuses the run-diff endpoint.
  A must reach A2 (the endpoint) before B5 (the CLI) lands; B1–B4 do not depend
  on A.
- **B1 gates the rest of Stream B.** The design memo resolves the fail-closed
  safety model; B2–B6 must not implement runtime replay before it lands. Treat B1
  as a hard barrier within Stream B, not a parallel doc item.
- **Stream H is independent** of A/B/C (it fixes shipped RBAC) and can land as a
  small standalone PR ahead of the wave. **H-2 depends on H-1**: the completeness
  guard must land only after the full backfill, or it fails CI against the
  existing gap. Once H-2 is in, it makes A2/B4/C2's policy entries non-optional —
  so ideally land H before those endpoint items, or accept that an endpoint item
  landing first will trip the guard (intended).
- **N-1 depends on A4 + B6 + C4** — the plan-level cross-links run last, after all
  three verbs ship.

**Within-stream order.**

- A: `A1 → A2 → A3 → A4` (strictly linear; each layer wraps the prior).
- B: `B1 (design memo, hard barrier) → B2 → B3 → B4 → B5 → B6`; B5 additionally
  needs A2.
- C: `C1 → C2 → C3 → C4` (strictly linear).
- H: `H-1 (full backfill) → H-2 (completeness guard)`.

**Cross-stream file conflicts.**

- `api/rest/bind/bind.go` — A2, B4, and C2 each add a route line + a controller
  import. Additive but the import block is rebase-prone; if two land in the same
  wave, sequence (A2 → B4 → C2) or expect a one-line mechanical rebase, not a
  semantic merge.
- `internal/auth/rbac.go` — H-1 (the bulk backfill) plus A2, B4, C2 each add
  entries to the `endpointPolicy` map. Map-literal appends on different lines,
  parallel-safe like the other slice/map appends; co-scheduled additions rebase
  mechanically. Once H-2's guard is in, any endpoint item missing its entry fails
  CI (intended).
- `internal/auth/rbac_test.go` — A2, B4, C2, H-1 each add an assertion; additive,
  parallel-safe (distinct lines).
- `cmd/run/` package — A3 (`diff.go`) and B5 (`replay.go`) add **separate new
  files**, each with its own `init()` calling `AddCommand` on the existing
  `run.Cmd`; parallel-safe (no shared-line edit). `cmd/execute.go`'s `cmds` slice
  is touched **only** by C3 (top-level `blame.Cmd`) — no contention.
- `docs/design-data-plane-memory.md` — A4 and B6 each flip a different
  feature-table row; different lines, mechanically rebaseable if co-scheduled.
- `internal/run/store.go` — B2 and B3 both touch it but are sequential within
  Stream B, so no cross-stream conflict.
- `internal/models/run.go` — only B2 (additive `JobRun` column). No new table, so
  no `models.All` / `hotTables` ordering concern.
- `go.sum` — no item adds a dependency; no `go mod tidy` conflict expected.
- `test/` — A4, B6, C4 land in **separate files** (`data_plane_e2e_test.go`,
  `replay_test.go`, `blame_test.go`) on the same suite type; Go permits methods on
  one suite across files, so they reuse the shared helpers without conflict.

**Open harness question (flag for the wave that picks up C4).** The integration
apply path may not stamp a distinct `GitCommit` the way git-sync does. If it
cannot, C4 attributes by snapshot identity/`created_at` rather than commit SHA —
which is acceptable, since snapshot identity (not the commit) is blame's
attribution key; the commit is a display detail. No author/ref is involved
(`DagSnapshot` does not persist them). Confirm the apply path's commit stamping
before implementing C4 rather than fabricating a provenance-threading item now.

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream conditional gates:

- **Every stream ships an integration scenario** (A4, B6, C4) — a new
  `cmd/`/REST surface with no `test/` scenario driving it through the real surface
  must block review (CLAUDE.md "End-to-end coverage is the gate"). `just unit-test`
  does **not** compile `test/` (it is behind `//go:build integration`), so a green
  unit-test is necessary but not sufficient; the integration gate is the
  end-to-end signal. Run golangci-lint with the integration tag, since the local
  `just lint` (no tag) does not catch issues in `//go:build integration` files.
- **Machine-readable CLI output** (`run diff --json`, `replay --diff --json`,
  `blame --json`) must be asserted clean on **stdout captured separately from
  stderr** via `runCLIStdout` — never the stream-merging `runCLIRaw`. Cobra
  `cmd.Print*` and log lines both leak to the wrong stream; a merged capture hides
  it.
- **B (quarantine, cache-touching, side-effecting):** add unit tests asserting a
  quarantined run does not write a cache-authoritative entry, that `--set`
  overrides change the task-identity hash (so changed tasks miss, unchanged tasks
  hit), that a request cannot disable quarantine, that replay fails closed when an
  unchanged task's baseline cache is absent, and that quarantined runs fire no
  callbacks. The replay path exercises the docker tier by default; no new engine
  tier is required.
- **RBAC (every new authenticated route — A2, B4, C2 — plus Stream H):** each new
  route has an `endpointPolicy` entry with a `RequiredRole` assertion; H-1
  backfills all currently-unpolicied `Protected()` routes; and once H-2's
  completeness guard is in, CI fails if any `Protected()` route lacks a policy
  entry (the guard excludes outer-group public routes like `/hooks/*` and
  `skipPaths`). Verify with `go test ./internal/auth/...` plus an integration
  assertion that an authenticated principal of the intended role can reach the
  endpoint (viewer for diff/blame/why/receipt/topology/impact; runner for replay).
- This plan's checkbox ticked, the active-wave `## Progress` bullet appended, and
  any cross-linked doc refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — causal `run diff`** is a runtime feature: `GET
   /v1/jobs/:id/runs/diff` returns per-task `HashInput`-blob attribution, the
   `caesium run diff --json` subcommand emits clean parseable stdout, and
   `test/data_plane_e2e_test.go`'s `TestRunDiffAttributesChangedField` is green in
   CI. The design doc's `run diff` feature-table row reads shipped.
2. **Stream B — quarantined replay** is a runtime feature that is **fail-closed**:
   the replay design memo (`docs/design-quarantined-replay.md`) has landed,
   `caesium run replay --set … --diff` re-runs only hash-changed tasks in an
   isolated run that leaves the baseline's cache/lineage authority untouched, and
   `test/replay_test.go` asserts in CI both the field-level diff on clean stdout
   **and** the safety invariants — quarantine cannot be disabled over the wire,
   replay fails closed when a baseline cache entry is missing, and a quarantined
   run fires no callbacks. The design doc's `Quarantined what-if replay`
   feature-table row reads shipped.
3. **Stream C — `caesium blame`** is a runtime feature scoped to the substrate:
   `GET /v1/jobs/:id/blame` attributes each task/edge to the **commit/snapshot**
   that introduced its current descriptor (keyed by `name+image+command`, not name
   alone; no historical author/ref, which `DagSnapshot` does not persist), the
   `caesium blame --json` command emits clean stdout, and `test/blame_test.go`
   asserts in CI an ordinary addition **plus** the two descriptor-keyed regressions
   — a same-name image/command mutation and a delete-and-readd, each attributed to
   the mutating/re-adding snapshot, not the original.
4. **Stream H — RBAC** is closed: H-1 has backfilled `endpointPolicy` for **all**
   previously-unpolicied `Protected()` routes (data-plane + stats/summary +
   system/* + notifications/* + jobdefs/lint+diff) with the roles in the H-1
   inventory, the new routes (A2/B4/C2) have entries, H-2's completeness guard
   (excluding `/hooks/*` and `skipPaths`) is green, and an authenticated viewer can
   reach the data-plane read endpoints in an integration test.
5. **Plan-level cross-links (N-1)** reflect the shipped trio: `docs/roadmap.md`
   §3.4 records the causal reimagining as shipped, `docs/README.md` indexes this
   plan, and `docs/differentiation-strategy.md` notes the Retain-layer progress.
6. **Cross-cutting**: `docs/roadmap.md` and the
   [data-plane-memory](../completed/data-plane-memory.md) sibling plan reflect
   every shipped stream; this plan's per-stream `## Progress` entries match merged
   PRs; and on full completion this plan is a candidate for archive to
   `docs/exec-plans/completed/`.

## How To Pick Up Work

1. Read this file end-to-end so you understand the streams, their
   interdependencies, and which acceptance criterion the item closes.
2. Pick an unchecked item under `## Streams` whose `Depends on:` line
   is satisfied (consult `## Sequencing & Dependencies`).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every
   PR)`.
5. Tick the checkbox for your item, add a per-stream bullet to the
   active wave subsection in `## Progress` (or open a new wave
   subsection if none exists yet), and update any cross-linked design
   doc / roadmap section in the same PR.
6. Open the PR with title format
   `<Imperative subject> (data-plane-memory-ii <wave>-<stream>)` —
   e.g. `Add causal run-diff read-side (data-plane-memory-ii W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

- [`docs/design-data-plane-memory.md`](../../design-data-plane-memory.md) — the
  design-of-record and source of truth; carries the "What each feature needs"
  substrate table and the honest-scope rules these verbs honor.
- [`../completed/data-plane-memory.md`](../completed/data-plane-memory.md) — the
  substrate plan (streams A–D) this builds on; its `#### Deferred to a follow-on
  feature plan` note named these three verbs.
- [`docs/differentiation-strategy.md`](../../differentiation-strategy.md) — why
  the data-plane memory is the Retain layer (second act), and the do-not-overclaim
  guardrails.
- [`docs/roadmap.md`](../../roadmap.md) — §3.4 Live DAG Debugging, reimagined here
  as *causal* (run diff / blame) rather than a visual state-viewer.
- [`docs/design-quarantined-replay.md`](../../design-quarantined-replay.md) — the
  replay quarantine-semantics design memo authored by item B1 (created when B1
  lands).
- `internal/run/whydiff.go` — the field-by-field `HashInput`-blob differ Stream A
  reuses.
- `internal/models/dag_snapshot.go` — the topology-history model Stream C reads.

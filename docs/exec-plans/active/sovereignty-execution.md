# Sovereignty Execution — Operationalize the Positioning Pivot

Last updated: 2026-06-19

This plan operationalizes the positioning pivot recorded in
[`docs/differentiation-strategy.md`](../../differentiation-strategy.md): **lead
with operational sovereignty.** It covers the two execution items that doc names
*beyond* the data-plane-memory substrate — (A) repositioning the project's
public surface to lead with sovereignty, and (B) delegating scheduling to Kueue
instead of building a toy priority field. The data-plane memory "second act" is a
separate, larger build tracked in
[`data-plane-memory.md`](data-plane-memory.md).

This is intentionally a **lean, docs-weighted plan**: the strategy doc's
highest-leverage move ("reposition first") needs no code, and the only net-new
runtime work is the Kueue passthrough. The do-not-build list and kill-conditions
in the strategy doc are guidance and a watch-list, not work, and are not items
here.

This plan follows the `exec-plan-wave` skill's structural convention:
`## Progress` is a wave-by-wave dashboard, `## Streams` is the work
backlog, `## Sequencing & Dependencies` captures cross-stream order,
and `## Acceptance Criteria` lists the gates that close out the entire
plan. Any agent can:

1. Pick a numbered checklist item from `## Streams` whose dependencies
   are satisfied (per `## Sequencing & Dependencies`).
2. Land it as a self-contained PR.
3. Run the verification block under `## Verification (Run For Every PR)`.
4. Tick the checkbox and update the active wave's per-stream bullet in
   `## Progress`.

For wave orchestration of the streams below, see
[`.claude/skills/exec-plan-wave/`](../../../.claude/skills/exec-plan-wave/).

## Project Posture

From [`docs/differentiation-strategy.md`](../../differentiation-strategy.md):
**sovereignty leads** (it sells by *constraint* — "you literally cannot run
Dagster here" — not by *comparison*, which a marketing-less project can't win),
**DX-over-k8s hooks**, and **data-plane memory is the second act** (separate
plan). Enforced here:

- **Delegate scheduling to Kueue; never bin-pack.** A shallow Caesium "priority
  field" reads as a toy and competes with Kueue on its home axis. This plan adds
  a passthrough label, not a scheduler.
- **Do not chase the do-not-build list** (generic priority/quotas/GPU/gang
  scheduling, connector breadth, out-UI-ing Kestra). Those are recorded as
  rejected in the strategy doc, not as items here.

## Source-Of-Truth Note

When this plan and [`docs/differentiation-strategy.md`](../../differentiation-strategy.md)
disagree, the **strategy doc wins**. The Kueue passthrough's YAML contract (B1)
additionally defers to `pkg/jobdef/definition.go`. The data-plane memory work is
owned by the sibling plan [`data-plane-memory.md`](data-plane-memory.md); where
the two plans touch the same files, the sibling's Stream A owns
`internal/cache/hash.go` and `pkg/jobdef/definition.go` (see Sequencing).

## Progress (as of 2026-06-20)

### Wave 0 — Reposition (this PR)

- **Stream A**: A1 first pass landed — README hero + intro + a new "Why Caesium"
  section now lead with sovereignty (zero-dependency single binary, "runs where
  Airflow/Dagster/Flyte can't", "free what they paywall") using search-term-aware
  copy. Iterate as positioning sharpens.

### Wave 1 — Proof-point docs (sovereignty-execution-alpha)

- **Stream A**: A2 landed — `docs/sovereignty.md` adds the free-vs-paywalled
  comparison table (HA/RBAC/SSO/audit/k8s/lineage vs. Dagster+/Kestra
  Enterprise/Prefect Cloud) and the zero-dependency / air-gapped quickstart
  (`scp` one binary story, no Postgres). Index entry added to `docs/README.md`
  (guardrail green). Air-gapped k8s notes subsection added to
  `docs/kubernetes-deployment.md` with cross-link to `cache.pinDigests` in
  `design-data-plane-memory.md` (not duplicated). Leaf item remaining: **B1**.

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
| A | Sovereignty positioning — reposition the public surface; proof-point docs | **P0** | A1 landed; **A2 landed** |
| B | Kueue delegation — emit `kueue.x-k8s.io/queue-name`, never bin-pack | P1 | Not started |

## Streams

### Stream A — Sovereignty positioning

The strategy doc's highest-leverage, no-code move: make the public surface lead
with the one asset competitors structurally cannot copy (the zero-dependency
single binary). Sells by constraint; needs no benchmark or sales motion.

- [x] A1. Reposition the README + project landing to lead with sovereignty:
      zero-dependency single binary, "runs where Airflow/Dagster/Flyte can't",
      "everything they paywall, free", in words a frustrated engineer searches
      for. (First pass landed in this PR; refine as positioning sharpens.)
      Files: `README.md`.
- [x] A2. Add sovereignty proof-point docs: a "free vs. paywalled" comparison
      (HA/RBAC/SSO/audit/k8s vs. Dagster+/Kestra/Prefect Cloud) and a
      zero-dependency / air-gapped quickstart ("`scp` one binary, no Postgres").
      Files: new `docs/sovereignty.md` (or `docs/comparison.md`) **plus its index
      entry in `docs/README.md`** (required — the `TestDocsREADMEIndexesEveryTopLevelDoc`
      guardrail tracks every top-level `docs/*.md`), `docs/kubernetes-deployment.md`
      (air-gap notes). Air-gapped image-digest resolution is owned by
      `data-plane-memory.md` Stream A (A1) — cross-link, do not duplicate.

### Stream B — Kueue delegation

Answer "why not Kueue?" by *using* Kueue for admission instead of reimplementing
it. The data DAG inherits Kueue's quota/fair-share/preemption for free.

- [ ] B1. Add a Kueue passthrough: a job/step field that creates the Kubernetes
      Job **suspended** with the `kueue.x-k8s.io/queue-name` label so Kueue gates
      admission (un-suspends on quota). The field is **excluded from the cache
      identity hash** — it is scheduling metadata, not execution input (treat it
      like secrets / workload-identity, which are already excluded). Add a unit
      test asserting the hash is identical with and without it.
      Files: `pkg/jobdef/definition.go` (+ `pkg/jobdef/schema.go`),
      `internal/jobdef/runtime/spec.go`, `internal/atom/kubernetes/engine.go`
      (suspend + label), `internal/cache/hash.go` (assert exclusion + test),
      `docs/caesium-job-llm-reference.md` + `docs/job-schema-reference.md` +
      `docs/kubernetes-deployment.md`, `docs/examples-k8s/*.job.yaml`.

## Sequencing & Dependencies

**Cross-stream order.** Streams A and B are independent. A1 (landed), A2, and B1
are all leaf items — Wave 1 can run A2 and B1 in parallel.

**Cross-plan file conflicts.** B1 touches `internal/cache/hash.go` and
`pkg/jobdef/definition.go`, which `data-plane-memory.md` **Stream A (A1/A2)**
rewrites heavily (both are true-conflict surfaces). Coordinate: sequence B1
*after* data-plane-memory Stream A merges, or expect a manual rebase. Do not run
B1 in the same wave as data-plane-memory A1/A2.

**Doc index.** A2 adds a new top-level `docs/*.md`; it **must** be added to
`docs/README.md` in the same PR or `TestDocsREADMEIndexesEveryTopLevelDoc` fails.
(Note: exec-plan files under `docs/exec-plans/` are *not* indexed in
`docs/README.md` — that guardrail only tracks top-level `docs/*.md`, and a subdir
link would itself fail the test.)

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

Per-stream conditional gates:

- **B1 (job-schema + k8s engine):** `caesium job lint --path docs/examples-k8s/`,
  `just helm-lint && just helm-template`, and the kind-based k8s integration path
  (`just k8s-distributed` + `just helm-test`) to confirm Kueue admission. Add a
  unit test asserting the queue field does **not** change the cache hash.
- **A1 / A2 (docs only):** the doc guardrail tests
  (`go test ./internal/guardrails/`) must stay green; A2 must keep
  `docs/README.md` in sync.
- This plan's checkbox ticked, the active-wave `## Progress` bullet appended, and
  any cross-linked doc refreshed in the same PR.

## Acceptance Criteria

The plan is done when **all** of these hold:

1. **Stream A — sovereignty positioning**: the README/landing leads with
   sovereignty (zero-dependency single binary, "runs where the heavy
   orchestrators can't", "free what they paywall") in search-term-aware copy; the
   "free vs. paywalled" comparison + air-gapped quickstart doc lands and is
   indexed in `docs/README.md` (doc guardrails green).
2. **Stream B — Kueue delegation**: a Kubernetes job declaring a queue emits the
   `kueue.x-k8s.io/queue-name` label and is admitted by Kueue (k8s integration /
   helm path green), the field is excluded from the cache identity hash (unit
   test asserts an identical hash with and without it), and the job-schema docs +
   a `docs/examples-k8s/` sample are updated.
3. **Cross-cutting**: `docs/differentiation-strategy.md` and `docs/roadmap.md`
   reflect the shipped items; this plan's per-stream `## Progress` entries match
   merged PRs.

## How To Pick Up Work

1. Read this file end-to-end, plus `docs/differentiation-strategy.md` for the
   positioning rationale.
2. Pick an unchecked item under `## Streams` whose `Depends on:` /
   cross-plan-coordination constraints are satisfied. Leaf items: A2, B1
   (run B1 after data-plane-memory Stream A merges).
3. Branch from `master` (or land in a worktree if dispatched by
   `exec-plan-wave`); do the work as a self-contained PR.
4. Run the verification block under `## Verification (Run For Every PR)`.
5. Tick the checkbox, add a per-stream bullet to the active wave subsection in
   `## Progress`, and refresh any cross-linked doc in the same PR.
6. Open the PR with title format
   `<Imperative subject> (sovereignty-execution <wave>-<stream>)`.

## Cross-References

- [`docs/differentiation-strategy.md`](../../differentiation-strategy.md) — the
  positioning thesis and source of truth for this plan.
- [`data-plane-memory.md`](data-plane-memory.md) — the sibling plan for the
  "second act" substrate; owns `internal/cache/hash.go` /
  `pkg/jobdef/definition.go` where the two overlap.
- [`docs/roadmap.md`](../../roadmap.md) — Kueue delegation supersedes the
  "Priority Queues" (1.4) roadmap item; never bin-pack.
- `README.md` — the public surface repositioned by Stream A.
- `pkg/jobdef/definition.go` — the job-definition schema (the Kueue field in B1
  defers to it).

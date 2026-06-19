---
name: draft-exec-plan
description: Draft a new execution plan in the canonical exec-plan-wave shape so the resulting doc can be picked up end-to-end by `exec-plan-wave` without structural surprises. Reads the user's problem statement, enumerates the work, groups it into parallelizable streams (Greek-letter wave suffix per stream), establishes per-item file paths and dependency edges, fills in the standard sections (`## Progress`, `## Streams`, `## Sequencing & Dependencies`, `## Verification`, `## Acceptance Criteria`, `## How To Pick Up Work`), and saves the plan to `docs/exec-plans/active/<slug>.md`. Use when the user asks to "draft a plan", "write up an exec plan for X", "scope out a plan for Y", or invokes this skill by name.
---

# Draft Execution Plan

Companion to [`exec-plan-wave`](../exec-plan-wave/). Where `exec-plan-wave`
*executes* one wave of an existing plan, this skill *creates* the plan doc
in the shape `exec-plan-wave` knows how to consume.

This is the **caesium** adaptation: a Go distributed job scheduler (GORM over
dqlite/SQLite, echo HTTP API, Cobra CLI, embedded React/Vite UI, Prometheus
metrics, Docker/Podman/Kubernetes runtimes). The verify chain, source-of-truth
files, and shared-file conflict rules below are caesium-specific.

## When to use

- A new initiative needs a plan doc (e.g. "draft a plan for the event-trigger
  fan-out work").
- An existing initiative has scope but no skill-compatible plan
  (the user asks "rewrite this in the exec-plan-wave shape").
- A wave-orchestration run failed with "plan structure deviates from the
  standard pattern" — fixing the doc with this skill is the unblock.

## Inputs

- **Initiative name + problem statement** (required). The user describes
  what work the plan covers, what shipping it changes, and the rough
  surface area. Ask one focused clarifying question if the surface area
  is genuinely ambiguous; otherwise infer from the description and
  surrounding repo state.
- **Reference docs** (optional but high-leverage): existing design docs
  (`docs/design-*.md`), superpowers specs
  (`docs/superpowers/specs/<date>-<topic>-design.md`), the roadmap
  (`docs/roadmap.md`), related plans, source files. Pull these up early —
  they shape the streams.
- **Target file path** (optional). Default: `docs/exec-plans/active/<slug>.md`
  where `<slug>` is the kebab-case form of the initiative name.

## High-level flow

```
┌────────────────────────────────────────────────────────────────────┐
│ 1  GATHER     read problem; survey related docs/code; identify     │
│               existing source-of-truth files                       │
│ 2  ENUMERATE  list every concrete piece of work as a candidate     │
│               item (description + files + intent)                  │
│ 3  GROUP      partition items into streams (one stream = one       │
│               coherent area of code/docs); apply the divide-rules  │
│ 4  ITEM-SHAPE normalize each item: short description, file paths,  │
│               Depends-on edges, success criterion                  │
│ 5  SEQUENCE   capture cross-stream dependencies and shared-file    │
│               conflicts                                            │
│ 6  CRITERIA   author 1-line acceptance criteria per stream + plan- │
│               level cross-cutting gates                            │
│ 7  ASSEMBLE   fill in plan-template.md and save to                 │
│               docs/exec-plans/active/                              │
│ 8  CROSS-LINK update roadmap / sibling plans / design docs to      │
│               point at the new plan                                │
└────────────────────────────────────────────────────────────────────┘
```

Each phase has detailed instructions in [`PLAYBOOK.md`](PLAYBOOK.md). Read
it once at phase 1 — the rubrics for stream decomposition, item
granularity, and acceptance-criteria authoring are load-bearing decisions
you make autonomously.

The output template is [`plan-template.md`](plan-template.md). Fill in
every `<<placeholder>>` and remove every `<!-- guidance comment -->`
before saving.

## Repo-specific conventions

Bake these into the plan you produce (do not deviate):

- **File location**: `docs/exec-plans/active/<slug>.md`. When the plan
  lands, the orchestrator (or a finalize commit) moves it to
  `docs/exec-plans/completed/` per `exec-plan-wave/PLAYBOOK.md` Phase 8b.
- **Stream identifiers**: single capital letter (A, B, C, …). Avoid
  Phase-N labels in stream names — they conflict with the per-wave
  Greek-letter suffix.
- **Wave suffix**: `<plan-slug> <wave>-<stream>`, where wave is W1, W2,
  … and stream is α, β, γ, δ, ε, ζ, η. Used in PR titles by
  `exec-plan-wave` Phase 3 dispatch.
- **Item identifiers**: `<stream-letter><number>`, e.g. `A1`, `H-3`.
  Optional `H-` prefix for "Harness Strengthening" items (test infra,
  golangci/lint, justfile, CI), `N-` for "Navigational / Organizational"
  items (docs, roadmap, README, runbooks). Do not introduce new prefixes.
- **Source-Of-Truth Note**: every plan declares one. Common forms:
  - "When this plan and `docs/roadmap.md` disagree, the roadmap wins."
  - "When this plan and `pkg/jobdef/definition.go` disagree, the schema
    wins." (job-definition / YAML-contract plans)
  - "When this plan and `docs/superpowers/specs/<spec>.md` disagree, the
    spec wins."
  - "When this plan and `<sibling-plan>.md` disagree, the sibling wins."
- **Verify chain**: every plan ends with the canonical block:
  ```sh
  just lint              # go fmt + go vet + golangci-lint
  just unit-test         # go test -race -coverprofile ./...
  just integration-test  # go test ./test/ -tags=integration (real server)
  ```
  Plus per-stream conditional gates if they exist (`just ui-lint` +
  `just ui-test` + `just ui-e2e` for `ui/**`; `just helm-lint` +
  `just helm-template` for `helm/**`; `just integration-test-podman` for
  the podman engine). See `plan-template.md` for the full conditional list.

## Hard rules

- **Always produce the standard sections in the standard order** — see
  the template. `exec-plan-wave/PLAYBOOK.md` Phase 1 step 2 enumerates
  them; deviation triggers the skill's "stop and ask" escalation.
- **Never invent items** that aren't grounded in either the user's
  problem statement, the related docs you read, or the existing repo
  state. If a stream needs items but the user hasn't given enough scope
  detail, leave a single placeholder item asking the wave that picks up
  the stream to enumerate sub-items first.
- **Never produce a plan that violates the divide-rules** in
  [`PLAYBOOK.md`](PLAYBOOK.md) Phase 3 — those map directly to the
  collision-avoidance rubric `exec-plan-wave` uses at Phase 2b.
- **Never copy unchecked items between plans.** If a sibling plan
  already owns an item, mark this plan's reference as "owned by Stream
  X in `<sibling>.md`" rather than duplicating the checkbox.
- **Never delete content from an existing plan during a rewrite** —
  preserve all substantive technical content; only restructure section
  order, item shape, and missing skill sections. Historical Log /
  Coordination / Performance Targets content stays as trailing
  supplementary sections after the skill-relevant ones.

## Escalation paths (when to stop and ask)

- **The problem statement spans multiple unrelated initiatives**: stop
  and ask which one to draft (or whether to draft a meta-plan that
  cross-links several).
- **The problem area has no existing source-of-truth file** to anchor
  the Source-Of-Truth Note: ask the user where the contract lives
  (often: `docs/roadmap.md`, a design doc under `docs/`, a superpowers
  spec, or `pkg/jobdef/definition.go`).
- **A proposed stream has no obvious file ownership boundary** with
  another (e.g. two streams both rewrite `cmd/start/start.go` or
  `pkg/jobdef/definition.go`): ask the user whether to bundle into one
  stream or sequence sequentially.
- **The acceptance criteria can't be made concrete** because the
  initiative is exploratory: produce a "design-only" plan with one
  stream that ends in a design memo (`docs/design-<topic>.md` or a
  superpowers spec), and ask the user to confirm before saving.

Otherwise, run end-to-end without checking in.

## Output

When the plan is drafted and saved, post one summary message to the
user:

- **Path**: where the new plan lives
  (`docs/exec-plans/active/<slug>.md`).
- **Stream count + count of unchecked items**: e.g. "6 streams,
  34 unchecked items, 1 deferred".
- **Cross-links updated**: which sibling docs (`docs/roadmap.md`,
  related plans, design docs, superpowers specs) you edited to point at
  the new plan.
- **First-wave eligibility hint**: which items are leaf items
  (no unmet `Depends on:` edges) so the user can immediately invoke
  `exec-plan-wave` if they want.
- **Open questions**: anything you escalated or would like the user to
  confirm before the first wave runs.

That's the entire operator-facing output. No "let me know if you want me
to ..." — the plan is drafted or it's reported as stopped with a clear
reason.

## Pairing with exec-plan-wave

The two skills are designed to compose:

- `draft-exec-plan` produces a doc that satisfies
  `exec-plan-wave/PLAYBOOK.md` Phase 1's structural expectations.
- `exec-plan-wave` then runs waves against that doc, appending per-wave
  bullets to `## Progress`, ticking checkboxes in `## Streams`, and
  syncing the dashboard at Phase 8.
- When all `## Acceptance Criteria` are met, the wave's Phase 8b audit
  flags the plan as a candidate for archive to
  `docs/exec-plans/completed/`.

Do not invoke `exec-plan-wave` from inside this skill — leave that
choice to the user. The handoff is via the saved file.

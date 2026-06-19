# Draft Execution Plan Playbook

Detailed instructions for each phase of the `draft-exec-plan` skill. Read
this once at phase 1 start; the rubrics here drive autonomous decisions
across all later phases.

The reference for "what good looks like" in caesium is the wave/workstream
structure of `.claude/plans/golden-kindling-cherny.md` (parallel workstreams
with new/modified-files tables) blended with the header/exit-criteria
scaffolding of `docs/superpowers/plans/archive/2026-05-27-sso-foundation.md`
(note its `## Phase N exit criteria` headers). Skim one of each before
starting phase 1 — the structural pattern is more easily absorbed than
described.

---

## Phase 1: Gather

**Goal**: understand the initiative deeply enough to enumerate work
without inventing.

1. Re-read the user's problem statement. Note the verbs (what changes?)
   and the nouns (what files/systems/concepts?).
2. Run `git fetch origin && git status` — make sure you're on a clean
   `master` so the doc you produce ages well against the next merge.
3. Survey related repo state:
   - `ls docs/exec-plans/active/` and `docs/exec-plans/completed/` to
     find sibling plans that might already own part of the scope.
   - For each related sibling, read its `## Source-Of-Truth Note` and
     `## Streams` headers — you'll either depend on them or
     handoff-to-them.
   - `grep -ri <key-noun> docs/` to find existing design docs
     (`docs/design-*.md`), superpowers specs
     (`docs/superpowers/specs/`), and the roadmap section
     (`docs/roadmap.md`) that contain the technical contract.
   - `git show --stat 53cdb57` — the volumes/workload-identity feature
     (PR #207) is the canonical "what a full feature touches" reference
     (23 files across schema, runtime, all three engines, cache, docs).
4. Identify the **source of truth** for the plan's contract. Common
   candidates:
   - `docs/roadmap.md` (the strategic priority list; per-section
     `**Status**:` + `**Design doc**:` lines)
   - A design doc under `docs/design-<topic>.md` (each carries a
     `> Status:` banner)
   - A superpowers spec under `docs/superpowers/specs/<date>-<topic>-design.md`
   - `pkg/jobdef/definition.go` (the job-definition schema) for any plan
     that changes the YAML contract
   When in doubt, ask the user. The Source-Of-Truth Note in the final
   plan declares which file wins on disagreement.
5. List the existing wave history (if any) — sibling plans that recently
   shipped tell you what just landed and what's actively in flight; new
   plans should not collide with active work. Check recent commits
   (`git log --oneline -30`) and the two live initiatives (SSO under
   `docs/sso-authentication.md`; Volumes & workload-identity under
   `docs/superpowers/specs/2026-05-29-volumes-and-workload-identity-design.md`).

**Output of phase 1**: a one-paragraph summary of the initiative, the
source-of-truth file, and the names of every sibling plan or design doc
the new plan will cross-link.

---

## Phase 2: Enumerate

**Goal**: list every concrete piece of work the initiative needs, before
trying to organize them.

Strategy: brain-dump candidate items in a flat list, with no streams
yet. Each candidate should be:

- **Concrete**: nameable as a single PR or design memo.
- **Anchored**: at least one file path (or "new design doc:
  `docs/design-<x>.md`") that the work touches.
- **Self-contained**: a reader who only sees this item should know what
  to build.

Sources for candidate items:

- The user's problem statement (split compound asks into atoms).
- Existing design docs / superpowers specs (extract the "Open question"
  / "Remaining work" / "Non-goals → later" lists; each often becomes a
  stream item).
- Code grep for `// TODO` near the named files.
- Sibling plans' "Deferred" or "Carried forward" sections.
- **The standard checklist for any new caesium feature** (verified
  against the repo; tailor to the initiative):
  - **Implementation** — business logic under `internal/<feature>/`
    (store.go pattern: a struct holding `*gorm.DB` with typed CRUD).
    Background processors (`New...(deps).Start(ctx)`) are env-gated
    goroutines wired in `cmd/start/start.go`.
  - **Persistent state** — a GORM model in `internal/models/<name>.go`
    registered in the `All` slice in `internal/models/models.go` (ORDER
    matters: parents before FK children). NO hand-written SQL — schema
    is derived from struct tags via AutoMigrate. If the table is a hot
    per-run table, also add it to `hotPathModels()` in `pkg/db/db.go`
    AND the `hotTables` map in `pkg/db/router.go`.
  - **Operator surface** — CLI verb (Cobra: a `cmd/<group>/` package
    exporting `var Cmd`; new top-level group → append to the `cmds`
    slice in `cmd/execute.go`); REST route
    (`api/rest/controller/<f>/` + `api/rest/service/<f>/` + a route line
    in `Protected()` of `api/rest/bind/bind.go`); UI page
    (`ui/src/features/<f>/` + route in `ui/src/router.tsx` + a method in
    `ui/src/lib/api.ts`). GraphQL (`api/gql/`) is a live-but-placeholder
    endpoint (only a `place` query; mounted at `/gql` only when auth is
    off) — surface new features via REST, not GraphQL.
    UI-gated capabilities add a field to the `Features` struct in
    `api/rest/service/system/system.go`.
  - **Metrics** — a `caesium_*` collector declared in the `var (...)`
    block of `internal/metrics/metrics.go` AND added to the
    `prometheus.MustRegister(...)` list in `Register()` (two edit sites,
    same file), with a test using `internal/metrics/testutil`.
  - **Config** — a `CAESIUM_*` field on the single `Environment` struct
    in `pkg/env/env.go` (`envconfig:` tag + `default:`), plus a check in
    `validate()` if cross-field.
  - **Job-schema change** (if the YAML contract changes) —
    `pkg/jobdef/definition.go` (structs + `Validate()` + the dual
    `Step`/`rawStep` declaration + `UnmarshalYAML`), `pkg/jobdef/schema.go`,
    `internal/jobdef/runtime/spec.go`, the three engines
    `internal/atom/{docker,kubernetes,podman}/engine.go`,
    `internal/cache/hash.go` (cache key MUST include new fields), and the
    docs (`docs/caesium-job-llm-reference.md`, `docs/job-definitions.md`,
    `docs/job-schema-reference.md`) + `docs/examples/*.job.yaml`.
  - **Design doc** — `docs/design-<topic>.md` with a `> Status:` banner,
    or a superpowers spec for a larger design-of-record.
  - **Tests** — unit (`*_test.go` beside code, run by `just unit-test`)
    + integration scenario (`test/<feature>_test.go` behind
    `//go:build integration`, run by `just integration-test`).
  - **Cross-references updated** — `docs/roadmap.md` section status,
    `docs/README.md` index, sibling plans.

Do **not** filter for size, parallelizability, or stream membership
yet — that's phase 3.

**Output of phase 2**: a flat list of 5–50 candidate items, each with a
one-sentence description and at least one file path.

---

## Phase 3: Group into streams

**Goal**: partition the flat candidate list into streams so each stream
is a coherent body of work that one agent can own end-to-end.

### Step 3a: Build a file-ownership matrix

For each candidate item, list the files it touches. Look for **shared**
files where two items both want to write — those are the conflict
candidates.

Common shared files to watch for in caesium:

- `go.mod` / `go.sum` (any item that adds a dependency — `go.sum` always
  conflicts across two dep-adding streams)
- `pkg/jobdef/definition.go` (any item that changes the job schema — note
  the dual `Step`/`rawStep` declaration + shared `Validate()`)
- `internal/models/models.go` (any item that adds a table — order-sensitive
  `All` slice)
- `pkg/db/db.go` + `pkg/db/router.go` (any item adding a hot per-run table)
- `api/rest/bind/bind.go` (any item that adds a REST route — the import
  block is the conflict-prone part)
- `cmd/execute.go` (any item adding a new top-level CLI command group)
- `cmd/start/start.go` (any item adding a startup-wired background subsystem)
- `pkg/env/env.go` (any item adding config — the `validate()` func is shared)
- `internal/metrics/metrics.go` (any item adding a metric — two edit sites)
- `api/api.go` (any item that extends the `Start()` signature — true conflict)
- `ui/src/router.tsx`, `ui/src/lib/api.ts`, `ui/src/components/layout/Sidebar.tsx`
  (any UI page — list appends; import blocks conflict)
- `docs/roadmap.md`, `docs/README.md` (doc-sync items)

### Step 3b: Apply the divide-rules

The rules below mirror `exec-plan-wave/PLAYBOOK.md` Phase 2b — a plan
that follows them will dispatch cleanly under the wave skill.

| Rule | Action |
|---|---|
| Two candidates touch the same `.go` file (same struct/func) | Bundle into one stream |
| Two candidates append to the same slice/list at different points (`models.All`, `cmds`, route lines, metric vars, env fields, UI nav arrays) | OK in different streams (additive; rebases mechanically if on different lines) |
| Two candidates both add a dependency (`go.mod`/`go.sum`) | OK in different streams, but flag the `go.sum` conflict for Phase 5 (resolved by `go mod tidy`, not hand-merge) |
| Two candidates both edit `pkg/jobdef/definition.go` (`Step`/`Validate`) | Bundle into one stream OR sequence sequentially — the dual declaration makes this a true-conflict file |
| Two candidates both extend `api/api.go` `Start()` or `cmd/start/start.go` composition | Bundle into one stream OR sequence |
| Two candidates create new files in unrelated packages/dirs | OK in different streams |
| Two candidates touch `CLAUDE.md` / `AGENTS.md` | Assign to one stream only |
| Two candidates create new docs in different `.md` files | OK in parallel; pair related ones into one stream |
| One candidate is a "doc sync" that updates `docs/roadmap.md` / sibling plans | Make it the last item in the stream that ships the runtime change, not a separate stream |

### Step 3c: Name the streams

- **Single capital letter**: A, B, C, … through whatever you need.
- **Stream descriptions are 1-line, action-oriented**: "Stream A — Event-trigger
  evaluation engine", not "Stream A — Trigger-related work".
- **Consider precedence**: stream A is the one that touches the
  largest blast radius (so it merges first under
  `exec-plan-wave` Phase 7); stream Z is the one that touches the
  smallest.

### Step 3d: Optional sub-stream conventions

If the plan has cross-cutting test/CI or docs work, peel those into their
own dedicated streams:

- **Harness Strengthening** items (`H-1`, `H-2`, …) for
  test-infrastructure / golangci / justfile / CI / Dockerfile work.
- **Navigational / Organizational** items (`N-1`, `N-2`, …) for
  documentation, `docs/roadmap.md`, `docs/README.md`, runbook, doc-map
  work.

These show up as their own `## Harness Strengthening` and
`## Navigational / Organizational Improvements` sections in the final
plan, not as a Stream-A or Stream-B grouping.

### Step 3e: Skip rules — what NOT to put in the plan

Do **not** include:

- Items that are explicitly out of scope per a strategic decision
  (record those in a "Rejected" or "Deferred" subsection of the
  relevant stream).
- Items that depend on infra/access the team doesn't have — flag the
  whole stream "Blocked" so wave orchestration skips it.
- Items already covered by a sibling plan — cross-link instead.

**Output of phase 3**: a stream table with stream id, name, items,
and any "Rejected/Deferred/Blocked" tags.

---

## Phase 4: Item shape

**Goal**: normalize every candidate item into the canonical
`- [ ]` form so `exec-plan-wave` Phase 1 can ingest it.

### Required fields per item

1. **Identifier**: `<stream-letter><number>` (e.g. `A1`, `B7`),
   `H-<n>` for harness, `N-<n>` for nav. No leading zeros. Items renumber
   sequentially as items are added/removed during draft.
2. **One-line description** that says what shipping the item changes.
   Strong verb up front: "Add", "Extract", "Implement", "Wire",
   "Persist", "Document", "Surface".
3. **`Files:`** — at least one path. Conventions:
   - **Existing file**: `api/rest/bind/bind.go`.
   - **New file**: `new internal/eventtrigger/store.go`.
   - **Per-topic package under a directory**: `internal/eventtrigger/`
     (write the directory, then add a parenthetical hint — e.g. `(the new
     evaluation-engine package)`).
4. **`Depends on:` line** — only if a dependency exists, only listing
   in-plan item ids (e.g. `A1 + A2`). External-plan dependencies
   ("sso-foundation Stream B") are documented inline in the description,
   not as a `Depends on:` line.
5. **Checkbox state**: `[ ]` for unstarted, `[x]` for done. Default to
   `[ ]` for newly drafted plans.

### Item granularity

Aim for items that can each ship as one PR. Heuristics:

- **Too small**: "Rename `foo` to `bar` in `definition.go`." (Bundle into
  the parent feature item.)
- **Right size**: "Add the `EventTrigger` model + store, register it in
  `models.All`, and wire the evaluation loop in `cmd/start/start.go`
  behind `CAESIUM_EVENT_TRIGGERS_ENABLED`." (One stream item; one PR;
  touches several files.)
- **Too large**: "Implement event triggers end-to-end." (Decompose into
  A1–A8.)

If an item's description starts to read like a paragraph, split it.
If you're tempted to say "this includes 5 sub-tasks: …", split it
into 5 items.

### Long-form items are OK when warranted

Some items genuinely span multi-paragraph design discussion (e.g. a
schema change that must explain a compatibility trade-off). Keep these
long when the design rationale must travel with the item; trim them when
the rationale belongs in a separate design doc.

**Output of phase 4**: every item has id + description + Files +
optional Depends-on, checkbox unchecked unless explicitly noted.

---

## Phase 5: Sequence & dependencies

**Goal**: capture cross-stream order and shared-file conflicts in the
final `## Sequencing & Dependencies` section.

The section has three parts:

1. **Cross-stream order**: which streams block which.
   Format: bulleted list of "Stream X depends on Stream Y for item Z".
2. **Within-stream order**: the load-bearing dependency chains within
   each stream. Don't restate every item's `Depends on:` line — just
   the chains that matter for parallelism (e.g. "A1 → A2 → (A3, A4 in
   parallel) → A5").
3. **Cross-stream file conflicts**: shared files that two streams
   shouldn't write to in the same wave. Format: "Streams X, Y, Z all
   touch `<file>`; sequence (X → Y → Z) rather than parallel." Always
   call out `go.sum` if two streams add dependencies (the orchestrator
   resolves it with `go mod tidy`, not a hand-merge), and
   `pkg/jobdef/definition.go` / `internal/models/models.go` if two
   streams both edit those.

This is the section `exec-plan-wave/PLAYBOOK.md` Phase 2 reads to plan
parallelism. If the section is missing or wrong, dispatch produces
merge conflicts.

**Output of phase 5**: filled-in `## Sequencing & Dependencies`
section.

---

## Phase 6: Acceptance criteria

**Goal**: write the gates that close out the entire plan.

Each `## Acceptance Criteria` entry is one numbered bullet. Style:

- Lead with the **stream** it closes out: "**Stream A — event-trigger
  engine** is a runtime feature: …"
- Reference concrete artifacts: an integration scenario green in CI
  (`test/<feature>_test.go`), a metric family registered, a doc updated,
  a `docs/roadmap.md` section flipped to Shipped.
- Avoid pure-prose criteria like "the system is performant" — those
  can't be verified mechanically.

For plans with already-shipped streams, criteria for those streams say
"(already met by Wave N)" to acknowledge they're closed.

For deferred / rejected streams, criteria say "(explicitly recorded as
deferred)".

Plan-level cross-cutting criterion (always include): "`docs/roadmap.md`
and any sibling plan reflect every shipped stream; this plan's per-stream
Progress entries match merged PRs."

**Output of phase 6**: filled-in `## Acceptance Criteria` section
with N+1 bullets (N streams plus the cross-cutting criterion).

---

## Phase 7: Assemble + cross-link

**Goal**: produce the final `.md` file and update sibling docs that
should know about it.

### Step 7a: Fill in plan-template.md

Open [`plan-template.md`](plan-template.md) and substitute every
`<<placeholder>>`:

- `<<plan-title>>` — `Stream Subject — Tagline`
- `<<last-updated>>` — today's date in `YYYY-MM-DD`
- `<<intro-paragraphs>>` — 1–3 paragraphs of context
- `<<source-of-truth-note>>` — single paragraph
- `<<progress-intro>>` — usually "No implementation waves have shipped
  yet" for a fresh plan; for a rewrite of an existing plan, summarize
  the prior waves
- `<<stream-status-table>>` — markdown table
- `<<streams-content>>` — `## Streams` body with all items
- `<<sequencing>>` — `## Sequencing & Dependencies` body
- `<<acceptance-criteria>>` — `## Acceptance Criteria` body
- `<<cross-references>>` — bulleted list of related docs

Remove every `<!-- guidance comment -->` from the template before
saving.

### Step 7b: Save the plan

Save to `docs/exec-plans/active/<slug>.md` where `<slug>` is the
kebab-case form of the initiative name. Do not commit unless the user
asks — the user will commit when satisfied.

### Step 7c: Update sibling docs

Update at least:

- [`docs/roadmap.md`](../../../docs/roadmap.md): find the matching
  numbered initiative section (`### 1.1`, `### 1.2`, …) and update its
  `**Status**:` line + add/update a `**Design doc**:` (or plan) link back
  to the new plan. The roadmap tracks status per-section, not in a central
  table.
- Any sibling plan whose stream now hands off to or owns from the new
  plan: edit the relevant stream description to cross-link.
- Any design doc / superpowers spec the plan promotes: update its
  `> Status:` banner (e.g. "Status: Proposed → active — Stream X in
  `<new-plan>.md`").
- [`docs/README.md`](../../../docs/README.md): add a bullet under the
  active-records section if the plan is a durable record.

These edits are part of the same draft session; surface them in the
final output summary.

---

## Phase 8: Final output

**Goal**: hand the plan off to the user with everything they need to
either run a wave against it or hand back feedback.

Per `SKILL.md` § Output, post one summary message with:

1. **Path** to the saved plan.
2. **Stream count + count of unchecked items** (e.g. "6 streams,
   34 unchecked items, 1 deferred").
3. **Cross-links updated**: bullet list.
4. **First-wave eligibility hint**: which items are leaf items (no
   unmet `Depends on:` edges).
5. **Open questions**: anything you escalated.

End the turn. Do not invoke `exec-plan-wave` — the user decides when to
run the first wave.

---

## Common gotchas

- **The user gives a problem statement that's actually two
  initiatives.** Stop at phase 1; ask which one to draft. Don't
  silently merge them — the resulting streams won't have coherent
  acceptance criteria.
- **The user gives an initiative that's exploratory.** Produce a
  design-only plan with one stream that ends in a design memo
  (`docs/design-<topic>.md` or a superpowers spec). Don't fabricate
  concrete items below the design level.
- **A candidate item is "ship feature X end-to-end".** That's a stream,
  not an item. Decompose it.
- **Items with no `Files:` line.** Reject — every item must be
  anchored. If you genuinely don't know yet, leave the item as
  "design memo first" and let the design memo enumerate the file
  paths.
- **A job-schema item that forgets `internal/cache/hash.go`.** Any new
  field on a step/definition that affects execution MUST be hashed into
  the cache key, or cached results go stale silently. Include it.
- **A persistent-state item that forgets the `models.All` registration
  or hot-table router entry.** A new model that isn't in `models.All`
  never gets migrated; a hot per-run table not in `hotPathModels()` +
  `hotTables` breaks sharded routing. Make the registration explicit in
  the item's `Files:`.
- **Streams with one item only.** Sometimes correct, but more often a
  sign that the stream should be folded into a sibling.
- **Acceptance criteria that say "the design is good".** Reject —
  rewrite as "the design memo at `<path>` lands" or "the integration
  scenario `<name>` is green in CI".

## When something genuinely doesn't fit the playbook

Stop and ask. The user wrote `draft-exec-plan` to handle the typical
case end-to-end, but they'd rather you stop and say "X surprised me,
here's what I found" than push through with a fabricated plan.

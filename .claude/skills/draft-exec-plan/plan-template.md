<!--
  Plan template for draft-exec-plan (caesium).

  Substitute every <<placeholder>> with real content. Remove every
  <!-- guidance comment --> before saving.

  Section order is load-bearing: exec-plan-wave/PLAYBOOK.md Phase 1
  step 2 enumerates the headers it reads, in this order. Do not
  reorder.
-->

# <<plan-title>>

Last updated: <<YYYY-MM-DD>>

<<intro-paragraphs>>

<!--
  1-3 paragraphs explaining: what this plan ships, what it changes
  (current state vs. target state), and the rough surface area.
  Keep it tight — readers go to ## Streams for the work breakdown.
-->

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

<!--
  Optional: include a ## Project Posture or ## Strategic Decisions
  section here if the plan reflects scope/positioning calls that
  shape the streams. Skip if the streams stand on their own.
-->

## Source-Of-Truth Note

<<source-of-truth-note>>

<!--
  One paragraph. Pick the form that fits:
  - "When this plan and `docs/roadmap.md` disagree, the roadmap wins."
  - "When this plan and `pkg/jobdef/definition.go` disagree, the schema
    wins."  (job-definition / YAML-contract plans)
  - "When this plan and `docs/superpowers/specs/<spec>.md` disagree, the
    spec wins."
  - "When this plan and `<sibling-plan>.md` disagree, the sibling wins."
  - Plus: any per-stream cross-link rule (e.g. "Stream X is owned by
    sibling-plan; tracking continues there").
-->

## Progress (as of <<YYYY-MM-DD>>)

<<progress-intro>>

<!--
  For a fresh plan: "No implementation waves have shipped yet. The
  plan was published with the alignment from <event>; the first wave is
  the next eligible run of the exec-plan-wave skill against this doc."

  For a plan rewrite where prior waves shipped: per-wave subsection
  format below.
-->

<!--
  When a wave lands, append a ### Wave N — <Outcome> subsection here
  per the exec-plan-wave skill's Phase 8 convention, with one bullet
  per stream (PR link, merge SHA, review-resolve outcome).
-->

### Stream Status

| Stream | Scope | Priority | Status |
|--------|-------|----------|--------|
<<stream-status-table>>

<!--
  Example rows:
  | A | <one-line scope> | **P0** | Not started |
  | B | <one-line scope> | P1 | Not started |
  | C | <one-line scope> | P2 | Blocked |
  | D | <one-line scope> | — | **Rejected** — recorded decision |
-->

## Streams

<<streams-content>>

<!--
  One ### Stream X — <Name> per stream. Each stream has:

  1. A short description (1-2 paragraphs) explaining what shipping the
     stream does and why.
  2. A bulleted checklist of items, each formatted:

     - [ ] X1. <imperative description>.
           Files: <path1>, <path2>.
           Depends on: <other-item-ids> (only if applicable).

  Multi-line items are fine when the design rationale must travel with
  the item.

  Rejected/deferred sub-bullets within a stream go in a #### Rejected
  for <reason> sub-section.
-->

<!--
  Optional: ## Harness Strengthening section if the plan has H-N
  items (test infrastructure / golangci / justfile / CI / Dockerfile
  work).
-->

<!--
  Optional: ## Navigational / Organizational Improvements section if
  the plan has N-N items (docs / roadmap / README / runbook / doc-map
  work).
-->

## Sequencing & Dependencies

<<sequencing>>

<!--
  Three parts:

  1. **Cross-stream order**: bulleted list of "Stream X depends on
     Stream Y for item Z" or "Streams A, B are independent and can
     run in parallel".

  2. **Within-stream order**: load-bearing dependency chains within
     each stream (don't restate every Depends-on; just the chains
     that matter for parallelism).

  3. **Cross-stream file conflicts**: shared files two streams
     shouldn't write to in the same wave. Format: "Streams X, Y, Z
     all touch `<file>`; sequence (X → Y → Z) rather than parallel."
     Always call out `go.sum` (two dep-adding streams; resolved by
     `go mod tidy`, not hand-merge), `pkg/jobdef/definition.go`, and
     `internal/models/models.go` if multiple streams edit them.
-->

## Verification (Run For Every PR)

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # builds :latest-test, runs a real server, go test ./test/ -tags=integration
```

<!--
  Note: everything is containerized (the builder/builder-full images
  carry the dqlite CGO deps); host `go build`/`go test` is discouraged
  per CLAUDE.md. `just unit-test` does NOT compile test/ (it is behind
  //go:build integration), so a passing unit-test is necessary but not
  sufficient — the integration gate is the end-to-end signal.

  Add per-stream conditional gates the plan needs:

  - `ui/**` changes:        just ui-lint && just ui-test && just ui-e2e
  - `helm/**` / k8s engine:  just helm-lint && just helm-template
                             (+ kind-based k8s integration; locally
                              approximated by `just k8s-distributed` +
                              `just helm-test`)
  - podman engine adapter:   just integration-test-podman
  - job-schema change:       caesium job lint --path docs/examples/
  - new metric:              assert via internal/metrics/testutil in a
                             *_test.go (the metric must also be in
                             Register())
  - This plan's checkbox ticked, per-stream `## Progress` bullet
    appended for the active wave, and any cross-linked doc refreshed
    in the same PR.
-->

## Acceptance Criteria

The plan is done when **all** of these hold:

<<acceptance-criteria>>

<!--
  N+1 numbered bullets:
  - One per stream, leading with the stream name.
  - One cross-cutting bullet: "`docs/roadmap.md` and any sibling plan
    reflect every shipped stream; this plan's per-stream Progress
    entries match merged PRs."

  Style: reference concrete artifacts (integration scenario green in
  CI, metric family registered, doc updated, roadmap section flipped
  to Shipped). Avoid pure prose criteria.
-->

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
   `<Imperative subject> (<plan-slug> <wave>-<stream>)` —
   e.g. `Add event-trigger evaluation engine (event-triggers W1-α)`.
   GitHub appends `(#NNN)` on squash-merge.

## Cross-References

<<cross-references>>

<!--
  Bulleted list of related docs that this plan cross-links. Common
  entries:
  - docs/roadmap.md
  - sibling exec plans (docs/exec-plans/active/*.md)
  - design docs the plan promotes / consumes (docs/design-*.md)
  - superpowers specs (docs/superpowers/specs/*.md)
  - the job-definition schema (pkg/jobdef/definition.go) for YAML-contract plans
  - runbooks / operator docs the plan adds or extends
-->

<!--
  Optional trailing sections (preserve when rewriting an existing
  plan; skip on a fresh plan unless they're load-bearing):

  ## Coordination       (per-file editor responsibility, locking
                        rules)
  ## Blocked Work
  ## Performance Targets
  ## Log                (chronological history; usually unnecessary
                        because per-wave Progress entries serve the
                        same role)
-->

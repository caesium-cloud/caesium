# {{plan-title}}

Last updated: {{YYYY-MM-DD}}

{{problem-current-behavior-target-outcome-and-non-goals}}

## Source-Of-Truth Note

{{authoritative-contracts-precedence-and-proposed-contract-changes}}

## Progress (as of {{YYYY-MM-DD}})

{{prior-wave-evidence-or-no-implementation-waves-shipped}}

### Stream Status

| Stream | Scope | Priority | Status |
| --- | --- | --- | --- |
{{stream-status-rows}}

<!-- The orchestrator maintains wave status, PR links, tested head SHAs,
merge SHAs, evidence, and blockers here. Resume an unfinished wave before
allocating a new wave number. Workers report their entry to the orchestrator. -->

## Streams

{{stream-sections}}

<!-- Each stream uses ### Stream A — Name, a short purpose, then items:
- [ ] A1. Concrete result.
  Files: existing/path.go, new test/scenario_test.go.
  Depends on: none (or item IDs; name external prerequisites and evidence).
  Verify: observable behavior and the test proving it.
Keep deferred/optional/blocked items visibly labeled. Optional H-N and N-N
sections may follow Streams when they represent independent work. -->

## Sequencing & Dependencies

{{dependency-order-shared-file-owners-external-prerequisites-and-first-wave-candidates}}

## Verification (Run For Every PR)

{{scope-specific-verification-and-environment-prerequisites}}

<!-- For runtime changes include:
```sh
just lint
just unit-test
just integration-test
```
Add required conditional recipes from the current justfile and CI workflow.
Use containerized builds. New CLI commands and REST queries need real-surface
integration tests in test/. Parse machine output from separate stdout, and
enable any feature gate on the test server. For docs-only work state the
appropriate structural/link validation and any explicit repository checks.
Assign one owner for shared integration containers, image tags, and ports. -->

## Acceptance Criteria

{{numbered-observable-criteria-and-required-evidence}}

## How To Pick Up Work

1. Read this plan, its contracts, and the applicable `AGENTS.md`.
2. Select unchecked, included items whose dependencies are verified ready.
3. Use an assigned worktree and branch from the verified base. Keep changes
   within the stream's file ownership and include its tests and behavior docs.
4. Run the required verification on the actual candidate changes. Record what
   passed, failed, or could not run.
5. Update only assigned item checkboxes and notes. The wave orchestrator owns
   Progress and records completion from verified merged PRs.
6. Follow the requested publication endpoint. PR titles use
   `<Imperative subject> (<plan-slug> W<n>-<Greek stream suffix>)`.

Use `$exec-plan-wave` with this plan's path to orchestrate a wave in Codex.
Use `$draft-exec-plan` to revise the plan without starting implementation.

## Cross-References

{{links-relative-to-this-plan}}

---
name: exec-plan-wave
description: Orchestrate one full "wave" of a structured execution plan end-to-end with no operator intervention. Reads a plan doc (e.g. `docs/exec-plans/active/<slug>.md`), divides remaining checklist items into parallel streams, picks Opus/Sonnet per stream, spawns worktree-isolated sub-agents to do the work and raise PRs, dispatches review-resolution sub-agents to address GitHub review comments inline, diagnoses CI failures (flake vs real), runs a scope-aware integration gate, merges PRs into master resolving conflicts, and finalizes the plan-doc Progress dashboard. Use when the user asks to "run/execute/ship/orchestrate the next wave" of a plan, "implement the next wave with parallel agents", or invokes this skill by name with a plan-doc path.
---

# Execution Plan Wave Orchestration

End-to-end skill for running one full wave of an execution plan in the
**caesium** repo (a Go distributed job scheduler: GORM over dqlite/SQLite,
echo HTTP API, Cobra CLI, embedded React/Vite UI, Prometheus metrics,
Docker/Podman/Kubernetes runtimes). Designed for plans organized as
parallelizable streams (the `draft-exec-plan` pattern). Runs autonomously:
no operator intervention, no mid-flight confirmations.

**Companion skill:** [`draft-exec-plan`](../draft-exec-plan/) authors plan
docs in the shape this skill consumes. If a user asks to run a wave against
a plan whose structure deviates from the standard pattern (no `## Progress`
dashboard, no `## Streams` headers, no `## Acceptance Criteria` block,
items not in `- [ ]` form), the unblock is to draft or rewrite the plan
with `draft-exec-plan` first.

## Inputs

- **Plan doc path** (required) — e.g. `docs/exec-plans/active/event-triggers.md`. The user provides this; if they ask "the next wave" without naming a doc, default to the most recently modified file under `docs/exec-plans/active/` and confirm by reading the file's title to verify it matches the request.
- **Wave label** (optional) — e.g. `W3`. If absent, infer from the plan's `## Progress` section by adding 1 to the highest existing wave number.

## High-level flow

```
┌──────────────────────────────────────────────────────────────────────┐
│ 1   ANALYZE     read plan; enumerate unchecked items                 │
│ 2   DIVIDE      group into streams; map files; pick model per stream │
│ 3   SPAWN       launch background agents in worktrees (parallel)     │
│ 4   WAIT        monitor task notifications until all streams report  │
│ 4.5 PUBLISH     orchestrator-side commit + push + PR (codex only)    │
│ 5   REVIEW      fetch PR comments; dispatch sub-agents per PR        │
│ 6   CI          diagnose failures (flake vs real); rerun or fix      │
│ 6.5 GATE        scope-aware integration gate per gated PR            │
│ 7   MERGE       order PRs by conflict surface; merge sequentially    │
│ 7.5 CLEAN       prune merged-PR worktrees + stale stragglers         │
│ 8   FINALIZE    verification sweep; sync Progress dashboard          │
└──────────────────────────────────────────────────────────────────────┘
```

Each phase has detailed instructions in [`PLAYBOOK.md`](PLAYBOOK.md). Read it
once at the start of phase 1 — the rubrics there (model selection,
collision avoidance, conflict resolution patterns, flake vs real CI failure)
are load-bearing decisions you'll make autonomously.

Sub-agent prompts:
- [`stream-agent-prompt.md`](stream-agent-prompt.md) — for `general-purpose`
  (sonnet/opus) stream agents.
- [`stream-agent-prompt-codex.md`](stream-agent-prompt-codex.md) — for
  `codex:codex-rescue` stream agents. Skips the publish step (codex sandbox
  blocks network egress and can't run caesium's containerized verify chain);
  orchestrator publishes + verifies in Phase 4.5.
- [`review-agent-prompt.md`](review-agent-prompt.md) — review-resolve sub-agent
  template.

Substitute placeholders when dispatching.

## Repo-specific conventions

These are baked into the skill (do not deviate):

- **Repo + branch**: the working tree is the caesium checkout root —
  written `$REPO_ROOT` below; resolve it with `git rev-parse --show-toplevel`
  (do not hardcode a machine-specific path). Remote `caesium-cloud/caesium`,
  default branch **`master`**. Rely on
  `isolation: "worktree"` for **general-purpose** stream agents; the
  auto-generated branch (`worktree-agent-<id>`) is the PR branch. For
  review-resolve agents, reuse the existing worktree at
  `$REPO_ROOT/.claude/worktrees/agent-<id>` (do not spawn
  fresh worktrees because the branch is checked out there). **Codex streams
  in a parallel wave are the exception**: do NOT use `isolation: "worktree"`
  for them — the forwarder agent's worktree gets auto-cleaned out from under
  the detached codex job (see `PLAYBOOK.md` Phase 3 § codex
  "Worktree-auto-clean race"). The orchestrator creates
  `.claude/worktrees/agent-<wave>-<stream>` itself (branch
  `worktree-agent-<wave>-<stream>`) and dispatches `codex-companion task
  --background` directly into it.
- **PR title format**: `<Imperative subject> (<plan-slug> <wave>-<stream>)` —
  e.g. `Add event-trigger evaluation engine (event-triggers W1-α)`. GitHub
  appends `(#NNN)` on squash-merge (caesium's house style is a plain
  capitalized imperative subject, not conventional-commits `type(scope):`).
  Greek-letter stream suffix (α/β/γ/δ/ε/ζ/η).
- **Verify chain** (must pass before any agent raises a PR):
  ```sh
  just lint              # go fmt + go vet + golangci-lint
  just unit-test         # go test -race -coverprofile=coverage.txt ./...
  just integration-test  # go test ./test/ -tags=integration (real server)
  ```
  Everything is containerized (the `builder`/`builder-full` images carry
  the dqlite CGO deps — libuv/lz4/sqlite); host `go build`/`go test` is
  discouraged per `CLAUDE.md`. **`just unit-test` does NOT compile `test/`**
  (it is behind `//go:build integration`), so a green unit-test is necessary
  but not sufficient — `just integration-test` is the end-to-end signal.
- **Conditional gates** (run the ones the diff requires; see Phase 6.5):
  `just ui-lint` + `just ui-test` + `just ui-e2e` for `ui/**`;
  `just helm-lint` + `just helm-template` (+ the kind-based k8s integration
  run) for `helm/**` or Kubernetes-engine / distributed-Raft changes;
  `just integration-test-podman` for the podman engine adapter.
- **Merge gate (read carefully — differs from a CI-required-checks repo)**:
  `master` branch protection has **NO required status checks**
  (`required_status_checks.contexts: []`). CI is advisory at the GitHub
  level. The enforced gate is **`require_code_owner_reviews: true`** with
  CODEOWNERS `@rocketbitz @RohanDalton` (`.github/CODEOWNERS`) and
  `enforce_admins: false`. So a green CI run does NOT mechanically permit a
  merge — a CODEOWNER approval does. Because `enforce_admins: false`, a repo
  admin can `gh pr merge` without the review; a non-admin cannot. The
  orchestrator attempts the merge and, if GitHub blocks it on "review
  required", surfaces it to the user (the CODEOWNER) to approve or
  admin-merge. See `PLAYBOOK.md` Phase 6d + Phase 7b.
- **Stream-section protocol**: each stream agent edits ONLY (a) the
  checkbox(es) for its own items in the relevant streams/sections of the
  plan doc, and (b) the per-item note attached to those items. The
  orchestrator (this skill) owns the `## Progress` dashboard — agents only
  contribute their bullet via reporting back, not via plan-doc edits to
  that section.
- **Source of truth**: `docs/roadmap.md` for strategic priority/status;
  `pkg/jobdef/definition.go` for the job-definition (YAML) contract. When
  the plan doc and the source of truth disagree, the source of truth wins.

## Hard rules

- **Never block on operator approval mid-flight.** If a decision is genuinely ambiguous, follow the rubric in `PLAYBOOK.md` and document the call in your final report — do not stop to ask. (Exception: the merge gate genuinely requires a human CODEOWNER approval when you're not an admin — that's a real stop, not a judgment call. See Phase 7b.)
- **Never skip the verify chain.** If `just unit-test` / `just integration-test` is too slow to iterate on in a worktree, scope spot-checks to the closest `just lint` run or read the test files, but the full containerized chain must pass before raising a PR. There is no fast host-side way to run the integration suite (no dqlite CGO libs, no server) — go through the `just` targets.
- **Never push a fix that bypasses CI.** No `--no-verify` on the published commit beyond the documented WIP/merge-commit use; no admin-merge unless the merge gate genuinely requires it and the user has approved (see Phase 7b flake rubric).
- **Never edit another stream's plan-doc section.** Cross-stream coordination happens in the orchestrator's final dashboard sync, not in stream PRs.
- **Never `git stash` in a worktree.** Worktrees share `.git/refs/stash` across this host; a stash from one worktree can be popped into another, silently corrupting foreign work. Stream agents commit WIP instead (codified in the stream prompt templates).
- **Do not delete a worktree whose PR is still open.** Stream worktrees host work-in-progress; review-resolve agents may write into them mid-wave. Wait until Phase 7's `gh pr merge` returns success (or `--delete-branch`'s local-branch-busy warning, which still means the remote branch + PR are gone) before pruning.
- **Worktrees ARE prunable post-merge** in Phase 7.5. Each agent worktree carries Go build/module caches and a checked-out tree; without a cleanup phase, many waves of orphaned worktrees fill the disk. Local-branch deletion failures from `gh pr merge --delete-branch` are cosmetic — `git worktree unlock` + `git worktree remove --force` from outside the worktree succeeds even when the runtime had it locked.

## Escalation paths (when to stop and report)

- **No remaining unchecked items**: report to user that the plan is complete; suggest archive to `docs/exec-plans/completed/`.
- **A review comment requires substantive design judgment** (not mechanical fix): the review sub-agent should reply explaining the trade-off and tag the orchestrator's report; the orchestrator surfaces this to the user as part of the final wave summary, but does NOT block the merge.
- **CI shows a real (non-flake) regression**: stop the merge sequence for that PR; report the failing test(s) and root cause; ask the user whether to fix forward, revert, or merge with override.
- **Integration gate fails for real (Phase 6.5)**: same disposition as a real CI regression — stop the merge for that PR, report root cause from the captured log + server container logs, ask the user.
- **Merge gate requires a CODEOWNER approval you can't supply** (you are not a repo admin, and the PR is unapproved): stop and ask the user to approve or admin-merge. This is the one expected human-in-the-loop stop in caesium (CI is advisory; CODEOWNER review is the real gate).
- **Merge conflict resolution requires content choices not derivable from the two sides**: stop, report the conflict regions and the choices needed.
- **The plan doc's structure deviates from the standard "## Streams" / "## Progress" / "## Acceptance Criteria" pattern**: stop and ask the user to confirm the structural interpretation before proceeding.

Otherwise, run end-to-end without checking in.

## Output

When the wave is fully shepherded, post one summary message to the user:

- **Wave label + 1-line outcome** (e.g. "Wave 3 complete — 4 PRs merged, event-trigger engine + UI shipped, all 5 acceptance criteria met")
- **Per-stream table**: stream | PR | model | merge SHA | review comments addressed (count) | integration gate (docker/k8s/podman/ui-e2e/skipped/failed) | notable judgment calls
- **CI flakes encountered**: which checks, on which PRs, whether bypassed or rerun
- **Integration-gate summary**: which PRs ran which tier of the gate, which scenarios passed, any harness flakes encountered
- **Plan-doc state**: completion status against acceptance criteria; any items deferred or marked N/A
- **Followups for the user**: anything you escalated or that needs human attention next — including any PR awaiting a CODEOWNER approval if you weren't able to merge

That's the entire operator-facing output. No "let me know if you want me to ..." — the wave is done or it's reported as stopped with a clear reason.

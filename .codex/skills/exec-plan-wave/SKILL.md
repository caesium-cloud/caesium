---
name: exec-plan-wave
description: Execute or resume one wave of a Caesium execution plan using native Codex agents and explicit Git worktrees, through implementation, review, verification, and the requested PR or merge endpoint. Use when asked to run, ship, orchestrate, or continue a plan wave; not for drafting a plan or unrelated small fixes.
---

# Execute a plan wave

Carry one wave through the user's requested endpoint using native Codex tools.
For a request to ship a wave, continue through eligible merges and the plan
update. Respect narrower endpoints such as local changes, no push, draft PRs,
or stop before merge. Preserve existing authorization across continuations;
do not add routine confirmation gates or infer permission from a plan alone.

## Establish the wave

Read the supplied plan end-to-end, its source-of-truth documents, and applicable
`AGENTS.md`. If no path is supplied, use conversation context and plan content
to identify it. Ask only if multiple plausible plans remain; modification time
alone does not identify the intended initiative.

Read [the playbook](references/playbook.md) for dispatch, recovery, publication,
review, merge, and cleanup. Read [verification](references/verification.md)
when choosing or running gates. Use the linked worker prompts when delegating.

Resume an unfinished wave and its existing PRs/worktrees before allocating a
new number. Check live branch/PR state rather than trusting an old dashboard.
Select only included items with satisfied dependencies; preserve deferred and
optional scope. A new file named by the plan is work to implement, not a reason
to skip it. Normalize harmless heading variations without rewriting technical
scope. Use [`$draft-exec-plan`](../draft-exec-plan/SKILL.md) only if substantive
planning is needed, and do not replace an executable plan just for formatting.

## Native Codex execution

Use the available native subagent tools for independent streams when delegation
is permitted. Create and record each worktree explicitly before spawning its
writer: Codex agents may share the parent directory and filesystem. A prompt
must specify the absolute worktree, branch, base SHA, assigned items, file
ownership, validation duties, and allowed side effects.

Inherit the active model and reasoning settings unless the user requests
otherwise. Do not translate old Claude model tiers into invented Codex model
IDs. Bound parallelism by available agent slots and shared resources. If
delegation is unavailable, execute the same worktree workflow sequentially and
report that fact; do not launch Claude, a companion plugin, or nested CLI agents
as an automatic fallback.

The orchestrator owns publication, GitHub review replies, integration-resource
scheduling, merging, and the Progress dashboard. Workers implement and report;
assign commit or test duties explicitly. Inspect actual tool/network/container
access rather than assuming every Codex session has the same restrictions.

## Completion rules

- Verify worker claims against the diff and observed test results. Fix real
  in-scope failures and substantive review findings before advancing a PR.
- Bind review and test evidence to the current candidate commit. After fixes,
  conflict resolution, or base updates, refresh the evidence that changed.
- Honor the current GitHub checks and review requirements. No missing checks,
  stale green runs, disabled tests, or unsupported flake claims count as a pass.
- Post review replies only when the user has explicitly authorized addressing
  or replying to reviews; otherwise prepare the dispositions locally. Ordinary
  PR publication and merge authorization do not imply sending review messages.
- Continue independent streams when one encounters a blocker. Preserve the
  blocked work and name the exact external dependency or product decision.
- Clean up only this wave's owned worktrees after verified merge, with no
  active writer or unpreserved changes. Never sweep unrelated old worktrees.

Report the wave outcome and, per stream, items, PR, current head/merge SHA,
review disposition, verification, and blockers. State whether the plan sync is
local, in an open PR, or merged. A wave is not shipped while its required PRs or
plan sync remain open; no unchecked eligible items is not by itself proof that
all acceptance criteria are met.

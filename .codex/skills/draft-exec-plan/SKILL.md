---
name: draft-exec-plan
description: Draft or restructure a Caesium execution plan with scoped items, file ownership, dependencies, and observable acceptance criteria for exec-plan-wave. Use when asked to write an exec plan or scope an initiative; do not start implementation or wave execution.
---

# Draft execution plan

Produce a durable plan that `$exec-plan-wave` can pick up without reconstructing
scope or guessing dependencies. Use Codex's native file, shell, and question
tools; this skill has no dependency on another agent product.

## Inputs and scope

Use the initiative and problem statement from the request. Respect a supplied
output path; otherwise use `docs/exec-plans/active/<slug>.md`. When rewriting,
preserve item IDs, completion evidence, technical decisions, deferred work, and
history. Do not duplicate work owned by a sibling plan.

Read the applicable `AGENTS.md`, inspect the current branch and working tree,
and survey only relevant code, designs, the roadmap, and active plans. Stay in
the user's checkout; drafting does not require switching to or updating
`master`. Distinguish existing behavior from proposed changes to that behavior.

If a missing product decision prevents a useful plan, ask a focused question
using the available question tool and continue independent investigation.
Resolve routine file ownership and sequencing choices yourself. For exploratory
work, plan a bounded investigation or design memo with an explicit deliverable;
do not invent an implementation contract or add runnable placeholder items.

## Draft the plan

1. Read [planning guidance](references/planning.md) for Caesium integration
   surfaces, item shape, and stream ownership.
2. Use the [plan template](assets/plan-template.md). Fill every `{{...}}` token
   and remove guidance comments. Preserve its core sections; optional history
   or coordination sections may remain in an existing plan.
3. Give each item a stable ID, concrete result, `Files:`, `Depends on:` (or
   `none`), and `Verify:`. Identify proposed new files explicitly. Record
   external prerequisites with evidence needed to satisfy them. Detect missing
   dependency IDs and cycles before calling anything ready.
4. Group coherent items into streams. Define one writer per overlapping
   function/schema/composition root. Sequence dependencies explicitly; shared
   files are not automatically safe just because both changes are appends.
5. Define acceptance criteria using observable results through the real
   surface. Include required verification and how the criteria will be proven.
   Mark blocked, deferred, or optional scope separately from ready items.
6. Save the plan and add relevant cross-links within the authorized scope.
   Do not mark the roadmap shipped because a plan was written. Do not edit
   unrelated documents to satisfy a fixed cross-link quota.

## Verify and hand off

Check that all paths are existing or explicitly new; item IDs are unique;
dependencies resolve without cycles; stream ownership is coherent; acceptance
criteria cover the requested outcome; and generated Markdown has no template
tokens or broken links. Read the plan once as a fresh executor and identify the
first dependency-ready items. Open questions that block them must be visible.

Report the saved path, streams and item counts, first-wave candidates,
cross-links changed, and unresolved decisions. Honor any instruction to commit
the plan; otherwise leave it for review. Do not launch implementation from this
skill. The saved plan is the handoff to
[`$exec-plan-wave`](../exec-plan-wave/SKILL.md).

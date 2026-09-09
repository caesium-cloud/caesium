# Native Codex wave playbook

## 1. Inspect and resume

Resolve the checkout with `git rev-parse --show-toplevel`. Record branch, HEAD,
`git status --short`, `git worktree list --porcelain`, remotes, and the requested
endpoint. Keep the user's checkout on its current branch and preserve unrelated
changes. Read `AGENTS.md`, the plan, its contracts, `justfile`, `docs/ci.md`, and
the applicable workflow sections. Do not start by checking out `master`.

For a connected run, verify `gh auth status`, identify the repository and
default branch with `gh repo view --json nameWithOwner,defaultBranchRef`, and
fetch the selected remote. Caesium normally uses `origin` and `master`, but
forks and explicitly supplied bases take precedence. For a local/offline task,
use the requested local base and disclose that remote state was not refreshed.

Read Progress and any existing local run record. Enumerate relevant PRs across
all states, matching plan, wave, branch, and head. An unfinished W2 is resumed
as W2; allocate W3 only after W2's disposition is established or the user
explicitly requests it. Do not create a duplicate PR just because its worker
session ended. Reconcile merged PRs, open PRs, local commits, and dirty trees
before dispatch. A closed, unmerged PR is not shipped.

Maintain a compact recovery record under
`.codex/runs/<plan-slug>/<wave>/state.md` in the controller checkout. Include:

- Plan path and revision, requested scope/endpoint, authorization boundaries.
- Remote, base branch and SHA, stream item IDs, dependencies and file ownership.
- Absolute worktree, branch, native agent ID, current head and PR URL per stream.
- Test commands, candidate SHA, exit/result and log paths; review dispositions.
- Current phase, blockers, next action, owned resources and any cleanup done.

Update after dispatch, handoff, publish, fixes, merge, and interruption. Keep
credentials out of records. The record supports recovery; refresh mutable
facts against Git and GitHub. Commit durable results to the plan, not this
machine-specific record.

## 2. Select and divide

Read all checklist items, including optional harness/navigation sections, and
their dependencies and acceptance criteria. Ignore checkboxes in illustrative
code fences. Preserve blocked/deferred/optional labels; report exclusions.
Existing checked items require credible completion evidence before dependent
work is declared ready. If all items are deferred or blocked, report that state
instead of declaring the plan complete.

Group ready items into coherent PRs. Sequence shared functions, schema changes,
startup wiring, module changes, and other semantic conflicts. For independent
appends in a shared file, name an integration owner and merge order. Bind the
worktree to the selected base SHA. Start dependent streams after prerequisites
land unless the user requested a stack; a stack needs explicit branch and PR
base relationships and downstream retargeting after squash merges.

Assign Greek execution suffixes (`W1-α`, `W1-β`) while retaining plan item IDs
(`A1`, `B2`). Use ASCII for branch/path suffixes (`w1-alpha`) and include the
plan slug to avoid collisions between plans. Limit concurrency to actual
available slots and reserve capacity for coordination when useful.

## 3. Create worktrees and delegate

Use `git worktree list --porcelain` to find an existing matching worktree first.
Reuse it only after verifying repository, branch, plan scope, and writer state.
If a proposed branch/path already belongs to different work, choose a unique
suffix; never reset or repurpose it. For a new stream, set task-specific shell
variables to the recorded, verified values, then run:

```sh
git worktree add -b "$wave_branch" "$wave_worktree" "$wave_base_sha"
git -C "$wave_worktree" rev-parse --show-toplevel
git -C "$wave_worktree" branch --show-current
git -C "$wave_worktree" rev-parse HEAD
```

Default placement is `.codex/worktrees/<plan-slug>-<wave>-<stream>` beneath the
controller checkout. Record absolute paths. If the plan exists only as an
authorized uncommitted draft, provide its exact snapshot to the worker and
assign a single owner to land it; do not silently use an older committed plan.

Fill the [stream prompt](../assets/stream-agent-prompt.md). Use the native
spawn capability exposed by this session (for example `spawn_agent`); do not
invent Claude's `Agent`, `isolation`, `subagent_type`, or `run_in_background`
parameters. If the API lacks a working-directory argument, the prompt must
require an explicit workdir on shell commands and absolute edit paths. Require
the worker to verify its path/branch before writing. Tools may share a cwd;
spawn does not establish isolation by itself.

Keep implementation and review writers mutually exclusive on a worktree.
Workers may run assigned safe checks, but the orchestrator schedules any
shared container/image/port use. Use available native status, message,
follow-up, and wait tools to receive results and steer ongoing work. An idle
worker is not a completed stream. Keep the user informed while work continues.

If a worker ends early, inspect its report, branch, diff, and active status.
Continue the same native agent when possible, or hand the existing worktree
and a scoped recovery brief to a replacement after the prior writer stops.
Do not restart based solely on a quiet log, discard partial work, or run two
writers while trying to recover a session. Never use shared `git stash` for
coordination; preserve in-scope work in place or in an explicitly scoped WIP
commit when commits are permitted.

## 4. Inspect, verify, and publish

On handoff, compare all changed files and local commits to the assigned scope.
Read the implementation and check persistence, startup, routes, schema/cache
propagation, and public-surface tests as relevant. Treat claims that a security
check or durability requirement was stubbed as an implementation failure.
Investigate claimed dependency/tool blockers before accepting them.

Run the required [verification gates](verification.md). A worker's pass can be
reused only for the same candidate contents and relevant environment, with
command/result evidence. Record the tested commit; if tests preceded the
commit, confirm the commit contains exactly those changes. Generated formatter
changes must also be reviewed. Fix in-scope failures and rerun affected gates.

When the requested endpoint includes publication, stage explicit paths and
inspect the staged diff before committing. Never use `git add -A`, reset
foreign changes, or bypass hooks. Use the configured Git author identity; do
not fabricate a model coauthor or add Claude branding. Inspect any commits a
worker already made rather than committing again.

Check for an existing PR by repository and branch. Push the intended branch
and create or update one PR with an explicit base. Title convention:
`<Imperative subject> (<plan-slug> W<n>-<Greek suffix>)`.
Write multiline bodies to a file and pass `gh pr create --body-file <path>` or
`gh pr edit --body-file <path>`; describe final behavior and actual validation.
If an environment blocker remains, a draft PR may preserve reviewable work
when the request permits it; label unrun checks and do not claim ready/green.

## 5. Review and CI

Fetch current PR head and base, all pages of inline comments, review summaries,
issue comments, and unresolved review threads. Use `gh api --paginate` for REST
lists and cursor pagination for GraphQL connections, including thread comments
when truncated. Bot findings can appear in top-level issue comments. Ignore
empty reviews and quota/status notices. Treat every substantive finding as a
hypothesis: verify its premise against the current code, fix real defects with
meaningful regression coverage, and explain declined or stale findings.

Use the [review prompt](../assets/review-agent-prompt.md) for native review
workers after implementation writers have stopped. Workers prepare findings,
fixes and reply text; the orchestrator reconciles and publishes. Refresh the
head before applying a suggested patch or posting a reply. If the user has
authorized addressing/replying to reviews, post the disposition in its correct
thread after any fix is pushed, and resolve only threads demonstrably addressed.
Otherwise retain the prepared replies locally. Do not resolve a thread merely
because it is outdated or a bot reported a high confidence score.

For CI, read `gh pr checks <pr>` and workflow runs for the current head. Match
head/base and event; a green run on an older push is not current evidence.
Inspect failed job logs and upstream dependencies. A skipped integration job
after a failed builder is not a pass. Read current change filters and the
`ci-ok` gate in `.github/workflows/ci.yml` and `docs/ci.md`. Missing checks or
unexplained skips block readiness; expected path-filter skips need evidence.

Classify failures using logs, reproduction, and baseline evidence. A timing
test name or a similar old failure does not prove a flake; races, durability
failures, and security regressions require fixes. Retry a demonstrated transient
infrastructure failure at most once before investigating further. Persistent
environment failures remain blockers, not permission to merge red CI. Preserve
logs and continue unaffected streams. Fix in-scope real failures autonomously;
ask only for an unresolved product choice, missing access, or scope expansion.

## 6. Integrate and merge

Respect the requested endpoint. A local-only run does not push; a PR-only run
reports readiness without merging. For a shipping run, order prerequisites
first, then high-conflict changes, then independent streams. Check current
GitHub branch protection/rules and review requirements rather than relying on
old claims about admins or required checks. An inaccessible protection API is
not evidence that protection is absent. Do not alter repository settings,
bypass checks with `--admin`, or approve the author's own PR.

Before each merge, refresh the remote base. Integrate relevant preceding merges
in the PR worktree (never by switching the user's main checkout). Merge the base
into the branch by default to preserve published history; use a requested
rebase strategy when authorized, with an explicit lease for any force push.
Do not operate while a worker writes or the worktree has unpreserved changes.

Resolve conflicts semantically: retain both intended routes/registrations,
preserve model dependency order, thread schema fields through decoding and
cache hashing, and choose compatible module versions before regenerating sums
in the builder. Never take a blind union of incompatible contracts. Check
`git diff --check`, `git diff --name-only --diff-filter=U`, and the resulting
files for unresolved markers. Commit only the resolved paths. Rerun affected
checks on the new candidate, push, and wait for its current checks and reviews.

Immediately before merging, confirm:

- The remote PR head equals the reviewed and verified candidate head.
- The candidate includes the required base/dependencies; no subsequent base
  change invalidates the evidence.
- Required and scope-relevant checks pass, reviews are satisfied, and no
  substantive unresolved finding remains. Refresh asynchronously posted reviews.

Use the verified head as the merge precondition:

```sh
gh pr merge "$wave_pr" --squash --match-head-commit "$wave_verified_head"
```

Read PR state, `mergedAt`, and `mergeCommit` afterward. Never infer merge
success or failure solely from a local branch-deletion warning or shell exit
status. Record the merge SHA, fetch the updated base, and reassess the next PR.
If the base advances during this sequence, re-evaluate affected evidence.
When GitHub requires human review, preserve the PR and report that exact blocker
while continuing independent authorized work.

## 7. Finalize the plan and preserve work

Reconcile the plan against actual results. Workers may propose checks for
their own verified items; the orchestrator alone updates the shared Progress
dashboard with PRs, merge SHAs, review outcomes, gates, and blockers. Pending
PRs must remain visibly pending. Update relevant roadmap/design status only
for behavior that shipped. Completion requires acceptance evidence, not just
a count of checked boxes. Preserve deferred scope and prior history.

Use a small dedicated plan-sync branch/worktree and PR for post-merge dashboard
changes when publication is in scope. Apply the requested endpoint and normal
gates to that PR too; do not push directly to the default branch or leave an
unreported sync PR unfinished. Archive under `docs/exec-plans/completed/` only
when acceptance is met and included in the requested workflow; update links
when moving it. A plan with unmet blockers stays active.

Clean up only worktrees recorded as created/owned by this wave. Before removal,
verify all of the following: no active agent/process, clean tracked and
untracked state, no valuable ignored artifacts, confirmed merged PR, and local
HEAD equal to the PR head whose content was merged (no later local commits).
Then use ordinary `git worktree remove <path>` from outside it. A refusal or
lock is a reason to inspect and preserve, not to add `--force`. Squash merges
do not make branch ancestry alone a reliable proof of preserved work. Retain
closed-but-unmerged, no-PR, dirty, and uncertain worktrees. Do not sweep older
waves or delete remote branches as opportunistic housekeeping.

Finish with the outcome, per-stream evidence, plan-sync status, and precise
remaining blockers or next actions. Keep the recovery record for unfinished
work so a continuation resumes the same wave.

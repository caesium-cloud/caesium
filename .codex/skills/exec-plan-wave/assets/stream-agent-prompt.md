# Native Codex stream assignment

Fill every `{{...}}` value before dispatch. This is a work assignment, not a
shell script. Use the native agent API available in the session.

You own {{stream_label}} for Caesium plan {{plan_path}}.

- Worktree (absolute): {{worktree_path}}
- Expected branch: {{branch}}
- Starting base SHA: {{base_sha}}
- Assigned items and acceptance criteria: {{items_and_criteria}}
- Allowed files/sections: {{file_ownership}}
- Dependencies and sibling coordination: {{dependencies_and_coordination}}
- Assigned validation and shared-resource reservation: {{validation}}
- Allowed Git operations: {{git_operations}}
- Plan snapshot/source: {{plan_revision_or_snapshot}}

Before writing, run `git rev-parse --show-toplevel`, `git branch --show-current`,
`git rev-parse HEAD`, and `git status --short` in the assigned worktree. Report
any mismatch. Read applicable `AGENTS.md` and the supplied plan/contract.
Use this explicit workdir on every shell call and absolute paths for edits;
agents can share the parent's cwd and filesystem.

Implement only the assigned items and their required tests/docs. Do not change
another stream's files or Progress dashboard; report a needed shared change to
the orchestrator. Update only your own item checkbox and note when its
verification supports it, or report it pending if validation remains. A proposed
checkbox in a branch is not a claim of merge. Preserve unrelated edits.

Trace the real public entry point, persistence, startup/registration, config,
schema/cache, and read path as relevant. New CLI commands and REST queries need
binary/HTTP integration tests in `test/`. Parse machine output from separately
captured stdout and enable the feature on the integration server. Do not stub
required security, durability, or runtime behavior to satisfy a test.

Run assigned checks with the repository's containerized recipes. Format only
changed Go files explicitly. Do not run shared container/image/port operations
without the assigned reservation. If access is unavailable, report the command
and exact blocker; do not assume a pass or substitute host builds.

Honor the allowed Git operations above. Stage only explicit owned files if
staging is assigned. Do not stash, reset foreign work, bypass hooks, push,
create PRs, send GitHub replies, merge, or remove worktrees. Publication and
shared integration gates belong to the orchestrator. If interrupted, leave
work preserved and report the exact next action.

Return:

- Worktree/branch/HEAD and whether changes are committed, staged, or unstaged.
- Implemented item IDs, changed paths, resulting behavior and remaining gaps.
- Each test command and result, candidate revision, and log path if available.
- Proposed plan updates and reviewable PR summary.
- Dependencies, anomalies, blockers, and whether any process is still running.

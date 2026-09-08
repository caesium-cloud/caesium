# Native Codex review assignment

Fill every `{{...}}` value before dispatch after the implementation writer stops.

Review and address the assigned findings for {{pr_url}} in Caesium.

- Existing worktree (absolute): {{worktree_path}}
- Expected branch and PR head: {{branch_and_head}}
- Base SHA: {{base_sha}}
- Plan items and allowed files: {{scope}}
- Review bodies with comment/thread IDs, author, paths and anchors: {{comments}}
- Assigned checks and shared-resource reservation: {{validation}}
- Allowed Git operations: {{git_operations}}

Verify worktree, branch, HEAD and status in the assigned directory before
writing. Read applicable `AGENTS.md`. Do not blindly pull, reset, or create a
second worktree for the same branch. If the supplied head is stale, report it
to the orchestrator and refresh context before applying fixes.

Treat each comment as a hypothesis. Trace the actual code, callers, persistence,
registration, configuration and public surface as relevant. Apply confirmed
fixes and add a meaningful regression when needed; do not apply suggestion
blocks merely because they compile. Check integration call sites before
deleting apparently unused code. Explain stale/incorrect findings with concrete
evidence. Substantive security, durability and correctness findings remain
merge blockers until resolved.

Use explicit workdirs and absolute edit paths. Stay within assigned ownership,
preserve unrelated changes, and leave Progress to the orchestrator. Use the
containerized checks assigned above; reserve shared resources before using
them. Report unavailable checks honestly. Do not narrow tests to get green.

Honor allowed local Git operations. Do not stash, bypass hooks, push, merge,
remove worktrees, or post/resolve GitHub threads. Prepare reply text for the
orchestrator, which checks authorization and refreshes the head before posting.
If two comments identify one defect, fix once and report both comment IDs.

Return the final head and worktree state, changed paths, verification evidence,
and a table: comment/thread ID | confirmed/stale/declined/blocked | evidence or
fix | proposed reply. Include unresolved design decisions and any running
process. Distinguish a prepared reply from one actually posted.

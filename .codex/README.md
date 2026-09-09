# Caesium skills for Codex

These are native Codex adaptations of this repository's plan-drafting and
wave-execution skills. They require no Claude installation, plugin, or helper
process. The existing `.claude/skills` remain available for Claude users.

Start Codex in this checkout and invoke either skill:

```text
$draft-exec-plan Draft a plan for <initiative>.
$exec-plan-wave Ship the next wave of docs/exec-plans/active/<plan>.md.
$exec-plan-wave Resume W2 of docs/exec-plans/active/<plan>.md; stop at verified PRs.
```

The maintained files live in `.codex/skills`. The two relative symlinks in
`.agents/skills` expose them through Codex's repository skill discovery. There
is one copy of each Codex skill to maintain. Codex supports symlinked skill
folders; see the [official skills documentation](https://developers.openai.com/codex/skills/).
If the skills do not appear in the skill picker after checkout, restart Codex
and check that Git materialized the symlinks as links, not text files.

Plan drafting writes a plan without starting implementation. Wave execution
uses native Codex subagents when available and permitted, with one explicitly
assigned Git worktree per writer. It executes sequentially when delegation is
unavailable. It inherits the selected model and permissions, checks available
tools, and records unfinished work so a later invocation can resume it.

For a request to **ship a wave**, the endpoint includes normal merging of
eligible PRs and the plan update. A narrower request such as `no push`, `draft
PRs only`, or `stop before merge` takes precedence. Review replies are posted
only when the user has authorized addressing or replying to reviews. The skill
does not grant permission to bypass branch protection or change user settings.

The adaptations also require current-commit review and CI evidence, real CLI
and HTTP integration coverage, and cleanup limited to owned, merged worktrees.
`.codex/runs` holds local recovery notes; `.codex/worktrees` holds temporary
checkouts. Both are ignored. No personal configuration or credentials belong
in this directory.

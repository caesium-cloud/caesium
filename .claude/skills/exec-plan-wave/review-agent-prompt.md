# Review-Resolve Agent Prompt Template

Substitute every `{{PLACEHOLDER}}` before passing this to `Agent({prompt: ...})`.

Required placeholders:
- `{{PR}}` — PR number, e.g. `212`
- `{{REPO}}` — `caesium-cloud/caesium`
- `{{WORKTREE_PATH}}` — absolute path of the existing worktree, e.g. `/Users/cryan/dev/caesium/.claude/worktrees/agent-a3d38286ebfbe3b78`
- `{{BRANCH_NAME}}` — the worktree's branch name (`worktree-agent-<id>`)
- `{{STREAM_LABEL}}` — e.g. `W3-β`
- `{{STREAM_TOPIC}}` — short PR topic, e.g. "event-trigger evaluation engine + store"
- `{{COMMENTS}}` — for each actionable comment: id, author, file:line, body, and orchestrator's disposition guidance (apply / investigate / explain)

---

You are addressing review comments on PR https://github.com/{{REPO}}/pull/{{PR}} ({{STREAM_LABEL}}, {{STREAM_TOPIC}}) in the **caesium** repo (a Go job scheduler; builds/tests are containerized via `just` targets).

== Worktree to operate in ==

```sh
cd {{WORKTREE_PATH}}
```

This worktree has the PR branch `{{BRANCH_NAME}}` checked out. **Stay in this worktree** for all work — do NOT spawn a fresh worktree, the branch is locked here.

First action: `git pull --ff-only origin {{BRANCH_NAME}}` to confirm you're current. (If this errors with "not a fast-forward", investigate — your branch may have been force-pushed.)

== Comments to address ==

{{COMMENTS}}

== Disposition guidance ==

For each comment:

- **Apply**: make the change exactly as suggested (or its semantic equivalent if the literal code doesn't compile). Verify via `just lint` after.
- **Investigate-then-decide**: do the investigation (grep, Read) the orchestrator hinted at; if the comment's premise is correct, fix it; if stale or wrong, reply explaining what you found.
- **Explain**: do not change code; reply with a clear technical reason.
- **Verify-deletion**: for "dead code" comments, confirm with `grep` the symbol/function is genuinely unreferenced (not even by reflection, struct tags, or test-only code). If confirmed dead, delete and reply.

If multiple comments (e.g. gemini-code-assist and greptile) target the same defect, fix once and reply to BOTH pointing at the same commit.

caesium-specific things review bots commonly flag — handle carefully:
- **Missing RBAC policy entry** for a new protected route: a new route absent from `endpointPolicy` in `internal/auth/rbac.go` fails CLOSED (request denied), which is safe but means the route doesn't work. Add the policy entry with the intended minimum role — don't grant a too-low role.
- **A security stub**: if a comment flags a stubbed `validateSignature`/`ValidateKey`/`HasRole`/`CheckScope`/`subtle.ConstantTimeCompare` swap — this is merge-blocking. Implement it for real; do not reply "will follow up".
- **Missing `models.All` registration** (a new model that won't migrate) or **missing `Register()` entry** (a metric absent from `/metrics`): apply.
- **Missing `internal/cache/hash.go` update** for a new execution-affecting field: apply, or cached runs go stale.

== After fixes ==

1. Run the verify chain — at minimum `just lint`. If you changed Go logic, run `just unit-test` (containerized). If you changed runtime behavior, the orchestrator's Phase 6.5 integration gate will re-check end-to-end against your post-fixup branch.
2. Stage and commit:
   ```sh
   git add -A
   git commit -m "Address review feedback (PR #{{PR}})" --no-verify
   ```
   Use `--no-verify` only if a pre-commit hook would re-run the verify chain you already ran.
3. Push:
   ```sh
   git push origin {{BRANCH_NAME}}
   ```

If the PR's only comments were declines (no code changes needed), skip the commit/push and go straight to replies.

== Integration-gate awareness ==

If this PR touches runtime paths (`internal/**`, `api/**`, `pkg/**`, `cmd/**`, `test/**`, `helm/**`, `ui/**`), the orchestrator runs the scope-aware integration gate (`just integration-test`, plus k8s/podman/ui-e2e tiers as the diff requires) against your post-fixup branch before merging. You do NOT run that sequence yourself. But: don't push a fixup that *narrows* end-to-end coverage (e.g. deleting code that looks dead but is reached only from an integration scenario under `test/`). If a comment asks you to delete code that looks dead, `grep test/` for the symbol before deleting; if found, push back in the reply rather than silently complying.

== Reply to every comment ==

```sh
SHA="$(git rev-parse --short HEAD)"
gh api -X POST "repos/{{REPO}}/pulls/{{PR}}/comments/<COMMENT_ID>/replies" \
  -f body="<reply text — see template below>"
```

**Reply templates** (adapt to comment specifics):

For an applied fix:
> Applied — <one-line description of the fix> in `$SHA`.

For an applied fix with caveat:
> Applied — <fix description> in `$SHA`. Note that <caveat / what was preserved / what changed semantically>.

For an investigated-and-stale comment:
> Investigated — <what you actually found, e.g. "the route is already gated by the `Auth` middleware in `bind.go`; the policy entry exists in `rbac.go`">. No code change in this PR.

For a declined suggestion:
> Considered but not applied — <reason>. <Pointer to alternative approach or to where the concern is already addressed>.

**Reply to ALL actionable comments**, including ones you declined. Bot status notifications ("usage limits reached", quota warnings) are NOT actionable — skip those.

== Report format (final message to orchestrator) ==

- **Commit SHA pushed** (or "no commit — all replies, no fixes")
- **Per-comment outcome table**: comment id | disposition (applied / investigated / declined) | one-line summary | reply posted ✓
- **Verify chain**: which steps passed
- **Anomalies**: stale comments, false-alarm flags, anything surprising

Be concise. The orchestrator is sequencing this PR into a merge and only needs the operational state.

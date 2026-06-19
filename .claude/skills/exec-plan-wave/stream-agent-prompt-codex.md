# Stream Agent Prompt Template — Codex variant

Use this template when dispatching to `subagent_type: "codex:codex-rescue"`
(or when the orchestrator dispatches `codex-companion` directly into an
orchestrator-owned worktree per `PLAYBOOK.md` Phase 3). Two things make codex
fundamentally different in **caesium**:

1. **No network egress.** Codex's sandbox blocks DNS for `github.com` and
   doesn't surface the host `gh` token, so `git push` / `gh pr create`
   deterministically fail.
2. **No Docker, no CGO build env.** caesium's entire verify chain
   (`just lint`, `just unit-test`, `just integration-test`) runs inside
   Docker containers, and the codex sandbox has no Docker. A bare host
   `go test ./...` would need the dqlite CGO libs (libuv/lz4/sqlite) that
   live only in the builder image. So a codex agent can run **no** meaningful
   verification beyond `gofmt`.

Therefore: this agent **implements + gofmts + stages + STOPS**. The
orchestrator runs the full verify chain (`just lint` + `just unit-test`,
plus the Phase 6.5 integration gate) and publishes the PR from outside the
sandbox in Phase 4.5.

Substitute every `{{PLACEHOLDER}}` before passing to `Agent({prompt: ...})`.
Required placeholders: same as `stream-agent-prompt.md`
(`{{WAVE_LABEL}}`, `{{STREAM_LABEL}}`, `{{PLAN_SLUG}}`, `{{STREAM_DESC}}`,
`{{ITEMS}}`, `{{PLAN_DOC}}`, `{{ITEM_DETAIL}}`, `{{COORDINATION}}`,
`{{PR_TITLE}}`).

---

You are wave-{{WAVE_LABEL}} stream {{WAVE_LABEL}}-{{STREAM_LABEL}} of the {{PLAN_SLUG}} plan, dispatched as a codex sub-agent. Your job: {{STREAM_DESC}} ({{ITEMS}}).

This is the **caesium** repo: a Go distributed job scheduler (echo HTTP API, Cobra CLI, GORM over dqlite/SQLite, embedded React/Vite UI, Prometheus metrics, Docker/Podman/Kubernetes runtimes).

## ══════════════════════════════════════════════════════
## ⛔ STASH BANNED ⛔  NO GIT STASH  ⛔ STASH BANNED ⛔
## ══════════════════════════════════════════════════════

**BEFORE any `git` command, ask yourself: "Is this `git stash`?" If yes, STOP.**

Do NOT run `git stash`, `git stash push`, `git stash -u`, or `git stash --include-untracked`.

**The structural risk:** worktrees share `.git/refs/stash` with every sibling on this host. A stash from your worktree creates an entry any sibling can pop with no error, silently overwriting foreign work. There is no "just for a moment" exception.

**Always commit instead:**
```sh
git add -A
git commit -m "wip: <reason>" --no-verify
# ... do what you needed ...
git reset HEAD~1
```

## ══════════════════════════════════════════════════════

== Plan doc and stream-section protocol ==

- Plan doc: `{{PLAN_DOC}}`
- Edit ONLY (a) your own item's checkbox(es) and per-item note in the relevant streams/sections of the plan doc, and (b) any one-line top-of-doc state your stream demonstrably changes. Do NOT add a Wave-{{WAVE_LABEL}} entry to `## Progress` — the orchestrator owns that.
- Stay on the worktree branch (`git symbolic-ref --short HEAD` to read its name).

== Items in scope ==

{{ITEM_DETAIL}}

== Coordination with sibling streams ==

{{COORDINATION}}

== Implementation — caesium conventions ==

Follow the existing repo patterns:

- **Persistent state**: GORM model in `internal/models/<name>.go` registered in the `All` slice in `internal/models/models.go` — ORDER matters (parents/FK-targets before children). No hand-written SQL; struct tags ARE the schema. Hot per-run tables also go in `hotPathModels()` (`pkg/db/db.go`) + the `hotTables` map (`pkg/db/router.go`). No concurrent-writer patterns (writes serialize through Raft).
- **Business logic**: `internal/<feature>/` (`store.go` pattern). Background processors `New...(deps).Start(ctx)`, env-gated, wired in `cmd/start/start.go`.
- **REST routes**: controller in `api/rest/controller/<feature>/`, service in `api/rest/service/<feature>/`, route line in `Protected()` of `api/rest/bind/bind.go` (+ import). GraphQL (`api/gql/`) is a live-but-placeholder endpoint (only a `place` query; mounted at `/gql` only when auth is off) — add features via REST, not GraphQL.
- **CLI**: `cmd/<group>/` exporting `var Cmd`; subcommands `Cmd.AddCommand(...)` in `init()`; new top-level group → append to the `cmds` slice in `cmd/execute.go`.
- **Metrics**: `caesium_*` var in `internal/metrics/metrics.go` + add it to the `prometheus.MustRegister(...)` list in `Register()` (two edit sites).
- **Config**: a `CAESIUM_*` field on the `Environment` struct in `pkg/env/env.go` (+ `validate()` if cross-field).
- **Job-schema change**: edit `pkg/jobdef/definition.go` — add the field to BOTH the exported struct AND the inner `rawStep` in `UnmarshalYAML`, plus a `Validate()` rule; thread it through `internal/jobdef/runtime/spec.go`, all three engines `internal/atom/{docker,kubernetes,podman}/engine.go`, and `internal/cache/hash.go` (cache key MUST include execution-affecting fields). Update the job docs + `docs/examples/*.job.yaml`.
- **UI page**: `ui/src/features/<feature>/` + route in `ui/src/router.tsx` + method in `ui/src/lib/api.ts` + nav in `ui/src/components/layout/Sidebar.tsx`.

**Never stub a security-critical function.** Auth/crypto/signature verification/access-control middleware/session+token validation/secret resolution are merge-blocking when stubbed (a stubbed `validateSignature`/`ValidateKey` = forgery; `HasRole`/`CheckScope` = privilege escalation; `subtle.ConstantTimeCompare` → `==` = timing/forgery hole). If you hit a hard problem on one of these paths, VERIFY the obstruction against `go.mod`/`go.sum` before concluding it's blocked. If you still can't implement, return WITHOUT staging and report — the orchestrator will fix-forward.

**Never `git stash`** — see the banner. Commit WIP instead.

== Verification — what you CAN and CANNOT run ==

You are in a sandbox with **no Docker and no dqlite CGO libs**, so you CANNOT run `just lint`, `just unit-test`, or `just integration-test` — every one needs a container. Do NOT attempt them; they will fail on environment, not on your code, and waste time.

What you CAN and SHOULD do:
- `gofmt -l -w <files-you-changed>` — format every Go file you edited (do NOT `gofmt` the whole tree; only your changed files).
- Re-read your diff for obvious issues (unused imports, the `models.All` registration, the `Register()` metric entry, the dual `Step`/`rawStep` edit, the `internal/cache/hash.go` cache-key update).

The orchestrator runs the real `just lint` + `just unit-test` + integration gate at Phase 4.5 from outside the sandbox. Your job is a correct, complete, gofmt-clean implementation — not a verified one.

== Stop after staging + report — do NOT publish ==

After implementing + gofmt:

1. **Stage your work**:
   ```sh
   git add -A
   git status --short
   ```
2. **Edit the plan doc** (`{{PLAN_DOC}}`): tick your item's checkbox and append a per-item note describing what landed. ONLY your stream's items.
3. **Stage the plan-doc edit** with `git add -A` again.
4. **STOP.** Do NOT run `git commit`, `git push`, or `gh pr create`. The sandbox blocks DNS for `github.com` and the host's `gh` token isn't visible, so all three fail. The orchestrator handles commit + push + PR creation in Phase 4.5.

   The ONE exception: if you've already run `git commit` this turn, DON'T try to undo it — leave the commit as-is. The orchestrator detects committed vs. uncommitted state when it picks up your worktree.

== Final report (to the orchestrator) ==

Brief — the orchestrator reads many of these. Include:

- **Files staged**: `git diff --cached --name-only` output, grouped (implementation, models + registration, tests, plan doc, docs).
- **gofmt**: confirm you formatted your changed files.
- **Suggested commit message / PR title** (the orchestrator copies it):
  ```
  {{PR_TITLE}}

  <2-3 sentence body describing what changed and why>
  ```
- **Suggested PR body bullets**: 3–5 bullets summarizing the change.
- **Plan-doc edits**: which checkbox(es) you ticked.
- **Coordination notes**: anything sibling-stream rebases or the dashboard sync should know (e.g. "added a `go.mod` dep — expect a `go.sum` conflict at merge").
- **Anomalies**: pre-existing issues, surprises, anything the orchestrator's Phase 4.5b verify should watch for.

If you hit a real implementation blocker (not a sandbox blocker), STOP and return WITHOUT staging. Report what blocked you. Do not stage a broken state.

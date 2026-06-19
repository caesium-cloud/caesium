# Stream Agent Prompt Template

Use this template for `subagent_type: "general-purpose"` (sonnet/opus). For
codex sub-agents (`subagent_type: "codex:codex-rescue"`), use
[`stream-agent-prompt-codex.md`](stream-agent-prompt-codex.md) instead —
that template omits the publish step (codex sandbox blocks network egress
and can't run caesium's containerized verify chain) and the orchestrator
handles verify + commit + push + PR creation in Phase 4.5.

Substitute every `{{PLACEHOLDER}}` before passing this to `Agent({prompt: ...})`.

Required placeholders:
- `{{WAVE_LABEL}}` — e.g. `W3`
- `{{STREAM_LABEL}}` — Greek letter, e.g. `α`
- `{{PLAN_SLUG}}` — the plan's slug, e.g. `event-triggers`
- `{{STREAM_DESC}}` — short stream description, e.g. "event-trigger evaluation engine"
- `{{ITEMS}}` — bulleted list of plan-doc item ids covered by this stream (e.g. `A1 + A2`)
- `{{PLAN_DOC}}` — relative path to plan doc (e.g. `docs/exec-plans/active/event-triggers.md`)
- `{{ITEM_DETAIL}}` — for each item, copy the plan doc's bullet text + any extra context the orchestrator gathered (file paths, suggested approach)
- `{{COORDINATION}}` — list of files/sections this stream owns vs. files other streams in the same wave own (so this agent doesn't touch them)
- `{{PR_TITLE}}` — `<Imperative subject> ({{PLAN_SLUG}} {{WAVE_LABEL}}-{{STREAM_LABEL}})`

---

You are wave-{{WAVE_LABEL}} stream {{WAVE_LABEL}}-{{STREAM_LABEL}} of the {{PLAN_SLUG}} plan. You are running in an isolated git worktree off `master`. Your job: {{STREAM_DESC}} ({{ITEMS}}).

This is the **caesium** repo: a Go distributed job scheduler (echo HTTP API, Cobra CLI, GORM over dqlite/SQLite, embedded React/Vite UI, Prometheus metrics, Docker/Podman/Kubernetes runtimes). Builds and tests are containerized — host `go build`/`go test` is discouraged (`CLAUDE.md`); use the `just` targets, which run inside the `caesium-builder` images.

## ══════════════════════════════════════════════════════
## ⛔ STASH BANNED ⛔  NO GIT STASH  ⛔ STASH BANNED ⛔
## ══════════════════════════════════════════════════════

**BEFORE any `git` command, ask yourself: "Is this `git stash`?" If yes, STOP.**

Do NOT run `git stash`, `git stash push`, `git stash -u`, or `git stash --include-untracked`.

**The structural risk:** worktrees share `.git/refs/stash` with every sibling worktree on this host. A `git stash` from your worktree creates an entry any concurrent sibling can pop — and will get no error when they do, silently overwriting foreign work. There is no "just for a moment" exception and no safe stash window. Every stash recovery is a lottery win, not a skill.

**Always commit instead:**
```sh
git add -A                                   # stage all changes (including untracked)
git commit -m "wip: <reason>" --no-verify   # park work safely
# ... do what you needed to do ...
git reset HEAD~1                             # undo commit and unstage changes
# OR
git reset --hard HEAD~1                      # drop the WIP commit entirely before pushing
```

## ══════════════════════════════════════════════════════

== Plan doc and stream-section protocol ==

- Plan doc: `{{PLAN_DOC}}`
- **Critical rule**: you may edit ONLY (a) your own item's checkbox(es) and per-item note in the relevant streams/sections of the plan doc, and (b) any one-line top-of-doc state your stream demonstrably changes. Do NOT edit any other stream's content. Do NOT add a Wave-{{WAVE_LABEL}} entry to the `## Progress` section — the orchestrator owns that.
- The branch you're on was auto-created by `isolation: "worktree"`. Stay on it. Get its name with `git symbolic-ref --short HEAD` when you need to push.

== Items in scope ==

{{ITEM_DETAIL}}

== Coordination with sibling streams ==

{{COORDINATION}}

== Implementation — caesium conventions ==

Follow the existing repo patterns:

- **Persistent state**: add a GORM model in `internal/models/<name>.go` (UUID PKs `gorm:"type:uuid;primaryKey"`, JSON blobs via `datatypes.JSON`, soft-delete `gorm.DeletedAt gorm:"index"`, FK `constraint:OnDelete:CASCADE`). **Register it in the `All` slice in `internal/models/models.go`** — ORDER matters (parent/FK-target tables before children) or AutoMigrate breaks at runtime. There are NO hand-written SQL migrations; struct tags ARE the schema. If your table is a **hot per-run table**, also add it to `hotPathModels()` in `pkg/db/db.go` AND the `hotTables` map in `pkg/db/router.go`. Use `db.Connection()` (catalog) or the router for hot tables; do NOT introduce concurrent-writer patterns (writes serialize through Raft; `busy_timeout` is a no-op).
- **Business logic**: under `internal/<feature>/` (the `store.go` pattern: a struct holding `*gorm.DB` with typed CRUD). Background processors follow `New...(deps).Start(ctx)` and are wired as env-gated goroutines in `cmd/start/start.go`.
- **REST routes**: handler in `api/rest/controller/<feature>/`, business logic in `api/rest/service/<feature>/`, and a `g.METHOD("/path", controller.Handler)` line in the `Protected()` func of `api/rest/bind/bind.go` (+ the controller import). Public/internal routes (health, /metrics, SSO, webhooks) are wired in `api/api.go`. **Do NOT add features via GraphQL** — `api/gql/` is a live-but-placeholder endpoint (registered at `/gql` only when auth is off; its schema has just a `place` query). New read/write surfaces go through REST.
- **CLI**: a `cmd/<group>/` package exporting `var Cmd`; subcommands self-register via `Cmd.AddCommand(...)` in their file's `init()`. A new top-level group → append `<pkg>.Cmd` to the `cmds` slice in `cmd/execute.go`.
- **Metrics**: declare a `caesium_*` collector in the `var (...)` block of `internal/metrics/metrics.go` AND add it to the `prometheus.MustRegister(...)` list in `Register()` (two edit sites). Assert via `internal/metrics/testutil` in a `*_test.go`.
- **Config**: add a `CAESIUM_*` field to the `Environment` struct in `pkg/env/env.go` (`envconfig:` tag + `default:`); cross-field validation goes in `validate()`.
- **Job-schema change**: edit `pkg/jobdef/definition.go` — add the field to BOTH the exported `Step`/`Definition` struct AND the inner `rawStep` in `UnmarshalYAML` (the dual declaration), plus a `Validate()` rule. Then thread it through `internal/jobdef/runtime/spec.go`, all three engines `internal/atom/{docker,kubernetes,podman}/engine.go`, and **`internal/cache/hash.go`** (the cache key MUST include any field that affects execution, or cached results go stale). Update the docs (`docs/caesium-job-llm-reference.md`, `docs/job-definitions.md`, `docs/job-schema-reference.md`) + `docs/examples/*.job.yaml`. `git show --stat 53cdb57` is the canonical 23-file example.
- **UI page**: a `ui/src/features/<feature>/` component, a route in `ui/src/router.tsx`, an API method in `ui/src/lib/api.ts`, and a nav entry in `ui/src/components/layout/Sidebar.tsx`. UI-gated capabilities add a field to the `Features` struct in `api/rest/service/system/system.go`.

== Verify chain (must pass before raising PR) ==

```sh
just lint              # go fmt + go vet + golangci-lint
just unit-test         # go test -race -coverprofile=coverage.txt ./...
just integration-test  # go test ./test/ -tags=integration against a real server
```

Everything is containerized; the first `just unit-test` / `just integration-test` builds the builder images (cached afterward). **`just unit-test` does NOT compile `test/`** (it's behind `//go:build integration`), so run `just integration-test` too if your change touches runtime behavior — a green unit-test alone is not end-to-end coverage.

**Conditional gates** — also run the ones your diff requires:
- `ui/**` changes: `just ui-lint` + `just ui-test` + `just ui-e2e` (Playwright)
- `helm/**` or Kubernetes-engine changes: `just helm-lint` + `just helm-template`
- podman-engine adapter changes: `just integration-test-podman`

If a verify step fails: investigate and fix the root cause. Do NOT skip hooks (`--no-verify` on the published commit), do NOT comment-out failing tests, do NOT push a broken branch.

**Never `git stash`** — see the top-of-prompt STASH BANNED banner. If you need to set work aside: `git add -A && git commit -m "wip: <reason>" --no-verify`, then `git reset HEAD~1` later.

**Never stub a security-critical function.** Auth, crypto, signature verification, access-control middleware, session/token validation, secret resolution, and similar paths are merge-blocking when stubbed — a stubbed `validateSignature`/`ValidateKey` is a forgery hole; a stubbed `HasRole`/`CheckScope` is a privilege-escalation hole; swapping a `crypto/subtle.ConstantTimeCompare` to `==` is a timing/forgery hole. If you hit a hard problem on one of these paths (a dep that "would conflict", an upstream API mismatch), VERIFY the obstruction by checking `go.mod`/`go.sum` for transitive presence of the named dep before concluding it's blocked. If after verification you still can't implement, return WITHOUT pushing and report the obstruction in detail — the orchestrator will dispatch a fix-forward. Do not stub-and-document; the documentation will not save users from a P0.

== Integration gate (orchestrator runs this; you do NOT) ==

If your PR's diff touches runtime paths (`internal/**`, `api/**`, `pkg/**`, `cmd/**`, `test/**`, `helm/**`, `ui/**`), the orchestrator runs the scope-aware integration gate (`just integration-test`, plus the k8s/podman/ui-e2e tiers as the diff requires) against your branch before merging — a real caesium server with a sharded database. You do NOT run the gate sequence yourself (it's serialized at the host level on the Docker daemon). Implications:

- Your code is exercised against a real server, real scheduler/engine paths, and a sharded DB router (`CAESIUM_DATABASE_SHARDS=4`) — not just unit tests with mocks. Don't introduce changes that pass `just unit-test` by mock/stub coincidence but misbehave end-to-end (e.g. a query that assumes a single shard, or rename/ordering that races real run-apply timing).
- If you add a new integration scenario under `test/`, tag it `//go:build integration` and follow the existing harness pattern (drive the server over REST + the copied CLI via `CAESIUM_CLI_PATH`), or the gate won't compile it.

== Raising the PR ==

```sh
git add -A
git commit -m "$(cat <<'EOF'
{{PR_TITLE}}

<2-3 sentence body describing what changed and why>

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
git push -u origin "$(git symbolic-ref --short HEAD)"
gh pr create --title "{{PR_TITLE}}" --body "$(cat <<'EOF'
## Summary
- <bullet 1>
- <bullet 2>

## Test plan
- [x] just lint
- [x] just unit-test
- [x] just integration-test
- [ ] <conditional gate if applicable: just ui-e2e / just integration-test-podman / just helm-lint>

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Note: `master` has no required status checks but DOES require a CODEOWNER review to merge. You only raise the PR; the orchestrator handles the merge gate.

== Report format (final message to orchestrator) ==

- **PR URL** (full HTTPS link)
- **Final state**: files changed, new packages/models added, items ticked
- **Verify chain**: which of `just lint` / `just unit-test` / `just integration-test` passed; any conditional gate run; any pre-existing failure noted
- **Plan-doc edits**: which checkboxes you ticked; any per-item notes you added
- **Coordination notes**: anything the orchestrator should know for sibling-stream rebases or the final dashboard sync (e.g. "I added a `go.mod` dep — expect a `go.sum` conflict")
- **Anomalies**: anything surprising — a `models.All` ordering subtlety, a test already broken on master, a needed import that wasn't obvious, etc.

If the verify chain fails irrecoverably, return WITHOUT pushing. Report what failed and why. Do not push a broken branch.

Be concise. The orchestrator reads many of these.

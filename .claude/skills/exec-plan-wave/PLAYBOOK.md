# Wave Orchestration Playbook (caesium)

Detailed instructions for each phase of the `exec-plan-wave` skill. Read
this once at phase 1 start; the rubrics here drive autonomous decisions
across all later phases.

Repo facts baked in: remote `caesium-cloud/caesium`, default branch
**`master`**. Stack: Go (echo, Cobra, GORM over dqlite/SQLite), embedded
React/Vite UI, Prometheus, Docker/Podman/Kubernetes runtimes. Everything
builds/tests inside the `caesium-builder` images via `just` targets — host
`go build` is discouraged (`CLAUDE.md`).

**Paths below use `$REPO_ROOT`** — the caesium checkout root. Resolve it
once at the start of a wave and export it so every `$REPO_ROOT/...` snippet
below works on any contributor's machine, not just the author's:

```sh
export REPO_ROOT="$(git rev-parse --show-toplevel)"
```

---

## Phase 1: Analyze

**Goal**: enumerate the work that's eligible for this wave.

1. `git fetch origin && git checkout master && git pull --ff-only origin master`. Sub-agents get stale plan-doc state otherwise.
2. Read the plan doc end-to-end with `Read`. Identify these structural sections (the `draft-exec-plan` template):
   - Top-level summary (line 1-15ish): for "Last updated:" and intro
   - `## Source-Of-Truth Note`
   - `## Progress (as of YYYY-MM-DD)`: the dashboard
   - `## Streams`: lettered streams (A, B, C, …) each with `- [ ]` / `- [x]` items
   - `## Harness Strengthening`: `H-N` items (test infra / golangci / justfile / CI)
   - `## Navigational / Organizational Improvements`: `N-N` items (docs / roadmap / README)
   - `## Sequencing & Dependencies`: cross-stream order constraints
   - `## Verification (Run For Every PR)`: the verify chain
   - `## Acceptance Criteria`: completion gates
3. Build the eligible-items list: every `- [ ]` (unchecked) item that is **not** explicitly marked deferred / placeholder / "revisit when X" / "optional" in its description. The plan doc explicitly tags these — do not pick them up unless the user told you to.
4. For each eligible item, capture:
   - Item id (e.g. `H-5`, `N-3`, `A4`)
   - Item description (full text of the bullet)
   - Stream/section (Harness Strengthening, Navigational, Stream A, etc.)
   - Files implied by the description (use `grep` / `Read` on the named paths)
   - Any cross-references (other items it blocks/depends on)
5. Read the plan's source of truth (`docs/roadmap.md` section, or `pkg/jobdef/definition.go` for a schema plan). Cross-reference: an item that flips a roadmap section to Shipped, or closes a `> Status:` banner on a design doc, is high-priority for the wave.
6. Read `## Acceptance Criteria`. Note which are already met and which the eligible items would close out.

**Output of phase 1**: a structured candidate list with item ids, file paths, and acceptance-criteria mapping.

---

## Phase 2: Divide & pick models

**Goal**: group eligible items into N parallel streams that don't collide, and pick a model per stream.

### Step 2a: Build a file-ownership matrix

For each eligible item, list the files it will touch. For frequently-shared caesium files, note the **kind** of edit (which determines whether two streams can touch it in parallel):

| Shared file | Edit shape | Parallel-safe? |
|---|---|---|
| `go.mod` | append a `require` line | usually yes (different lines) — but pair with `go.sum` below |
| `go.sum` | regenerated hashes | **NO** — two dep-adding streams always conflict; resolve by re-running `go mod tidy`, never hand-merge |
| `pkg/jobdef/definition.go` | struct field (TWO sites: `Step` + inner `rawStep`) + `Validate()` rule | **NO** — true-conflict file; one stream only, or sequence |
| `internal/models/models.go` | append `&Model{}` to the order-sensitive `All` slice | yes if different lines (but order matters at runtime) |
| `pkg/db/db.go` + `pkg/db/router.go` | hot-table registration (`hotPathModels()` + `hotTables`) | one stream owns the hot-table change |
| `api/rest/bind/bind.go` | `g.METHOD(...)` route line + a controller import | route lines yes; the **import block** conflicts |
| `cmd/execute.go` | append `<pkg>.Cmd` to the `cmds` slice + import | yes (different lines) |
| `cmd/start/start.go` | append a startup-wired subsystem goroutine | **NO** — composition root; one stream or sequence |
| `pkg/env/env.go` | append an `envconfig` field; shared `validate()` | fields yes; `validate()` conflicts |
| `internal/metrics/metrics.go` | var decl + `MustRegister` arg (TWO sites) | yes if different lines |
| `api/api.go` | extend `Start()` signature / `e.Use(...)` | **NO** — signature change is a true conflict |
| `ui/src/router.tsx`, `ui/src/lib/api.ts`, `ui/src/components/layout/Sidebar.tsx` | list/array append + import | appends yes; import blocks conflict |
| `docs/roadmap.md`, `docs/README.md` | section/bullet edit | yes if different sections |
| `CLAUDE.md`, `AGENTS.md` | prose | one stream maximum |

### Step 2b: Form streams

Group items so that within a stream:
- **Items use the same area** (one PR per stream is sensible — e.g. two items both editing `internal/eventtrigger/` belong together).
- **Items don't conflict with sibling streams' file ownership** for the same file/section per the table above.

Apply these rules:

| Rule | Action |
|---|---|
| Two streams want to write `pkg/jobdef/definition.go` (Step/Validate) | Bundle into one stream OR sequence sequentially |
| Two streams want to extend `cmd/start/start.go` composition or `api/api.go` `Start()` | Bundle OR sequence |
| Two streams want to write the same `.go` file (same func/struct) | Bundle into one stream |
| Two streams append to different lines of the same slice/list (`models.All`, `cmds`, route lines, metric vars, env fields, UI arrays) | OK in parallel — additive, rebases mechanically |
| Two streams both add a dependency (`go.mod`/`go.sum`) | OK in parallel, but flag the `go.sum` conflict for merge (regenerate, don't hand-merge) |
| Two streams want to write `AGENTS.md` / `CLAUDE.md` | Assign to one stream only |
| `N` items that just create new docs in different `.md` files | Can all go in parallel; pair related ones into one stream |

Each stream gets a Greek-letter suffix (α, β, γ, δ, ε, ζ, η) under the wave label (W3-α, W3-β, etc.). Order them in your head by intended merge-order (largest blast radius first; see Phase 7).

### Step 2c: Pick a model per stream

Default rubric:

**Use Opus** when the stream involves:
- **Security-critical paths** — Sonnet on these can stub-instead-of-solve when the dep/integration story looks hard, and the failure mode is a forgery / privilege-escalation / secret-leak hole that ships. The caesium security surface (verified):
  - `internal/auth/**` — OIDC/SAML/LDAP providers, sessions, API-key service, RBAC (`rbac.go`), scope (`scope.go`), role mapping (`rolemap.go`), hashing (`hash.go`), key generation (`keygen.go`), state cookies (`oidc/state.go`, `saml/state.go`), replay store (`saml/replay_store.go`), rate limiter.
  - `api/middleware/auth.go`, `api/middleware/auth_scope.go`, `api/middleware/csrf.go`, `api/middleware/proxy.go` — the request-time auth+RBAC+scope+CSRF gate (stubbing any = total bypass or priv-esc).
  - `api/rest/controller/auth/**` — SSO controller + admin key management.
  - `internal/jobdef/secret/**` — `secret://` env/k8s/vault resolution (Vault `TLSSkipVerify` is a MITM footgun).
  - `internal/trigger/http/auth.go` — webhook HMAC/bearer/basic verification (returns open when secret is empty by design — easy to get wrong).
  - Anything touching `crypto/subtle.ConstantTimeCompare` (swapping to `==` is a timing/forgery hole), `AuthKeyHashSecret`, or token/key RNG.
- **Concurrency / durability-critical paths** — `pkg/db/**` (the dqlite read/write split, sharded router), `pkg/dqlite/**`, anything around Raft / distributed execution mode. Writes serialize through Raft (`busy_timeout` is a no-op); a concurrent-writer pattern reintroduces the "database is locked" class.
- **Job-schema changes** that fan out across all three engines + the cache key (`pkg/jobdef/definition.go` + `internal/atom/{docker,kubernetes,podman}` + `internal/cache/hash.go`) — getting the cache key wrong silently serves stale results.
- Architecturally novel work that doesn't have a precedent in earlier waves.

**Use Sonnet** when the stream is:
- Pure documentation (`docs/*.md`, `CLAUDE.md`, `AGENTS.md`)
- Helm chart YAML (`helm/**`) or JSON/YAML fixtures
- UI work that mirrors an existing page pattern (`ui/src/features/<f>/`)
- Mechanical Go: a new model file + `models.All` registration, a new route mirroring an existing controller, adding a metric, adding a config knob, a new CLI subcommand mirroring an existing verb
- Adding new test files that mirror existing patterns

When in doubt, pick Sonnet — it's faster, cheaper, and handles most mechanical feature work in this repo. Reserve Opus for the truly hard streams (typically ≤ 1 per wave) and for the security/durability class above.

### Step 2d: Skip rules — do not pick up

Skip these even if they appear `- [ ]`:
- Items explicitly marked "(optional)" / "(placeholder)" / "(N/A)" / "revisit when …"
- Items whose description names a future runner / system / file that does not yet exist
- Items whose blast radius makes them inappropriate for parallel waves (e.g. "rename a type used across every package" — conflicts with every other change)

Document each skip in your dispatch summary.

**Output of phase 2**: a stream table with stream id, items, files, model, and merge-order rank.

---

## Phase 3: Spawn

**Goal**: launch one background sub-agent per stream, each in its own worktree.

For each stream, fill in `stream-agent-prompt.md` template (substitute every `{{PLACEHOLDER}}`) and call:

```
Agent({
  description: "<wave>-<stream> <short-desc>",
  subagent_type: "general-purpose",
  model: "opus" | "sonnet",
  name: "<wave>-<stream>-<slug>",       // e.g. "w3-alpha-event-engine"
  isolation: "worktree",
  run_in_background: true,
  prompt: <filled-in template>
})
```

**Always launch in parallel** by emitting all `Agent` tool calls in a single tool-block message. Do NOT serialize.

**Always use `run_in_background: true`** — foreground blocks the conversation for the entire wave runtime. With background you get task notifications as each completes.

After spawning, end your turn or continue with non-blocking work (creating tasks, posting status to user). Do not poll.

**Track each agent**: keep a `TaskCreate` per stream so the user has visibility. Mark in_progress on launch.

### When the user asks for codex sub-agents

If the wave invocation specifies codex sub-agents (e.g. "use the codex plugin"), use the [`stream-agent-prompt-codex.md`](stream-agent-prompt-codex.md) template (NOT the default one) and dispatch each stream into an **orchestrator-owned worktree** (see the race warning below — do NOT use `Agent({subagent_type: "codex:codex-rescue", isolation: "worktree"})` for a parallel codex wave). caesium uses codex regularly (e.g. the `codex/sso-remaining-foundation-wave` branch behind PR #200), so this path is well-trodden. Several non-obvious things differ:

- **⚠️ Worktree-auto-clean race — dispatch codex into an orchestrator-owned worktree, NOT `Agent(isolation: "worktree")`.** When you spawn codex via `Agent({subagent_type: "codex:codex-rescue", isolation: "worktree"})`, the worktree is owned by the spawned *forwarder* agent. That forwarder dispatches the detached codex job and then completes within seconds-to-minutes — and when it completes, the harness **auto-cleans its worktree if it looks unchanged**. The detached codex job frequently hasn't staged anything that early, so the worktree (judged unchanged) is removed out from under the still-starting job; the job then dies with zero work or self-rescues by copying to `/private/tmp/caesium-agent-<id>-impl`. This is a **dispatch-time race**.

  **Fix (use this as the default for codex waves) — dispatch `codex exec` directly, NOT the `codex-companion` background job manager.** The companion's `task --background` spawns a detached job-worker that, on the current codex-cli, dies instantly with `failed to load configuration: No such file or directory (os error 2)` — while the interactive `codex exec` path is unaffected (this is NOT a codex-cli version regression; the same error is recorded against much older plugin versions — see the § "codex-companion background dispatch" note below). Drive `codex exec` yourself, one detached run per stream, via a `Bash` `run_in_background` call — no `Agent` wrapper, no forwarder, no detached companion worker, nothing to auto-clean:

  ```sh
  scratch=$(mktemp -d)                                                   # scratch dir for the per-stream prompt + log files
  wt="$REPO_ROOT/.claude/worktrees/agent-<plan-slug>-<wave>-<stream>"     # ALWAYS include the plan slug — a bare agent-<wave>-<stream> collides with a prior plan's branch on the remote; keep the agent-* prefix so Phase 7.5b prunes it
  git worktree add "$wt" -b "worktree-agent-<plan-slug>-<wave>-<stream>" master   # orchestrator owns this worktree
  pf="$scratch/codex-<stream>-prompt.md"                                 # write the filled stream-agent-prompt-codex.md body here (Write tool)
  # ...then run EACH stream as its OWN Bash run_in_background call. ⚠️ cd INTO the worktree first —
  # a missing cd runs codex in the MAIN checkout (it edits master's tree, not the branch):
  #   cd "$wt" && cat "$pf" | codex exec --skip-git-repo-check > "$scratch/codex-<stream>.log" 2>&1
  ```

  `codex exec` reads the prompt from stdin and runs the agent to completion in the worktree cwd (with the user's configured model/effort). You get one background-task completion notification per stream; on completion, inspect the worktree `git status --short` + the log's final report, then verify + publish in Phase 4.5. Confirm each run's cwd once right after dispatch (`lsof -a -p <pid> -d cwd` or `git -C "$wt" status`) — a mis-targeted run is the one dispatch failure mode left. The companion-specific guidance below (`status`/`resume`/hung-job/pid) applies ONLY if you fall back to the companion; with `codex exec`, the harness's background-task lifecycle replaces it.

  **(Companion fallback only — the `codex exec` default above needs none of this.)** If you fall back to `codex-companion`, resolve its path (`companion=$(ls ~/.claude/plugins/cache/openai-codex/codex/*/scripts/codex-companion.mjs | sort -V | tail -1)`), do all streams' `git worktree add` + `node "$companion" task --background` spawns in one Bash batch, then poll the codex state dir for `$wt` (enumerate the newest `~/.claude/plugins/data/codex-openai-codex/state/agent-*` dirs and read their `state.json`, or `cd "$wt" && node "$companion" status <job-id> --json`) — on that path there is **no completion notification**, so the `state.json` poll (pid alive + `phase`/`updatedAt`) is the only signal. Either way the PR branch is the `worktree-agent-<plan-slug>-<wave>-<stream>` you created; Phase 4.5 publishes from `$wt`; Phase 7.5b prunes `agent-*` worktrees by PR state.

- **codex-rescue is a thin forwarder.** It dispatches one codex job via `codex-companion task --background --write` and exits, regardless of "stay alive and poll" instructions. The orchestrator MUST poll codex job status itself. Resolve the companion path at runtime: `companion=$(ls ~/.claude/plugins/cache/openai-codex/codex/*/scripts/codex-companion.mjs | sort -V | tail -1)`. Job state lives at `~/.claude/plugins/data/codex-openai-codex/state/agent-<id>-<hash>/state.json`; read those directly for `status`/`phase`/`updatedAt`. `cancel <job-id>` from the worktree cwd to stop a stuck job.

- **Hung-job heuristic.** A codex job whose `phase=running` but `updatedAt` hasn't moved in 5+ minutes is stuck on an upstream rate wall. Cancel and re-dispatch — it will not recover.

- **Codex pid is the source of truth for "is it actually running".** The `state.json` `status=running` field is NOT updated when the codex job's pid dies. After ~5+ min of no log activity AND no `updatedAt` change, check `ps -p <pid>` (the pid is in `state.json`'s `job.pid`). If the pid is dead, the job is finished — consult the three-way disposition below.

- **CODEX SANDBOX BLOCKS NETWORK EGRESS *AND* CAN'T RUN caesium's CONTAINERIZED VERIFY CHAIN — orchestrator publishes AND verifies by default.** Two compounding facts: (1) codex's sandbox blocks DNS for `github.com` and doesn't surface the host `gh` token, so `git push` / `gh pr create` deterministically fail; (2) caesium's entire verify chain (`just lint`, `just unit-test`, `just integration-test`) runs **inside Docker containers**, and the codex sandbox has no Docker access — and a bare host `go test ./...` would need the dqlite CGO libs (libuv/lz4/sqlite) that live only in the builder image. So a codex stream agent can do essentially **no** meaningful verification itself beyond `gofmt`. The codex stream prompt (`stream-agent-prompt-codex.md`) MUST tell the agent to implement + `gofmt` its changed files + stage + STOP. The orchestrator runs the **full** verify chain at Phase 4.5b from outside the sandbox (this is heavier than a Rust repo's `cargo check` — it's the real `just lint` + `just unit-test`, plus the Phase 6.5 integration gate). Letting the codex agent try to push/test anyway wastes 1–3 minutes per agent on doomed retries and produces inconsistent intermediate states.

- **Codex API session drop — three-way disposition.** Codex jobs sometimes terminate with `status=failed` (`previous_response_not_found`, an upstream 400) or just stop silently with `state.json` never updating. Inspect `state.json` + `ps -p <pid>` to detect death; then `git status` in the worktree to assess what landed. Three dispositions, in order of cost:

  1. **Resume** (cheap; first attempt when uncertain). Re-attach to the same codex thread so the agent picks up its context and finishes staging + the plan-doc edit. Best when: last phase was `implementing`, worktree has substantial in-scope work, and the implementation looks correct in shape. Use [`codex-resume.sh`](codex-resume.sh):
     ```sh
     bash $REPO_ROOT/.claude/skills/exec-plan-wave/codex-resume.sh <dispatching-agent-id>
     # Optional: --prompt "specific continuation text" for non-default guidance
     ```
     The helper reads the agent's `state.json`, refuses if pid is still alive, and dispatches `codex-companion task --background --write --resume <threadId>` from the agent's worktree.
  2. **Publish-as-is**. If the implementation looks substantially complete (most expected files modified, plan-doc edited or close), proceed straight to Phase 4.5 publish — the orchestrator's verify there is the real gate anyway.
  3. **Re-dispatch with a focused fix-up prompt**. If the agent took a wrong design path or never produced output (nothing to resume), re-dispatch with a fresh prompt that points at the existing worktree state: `node "${companion}" task --background --write -- "<prompt>"` from the worktree. Don't resume — resume would continue the wrong design.

  **Rubric**: resume for "right track, didn't finish"; publish for "right track, basically done"; re-dispatch for "wrong track or empty". If a resumed agent also dies without progress (two strikes), fall back to publish-as-is or re-dispatch.

- **Codex worktree placement.** Some codex runs end up in `/private/tmp/caesium-agent-<id>-impl` instead of the orchestrator-provided worktree — codex makes its own writable copy when the worktree's git metadata isn't reachable from the sandbox. Symptom: the orchestrator-provided worktree is empty, but `/private/tmp/caesium-agent-<id>-impl` has the staged work on `master`. Bridge in Phase 4.5: `cd /private/tmp/caesium-agent-<id>-impl`, `git checkout -b worktree-agent-<id>`, commit there, `git push origin worktree-agent-<id>` (the temp clone's `origin` points at the host repo, so the branch lands locally), then from the host repo `git push origin worktree-agent-<id>` (host's `origin` is github), then `gh pr create`.

---

## Phase 4: Wait

**Goal**: receive completion notifications and capture each stream's PR URL.

When a `<task-notification>` arrives:

1. Mark the task complete (`TaskUpdate`).
2. Extract from the agent's result message:
   - PR URL
   - Files changed / items ticked
   - Verify-chain status (which of `just lint` / `just unit-test` / `just integration-test` passed; any conditional gate run)
   - Any flagged anomalies
3. Brief 1-2 sentence ack to the user.

If an agent reports verify-chain failure or didn't push a PR, record it as a Phase 8 followup. Do not abort the wave — other streams may still succeed.

**Sanity-check the agent's anomaly claims before accepting them.** Agents sometimes mis-attribute a failure. When a report cites a specific failure mode ("`just lint` fails on a pre-existing golangci issue in `foo.go`"), run the same command in the agent's worktree and read the actual output before deciding the failure class. A 30-second sanity check catches mis-attributed anomalies that would otherwise propagate into the merge train.

**Verify dep-claim / stub conclusions when an agent stubs.** If an agent reports "stubbed X because adding dep Y would conflict", grep `go.mod` / `go.sum` for Y before accepting the stub — Y may already be present transitively. **For security-critical paths the orchestrator treats stubs as bugs and immediately re-dispatches as a fix-forward**, even if the sonnet agent thought stubbing was acceptable: a stubbed `validateSignature` / `ValidateKey` / `HasRole` / `CheckScope` / `recordAssertion` / nonce-compare is a forgery, privilege-escalation, or replay hole. There is no "stub-and-document" exception on the auth/secret surface.

When all streams report in, proceed to Phase 4.5 (codex sub-agents) or Phase 5 (general-purpose sub-agents).

---

## Phase 4.5: Orchestrator publish + verify (codex sub-agents only)

**Goal**: take each codex agent's staged work, run the real verify chain on it, and publish it as a PR. Skip this phase entirely for general-purpose sub-agents — those publish + verify from inside the agent.

The codex sandbox blocks network egress AND Docker, so the codex agent should have stopped after `gofmt` + staging per the codex prompt template. The orchestrator verifies, commits, pushes, and creates the PR from outside the sandbox.

For each codex stream:

### Step 4.5a: Locate the worktree

**With the `codex exec` default (Phase 3), the worktree is the standard `agent-<plan-slug>-<wave>-<stream>` you created — the first row below.** The `/private/tmp/caesium-agent-<id>-impl` and rsync-recovery shapes are `codex-companion`-fallback artifacts (codex making its own copy when the sandbox can't reach the worktree's git metadata) and do NOT occur with `codex exec`; the `<id>` in those rows is the codex *job* id, not the worktree name.

| Symptom | Worktree location | How to detect |
|---|---|---|
| Standard (`codex exec`) | `$REPO_ROOT/.claude/worktrees/agent-<plan-slug>-<wave>-<stream>/` | `git -C "$wt" status --short` lists modified/untracked files |
| /private/tmp fallback | `/private/tmp/caesium-agent-<id>-impl/` | The orchestrator's worktree is clean/absent; `ls -d /private/tmp/caesium-agent-<id>*` finds the temp clone (on `master`) |
| Already committed locally | Same as above, but `git log master..HEAD` is non-empty | Codex's `git commit` succeeded but the push failed; commit is preserved |
| Worktree never materialized (rsync recovery) | `.claude/worktrees/agent-<id>/` exists as a plain dir (not a git worktree) | `git worktree list` lacks `agent-<id>`; `git -C <path> status` errors; the dir is a flat copy of the repo minus `.git/` |

For the /private/tmp case, the temp clone is on `master`. Its `origin` points at the host repo, so pushing from /private/tmp lands the branch on the host; pushing from the host then reaches github. For the rsync-recovery case, create a fresh worktree off master (`git worktree add /tmp/caesium-<wave>-<stream>-bridge -b worktree-agent-<id> master`), `diff -rq` the rsync'd directory against the host repo (filtering `.git`, build caches, `node_modules`, `ui/dist`), copy each modified/new file into the bridge worktree, then verify + stage + push from the bridge.

### Step 4.5b: Run the REAL verify chain (this is the gate codex couldn't run)

Because codex ran no containerized tests, the orchestrator runs them now, from the worktree, before publishing:

```sh
cd <worktree>
just lint              # go fmt + go vet + golangci-lint — catches compile + lint regressions
just unit-test         # go test -race ./... — catches logic regressions and data races
```

If `just lint` or `just unit-test` fails: stop, read the error, fix forward in the worktree (or re-dispatch a focused fix-up codex job with the error pasted in). Don't publish broken code. The Phase 6.5 integration gate runs after publish as the end-to-end check; `just integration-test` may also be run here if the change is risky enough to want it pre-publish.

If they pass: codex's work is credible enough to publish.

### Step 4.5c: Commit, push, create PR

Standard worktree case:

```sh
cd $REPO_ROOT/.claude/worktrees/agent-<plan-slug>-<wave>-<stream>
git add -A
git status --short
git commit -m "$(cat <<'EOF'
<Imperative subject> (<plan-slug> W<n>-<greek>)

<2-3 sentence body describing what changed and why>

{{MODEL_TRAILER}}
EOF
)"
git push -u origin "$(git symbolic-ref --short HEAD)"
```

For the /private/tmp fallback: `cd /private/tmp/caesium-agent-<id>-impl`, `git checkout -b worktree-agent-<id>`, commit, `git push origin worktree-agent-<id>` (lands on the host), then from the host repo `git push origin worktree-agent-<id>` (reaches github). For the already-committed case: just `git push -u origin "$(git symbolic-ref --short HEAD)"`.

Then create the PR from the host repo:

```sh
cd $REPO_ROOT
gh pr create --title "<Imperative subject> (<plan-slug> W<n>-<greek>)" --head <branch-name> --body "$(cat <<'EOF'
## Summary
<bullets from agent's suggested PR body>

## Test plan
- [x] just lint (orchestrator)
- [x] just unit-test (orchestrator)
- [ ] just integration-test — runs as the Phase 6.5 gate before merge
- Codex agent ran gofmt only; sandbox blocks Docker so the orchestrator ran the verify chain.

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

### Step 4.5d: When orchestrator publish would BE wrong

If the codex agent's final report says it **stopped without staging because of a real implementation blocker** (not a sandbox blocker), do NOT publish. Re-dispatch a focused fix-up codex job pointing at the existing worktree state, OR escalate to the user. If `git status --short` shows no changes and the report doesn't say what it did, the agent never staged — inspect the codex log before re-dispatching.

---

## Phase 5: Review-comment resolution

**Goal**: address every PR review comment with either a fix-and-reply or an explain-and-reply. No comment goes unanswered.

### Step 5a: Enumerate comments

For each PR raised:

```sh
# Inline review comments (the actionable ones)
gh api "repos/caesium-cloud/caesium/pulls/<pr>/comments" \
  --jq '.[] | "\(.id)\t\(.user.login)\t\(.path):\(.line // .original_line)\t\(.body[0:120])"'

# Top-level review summaries
gh api "repos/caesium-cloud/caesium/pulls/<pr>/reviews" \
  --jq '.[] | "\(.id)\t\(.user.login)\t\(.state)\t\(.body[0:120])"'

# Issue-level comments (usually bot status notifications)
gh api "repos/caesium-cloud/caesium/issues/<pr>/comments" \
  --jq '.[] | "\(.id)\t\(.user.login)\t\(.body[0:120])"'
```

Filter:
- Bot status notifications ("usage limits reached", "daily quota", etc.) — **skip**, do not reply.
- Empty review bodies — skip.
- Inline comments and substantive review summaries — actionable.

**Review bots active on this repo: `gemini-code-assist` and `greptile`.** Project convention (per repo memory) is to address their inline comments before building further. Greptile-style summaries often live in the **issue-comment** body (a confidence score + per-file table + a "Comments Outside Diff" section) rather than the review body — pull both:
```sh
gh pr view <pr> --json comments --jq '.comments[] | select(.author.login|test("greptile|gemini")) | .body'
gh api repos/caesium-cloud/caesium/pulls/<pr>/comments --jq '.[] | select(.user.login|test("greptile|gemini")) | "[\(.path):\(.line // .original_line)] \(.body)"'
```
Treat a high-confidence **P1** finding (real bug — security, durability, data-loss) as merge-blocking, same disposition as a real CI/gate failure. Note lower-confidence/style findings as followups.

**Timing — these bots post ASYNC**, often AFTER a fast orchestrator publish (Phase 4.5). So **re-check at Phase 6.5/7** (just before the gate-merge), when every PR has been open a few minutes — not only right after publish. If you discover post-merge that a bot posted a real P1 you merged past, audit the merged PRs' comments and fix-forward the real findings as a dedicated follow-up batch.

### Step 5b: Disposition rubric

| Comment type | Disposition |
|---|---|
| Suggestion with explicit code block (`suggestion` markdown) | Apply verbatim if it compiles / passes `just lint`; otherwise apply the spirit |
| Mechanical fix (typo, missing import, description sync, missing `models.All`/`Register()` registration) | Apply |
| "This is dead code" / "never called" | Verify with `grep`; if confirmed dead, delete |
| Description ↔ implementation drift | Update description |
| "This boundary/route is unprotected" / "missing RBAC policy entry" | Investigate via `grep` of `internal/auth/rbac.go` `endpointPolicy`; a new protected route with no policy entry fails CLOSED (good) but may be under/over-protected — verify the intended role |
| Architectural concern requiring design judgment | Reply explaining the trade-off; do not silently apply |

If you decline to apply, **always reply with the reason**. Don't leave bot comments unanswered.

### Step 5c: Spawn review sub-agents

One review-resolve sub-agent per PR with comments. Use the existing worktree (do NOT spawn fresh — the branch is locked there). Use `review-agent-prompt.md`, fill in: PR number, branch name, existing worktree absolute path, comment id + author + file:line + body for each comment, and per-comment guidance (apply / investigate / explain).

Launch in parallel via a single tool-block. `run_in_background: true`.

### Step 5d: Wait for replies

When each review agent completes, verify it (a) pushed a follow-up commit (or none if all comments were declines), and (b) replied to every comment:

```sh
gh api -X POST "repos/caesium-cloud/caesium/pulls/<pr>/comments/<comment_id>/replies" \
  -f body="<reply text>"
```

Cross-check by spot-fetching one or two comments after they finish.

---

## Phase 6: CI debugging

**Goal**: classify each failing CI check as flake-or-real, recover flakes by rerun, escalate real failures.

caesium CI is `.github/workflows/ci.yml` — 13 container-based jobs mapping to `just` targets, matrixed amd64 (`ubuntu-24.04`) + arm64 (`ubuntu-24.04-arm`). It triggers on push to **all** branches (`branches: ['**']`) and on PR to `master`, so a `worktree-agent-*` branch fires the full workflow on push, and a PR adds a second near-identical run — **dedupe by `event`** when reading status. The `builder`/`builder-arm64` jobs are the roots (they build + upload the builder image); if a builder job fails, everything downstream is skipped — **root-cause builder first**.

### Step 6a: Enumerate failures

```sh
gh pr checks <pr> | grep -i fail
# or, deduped by event:
gh run list --branch <pr-branch> --json databaseId,status,conclusion,event --jq '.[] | select(.event=="pull_request")'
```

For each failing check, fetch the log:
```sh
gh run view <run-id> --json jobs --jq '.jobs[] | select(.conclusion=="failure") | {name, databaseId}'
gh run view <run-id> --log-failed
```

### Step 6b: Flake vs real rubric

A failure is a **pre-existing flake** if ALL hold:
- The same check is failing on `master` HEAD (`gh api repos/caesium-cloud/caesium/commits/master/check-runs`).
- The same check failed on a recently-merged PR at merge time.
- The PR's diff cannot logically affect the failing test path (e.g. a docs PR breaking a Go test).

A failure is a **PR-specific flake** if only this PR fails it, the diff can't logically affect that path, and the test name suggests a timing dependency.

A failure is **real** otherwise — the PR's changes plausibly cause it. Don't bypass. Notes:
- `unit-test` runs with `-race`. **Race-detector failures are real data races, not flakes.**
- Failures matrixed by arch: check **both** `unit-test` and `unit-test-arm64` — an arch-specific bug fails only one.
- The two historical flakes are **already fixed in code — do NOT re-diagnose**: (1) dqlite "database is locked" (concurrent-writer collision; fixed by the read/write connection split in `pkg/db/db.go`, #188; contention classification in `pkg/dqlite/contention.go`); (2) OIDC state-cookie tamper (~1/16; fixed in 23d72c2/#206 by mutating the first base64 char in `tamperCookieValue`, `internal/auth/oidc/provider_test.go`). If you see these patterns, the test is deterministic now — a failure is a real regression, not the old flake.

### Step 6c: Recovery actions

| Classification | Action |
|---|---|
| Pre-existing flake on master | Note it; do **not** rerun. Note in final report; do not block merge |
| PR-specific timing flake | `gh run rerun <run-id> --failed`. If it fails twice, escalate as real |
| Real regression | Stop the merge for this PR. Report root cause. Ask user how to proceed |

### Step 6d: Merge-gate check (caesium has NO required status checks)

Unlike a CI-gated repo, caesium's `master` protection requires **zero** status checks — confirm with:

```sh
gh api "repos/caesium-cloud/caesium/branches/master/protection/required_status_checks" --jq '.contexts'
# expected: []
```

So a red CI run does NOT mechanically block merge, and a green one does NOT permit it. The enforced gate is **a CODEOWNER approval** (`require_code_owner_reviews: true`, CODEOWNERS `@rocketbitz @RohanDalton`, `enforce_admins: false`). Practical consequences:
- A failing CI check is advisory — but you should still NOT merge a PR with a **real** regression (Phase 6b "real"). Treat real CI failures and real integration-gate failures (Phase 6.5) as merge-blocking by policy, even though GitHub won't enforce it.
- The merge itself is gated on a CODEOWNER review. If you are running as a repo admin, `gh pr merge` succeeds without the review (`enforce_admins: false`). If not, GitHub blocks the merge — surface it to the user to approve/merge (Phase 7b). Re-check protection at runtime; it can change.

### Step 6e: CI darkness as a wave anomaly

If `gh pr checks <pr>` returns "no checks reported" for **every** PR in the wave, do not silently proceed — the Phase 6.5 integration gate is then your only safety net. Confirm with `gh run list --limit 5` that recent master pushes also have no runs (rules out a transient outage), check `.github/workflows/ci.yml`'s `on:` still matches the branches, and surface it as an explicit anomaly in the final report. Two consecutive dark waves should pause until the user investigates.

---

## Phase 6.5: Pre-merge integration gate (scope-aware)

**Goal**: for PRs whose diff plausibly affects runtime behavior, validate end-to-end against a real caesium server before merging. `just unit-test` does NOT compile `test/` (it's behind `//go:build integration`) and never exercises real HTTP / scheduler / engine / sharded-DB paths — this gate does.

caesium's gate is plain `just` targets (Docker, and kind/podman for those tiers) — **no sudo, no QEMU, no golden overlay**. It is much lighter than a kernel/cluster gate. Run it from the **main repo** (not a worktree) so the just targets mount the right working tree.

### Step 6.5a: Decide which tier(s) the PR needs

Run `gh pr diff <pr> --name-only` and match against:

| PR diff touches | Gate tier (run in addition to baseline) |
|---|---|
| Only `docs/**`, `*.md`, `brand/**`, `LICENSE`, `.claude/**` | **none** — docs-only, skip the gate |
| `internal/**`, `api/**`, `pkg/**`, `cmd/**`, `test/**`, `go.mod`, `go.sum`, `build/Dockerfile*` | **baseline**: `just lint` + `just unit-test` + **`just integration-test`** (Docker engine, `CAESIUM_DATABASE_SHARDS=4`) |
| Kubernetes-engine code (`internal/atom/kubernetes/**`), distributed/Raft execution-mode code, `helm/**` | baseline + **k8s tier**: `just helm-lint` + `just helm-template` + the kind-based k8s integration run (CI's `helm-integration-test`; locally approximated by `just k8s-distributed` + `just helm-test`) |
| Podman-engine adapter (`internal/atom/podman/**`) | baseline + **`just integration-test-podman`** |
| `ui/**` (incl. `ui/embed.go`) | **ui tier**: `just ui-lint` + `just ui-test` + **`just ui-e2e`** (Playwright). Add baseline too if Go API was also touched |

When in doubt, run the baseline gate — a false positive wastes a few minutes; a false negative ships a broken merge. For mixed diffs, run every matched tier. Document each gate decision in the wave's final report (gated PRs: list with tier; skipped PRs: list).

### Step 6.5b: Preconditions

- **Docker daemon up**: `docker info >/dev/null 2>&1` (or the configured `CAESIUM_CONTAINER_CLI`). The baseline + ui-e2e tiers need it.
- **No stale server container**: `just integration-up` does `container rm -f caesium-server-test` first, but if a prior run was killed mid-flight, `just integration-down` (and `docker rm -f caesium-server caesium-server-podman` for the other tiers) clears it.
- **k8s tier**: a reachable cluster — `kubectl cluster-info`. `just k8s-distributed` is verified on Docker-Desktop K8s and uses a local registry on port **5050** (dodging macOS AirPlay on 5000); CI uses `kind`. If no cluster is reachable, **stop and ask the user** rather than provisioning one autonomously.
- **podman tier**: the user podman socket active (`systemctl --user enable --now podman.socket`). If absent, stop and ask.

The builder/builder-full images are cached (`just builder`/`builder-full` skip if present); the first `just integration-test` of a wave also builds the `:latest-test` DinD image. Budget several minutes for the first run.

### Step 6.5c: Run the gate per PR

Run from the main repo against the PR's branch. `gh pr checkout` fails with "branch already used by worktree" for any worktree-driven wave, so use a temp local branch tracking origin:

```sh
git checkout master && git pull --ff-only origin master
git fetch origin <pr-branch>
git checkout -B gate-<pr> origin/<pr-branch>

# Baseline (always, for a code PR):
just lint
just unit-test
just integration-test          # on failure it dumps `container logs caesium-server-test`, then exits 1

# Conditional tiers per the table:
just integration-test-podman   # podman-engine PRs
just helm-lint && just helm-template   # helm / k8s PRs
just ui-lint && just ui-test && just ui-e2e   # ui/** PRs

git checkout master            # return for the merge step
```

**Pass criterion**: each `just` target exits 0. `just integration-test` exiting 0 means `go test ./test/ -tags=integration` passed against the live sharded server. If two PRs in the same wave are both gated, run the gate **sequentially** per the merge order — the server container name (`caesium-server-test`) and Docker daemon are global, so parallel integration runs collide.

### Step 6.5d: Failure disposition

| Failure mode | Disposition |
|---|---|
| `integration-test` reports a test `FAIL` on a scenario a sibling PR ran cleanly earlier in the wave | Real regression in this PR's diff. **Stop the merge for this PR.** Capture the failing test output + the dumped `caesium-server-test` logs. Report root cause. Ask user to fix-forward, revert, or override |
| `just lint` / `just unit-test` fails at the gate but the agent reported it passing | The agent's worktree was stale or it mis-reported. Re-run in the gate; if real, fix-forward; if a flake, note it |
| Stale `caesium-server-test` / `caesium-server` / `caesium-server-podman` from a prior run blocks `up` | `just integration-down` (+ `docker rm -f caesium-server caesium-server-podman`), then retry once |
| First `integration-test` of the wave fails on an image-build / pull error (alpine pull, DinD `dockerd` slow to start) | Likely environment, not code. Retry once; if it persists, surface as an anomaly — don't block the PR yet |
| `ui-e2e` Playwright failure | Read the trace/screenshot artifacts; UI e2e has 2 retries built in. A consistent failure on a UI-touching PR is real |
| k8s `helm-integration-test` / `k8s-distributed` fails to reach a ready cluster | Treat as infra; `just k8s-down` / clean the kind cluster and retry once. If it persists, stop and ask the user (could be a cluster-state issue) |

The orchestrator does NOT silently retry on **real** failures. The gate's value is catching the regression `just unit-test` structurally cannot see (it never compiles `test/`).

### Step 6.5e: Order of operations vs. Phase 7

Run the gate **per PR, immediately before its `gh pr merge`** in the merge order chosen at Phase 7a:

```
Phase 7a  →  sort merge order
Phase 6.5 →  integration gate for PR #1
Phase 7b  →  gh pr merge PR #1
Phase 7d  →  pull master
Phase 6.5 →  integration gate for PR #2 (against master + PR #2 rebased on top)
Phase 7b  →  gh pr merge PR #2
…
```

Each gated PR boots a fresh server against the post-prior-merge `master + this PR` state.

---

## Phase 7: Merge

**Goal**: merge all PRs into master in the order that minimizes rebase/conflict pain.

### Step 7a: Sort merge order

Highest-priority (most-disruptive shared-file edit) first:
1. The PR that touches the most-shared, true-conflict file at its most-disruptive point — `pkg/jobdef/definition.go` (Step/Validate), `internal/models/models.go` (the `All` slice), `cmd/start/start.go` / `api/api.go` (composition), or a `go.mod`/`go.sum` dependency add.
2. PRs touching the same shared file but a different section (route lines, metric vars, env fields, `models.All` appends).
3. Independent PRs (docs-only, helm-only, UI-only with no shared Go file).

### Step 7b: Merge each PR

```sh
gh pr merge <pr> --squash --delete-branch
```

**Merge gate (caesium-specific):** because `require_code_owner_reviews: true` and `enforce_admins: false`:
- If you're running as a repo admin, the squash-merge succeeds without an explicit review.
- If GitHub returns `Pull request review required` / `not authorized to merge`, the PR needs a CODEOWNER (`@rocketbitz` / `@RohanDalton`) approval. **Stop for this PR and surface it to the user** to approve or admin-merge — list it under "Followups for the user". Don't try to bypass with `--admin` unless the user has told you that's allowed. (Note: GitHub forbids approving your own PR, so if all wave PRs are authored under the user's account, a second reviewer or an admin-merge is genuinely required.)

Treat `failed to delete local branch ... used by worktree at ...` as **success** (the PR merged; only local cleanup failed). Do NOT panic or force-remove the worktree.

After each merge:
1. `git checkout master && git pull --ff-only origin master`
2. Verify the merge SHA: `gh pr view <pr> --json mergedAt,mergeCommit --jq '{mergedAt, sha: .mergeCommit.oid[0:8]}'`
3. Check the next PR in line.

### Step 7c: Conflict resolution

If `gh pr merge` returns `Pull Request has merge conflicts`:

1. cd to the PR's worktree (`$REPO_ROOT/.claude/worktrees/agent-<id>` for general-purpose streams, `agent-<plan-slug>-<wave>-<stream>` for codex streams).
2. `git fetch origin master && git merge --no-commit --no-ff origin/master`
3. `grep -n "<<<<<<< \|>>>>>>> \|^=======$" <conflicted_files>` to find conflict regions.
4. Resolve via `Read` + `Edit`. Common caesium patterns:

| Conflict | Resolution |
|---|---|
| Two streams ticked adjacent checkboxes in the plan doc | Take both ticks + both per-item notes |
| Two streams added entries to the same `docs/roadmap.md` / `docs/README.md` list | Combine both list items in the intended order |
| **`go.mod` `require` collision** | Hand-resolve the conflict markers to the **union** of both branches' `require` lines (a go.mod conflict is a simple line union; never `git checkout --ours/--theirs go.mod`, which silently drops the other side's dep) |
| **`go.sum` conflict** (two streams added deps) | Do NOT hand-merge `go.sum`. AFTER resolving `go.mod` to the union (row above), drop the conflicted `go.sum` and regenerate it from the merged `go.mod` in the builder image: `git checkout --theirs go.sum 2>/dev/null \|\| true; docker run --rm -v "$PWD":/bld/caesium -w /bld/caesium caesiumcloud/caesium-builder:latest-full sh -c 'go mod tidy'`. `go mod tidy` rebuilds `go.sum` to match the unioned `go.mod`; commit the regenerated file |
| **`internal/models/models.go` `All` slice** | Take both appended models, but preserve **dependency order** (parent/FK-target before child) — re-check the comment in the file. A wrong order breaks AutoMigrate at runtime, not compile time |
| **`api/rest/bind/bind.go`** (both added routes + imports) | Union the route lines (group by topic block) and union the import block (alphabetized). The compile validates every import resolves |
| **`cmd/execute.go` `cmds` slice** | Union both `<pkg>.Cmd` entries + both imports |
| **`internal/metrics/metrics.go`** | Union both branches' var declarations AND both `MustRegister(...)` args — a metric in the var block but not `Register()` never appears at `/metrics` |
| **`pkg/env/env.go`** | Union both appended fields; if both edited `validate()`, take both checks |
| **`pkg/jobdef/definition.go`** | Take both struct fields, and add each to BOTH the exported `Step` and the inner `rawStep` in `UnmarshalYAML` (the dual declaration), plus any `Validate()` rules from each side |
| **Interface-method addition cascade** (one branch added a method to an interface; another added an implementer) | NOT a textual conflict — surfaces as a `go vet`/build error ("does not implement"). After resolving textual conflicts, run `just lint`; add the missing method to the new implementer |

5. Run `just lint` in the worktree (its `go vet ./...` step compiles the whole module and catches the import / interface / missing-field issues a clean textual merge hides) and, for a runtime PR, the relevant `just unit-test` or the Phase 6.5 gate.

   **MANDATORY conflict-marker re-check** before `git commit`:
   ```sh
   git diff --cached | grep -E '^[+-]<<<<<<<|^[+-]>>>>>>>|^[+-]=======$' && { echo "ABORT: conflict markers still staged"; exit 1; } || echo "OK: no conflict markers"
   ```
   A non-empty grep means an `Edit` silently refused (commonly because the file was inspected via `Bash` `sed`/`grep`/`cat` but never `Read` — `Edit` requires a session-local `Read` of the exact file path). Recovery: `Read` the specific file path in the session, then re-attempt the `Edit`, then re-run the marker check.

6. `git add <files> && git commit -m "Merge origin/master — <one-line resolution description>" --no-verify`. Do NOT chain commit/push/merge across newlines without `&&` — a failed marker-check in the middle does not abort a newline-separated commit. Either chain with `&&` or split into separate Bash calls and verify each exit code.
7. `git push origin <branch>`
8. Wait ~5 seconds for GitHub to re-evaluate `mergeStateStatus`, then retry `gh pr merge`.

If conflict resolution requires content choices that aren't derivable from either side (a true editorial decision), stop and report.

### Step 7d: Sync local master between merges

Always `git checkout master && git pull --ff-only origin master` after each merge so subsequent conflict resolutions are against current master.

---

## Phase 7.5: Worktree cleanup

**Goal**: free disk by pruning worktrees whose PRs have merged. Each agent worktree carries a checked-out tree plus Go build/module caches; many waves of orphaned worktrees fill the disk.

Runs after Phase 7's merge train completes and **before** Phase 8 (a stale worktree could otherwise leak into Phase 8's verification sweep).

### Step 7.5a: Prune this wave's merged worktrees

For each merged PR's worktree path:

```sh
git worktree unlock "$path" 2>/dev/null    # silent no-op if not locked
git worktree remove --force "$path"
```

`--force` is required because the runtime marks each worktree locked at spawn; unlock first, then remove. The remove succeeds even when the branch is "busy" from `gh pr merge --delete-branch`'s perspective — that warning is about local-branch ref cleanup, not the worktree directory.

### Step 7.5b: Sweep stragglers from prior waves

`.claude/worktrees/agent-*` accumulates orphans (codex silent-deaths, crashed sub-agents, manual worktree calls that never raised PRs). Enumerate every `agent-*` and prune those whose branch's PR is closed/merged on origin:

```sh
cd $REPO_ROOT
for path in $REPO_ROOT/.claude/worktrees/agent-*; do
  [ -d "$path" ] || continue
  branch=$(git -C "$path" symbolic-ref --short HEAD 2>/dev/null) || branch=""
  # Skip THIS wave's still-in-flight worktrees (tracked in orchestrator state)
  case "$path" in *agent-<id1>|*agent-<id2>|...) continue ;; esac
  if [ -z "$branch" ]; then
    git worktree unlock "$path" 2>/dev/null
    git worktree remove --force "$path"
    continue
  fi
  state=$(gh pr list --head "$branch" --state all --json state --jq '.[0].state // "no-pr"')
  case "$state" in
    MERGED|CLOSED|"no-pr")
      git worktree unlock "$path" 2>/dev/null
      git worktree remove --force "$path"
      ;;
  esac
done
git worktree prune
```

Also prune `/private/tmp/caesium-*-bridge` and `/private/tmp/caesium-agent-*-impl` (codex sandbox fallback worktrees), and any `.claude/worktrees/agent-*` whose `git -C ... status` errors (rsync-recovery shells, post-publish).

### Step 7.5c: Delete merged remote branches

`gh pr merge --delete-branch` does NOT reliably delete the remote branch when the local branch is worktree-locked (see the "Common gotchas" note), so `worktree-agent-*` branches accumulate on origin and — because the codex path names branches deterministically — collide with a later wave's same-named branch. After the merge train, delete this wave's remote branches and sweep prior waves' strays:

```sh
git remote prune origin
git branch -r | grep -E '^[[:space:]]*origin/worktree-agent-' | sed 's|^[[:space:]]*origin/||' | sort -u | while IFS= read -r b; do
  [ -z "$b" ] && continue
  # Delete ONLY branches whose PR is MERGED. A closed-but-unmerged PR, a paused-wave
  # branch, or a recovery branch (any non-MERGED state, INCLUDING no PR at all) may
  # carry commits that are not on master — deleting it would lose that work.
  state=$(gh pr list --head "$b" --state all --json state --jq '.[0].state // "NO_PR"')
  case "$state" in
    MERGED) git push origin --delete "$b" </dev/null && echo "deleted remote $b" ;;
    *)      echo "SKIP $b ($state)" ;;
  esac
done
git remote prune origin
```

Deleting a squash-MERGED branch is safe — the content is in `master` and the PR preserves the head SHA. The `MERGED`-only guard (not "no open PR") protects closed-unmerged / paused / recovery branches; `</dev/null` stops `git push` from consuming the loop's stdin; the anchored `origin/worktree-agent-` grep avoids matching a differently-named remote. Never sweep non-`worktree-agent-*` branches (`sync-*`, `claude/*`) here, and delete per-branch after `git remote prune origin` — a single multi-ref `git push origin --delete a b c …` aborts if any one ref is already gone from the remote.

### Step 7.5d: Do NOT prune

- Worktrees the operator created manually (e.g. a sibling clone like `../caesium-<feature>/` outside `$REPO_ROOT`) — user-owned. Surface their existence in the final report if disk pressure remains; never auto-prune without explicit consent.
- Active claude-code chat worktrees (`.claude/worktrees/<adjective-name>/` like `adoring-pascal` — not `agent-*`).
- The main checkout (`$REPO_ROOT/`).

### Step 7.5e: Verify reclaim

```sh
du -sh $REPO_ROOT/.claude/worktrees
git worktree list | wc -l
```

If still large, find the heavy outlier with `du -sh .claude/worktrees/*/ | sort -h | tail` and either prune it or surface it in the final report.

---

## Phase 8: Finalize

**Goal**: verification sweep + Progress dashboard sync.

### Step 8a: Verification sweep

In the main repo (not a worktree):
```sh
git checkout master && git pull --ff-only origin master
just lint
just unit-test
```

If the wave shipped a runtime feature, optionally run `just integration-test` once on the final merged master as a closing end-to-end check.

### Step 8b: Audit the plan doc

Read the plan doc end-to-end again. Look for:

| Issue | Fix |
|---|---|
| Wave-N section in `## Progress` only has the wave-α entry (only that agent added one) | Rewrite the Wave-N section with full per-stream bullets, using the per-PR merge SHAs and review-resolve outcomes from your tracking |
| "Last updated" line still references a prior wave | Bump to today's date and current state |
| Acceptance Criteria now satisfied but unchecked | Tick (if checkboxes) or edit the prose to reflect met state |
| "Not yet picked up" line lists items this wave shipped | Update to list only the genuinely-deferred remainder |
| Per-item notes describe original behavior but were tweaked in review-resolve | Tighten the per-item note to match the merged behavior |
| Plan is functionally complete (all Acceptance Criteria met) | Add a top-of-doc note flagging the plan as a candidate for archive to `docs/exec-plans/completed/`, and update the matching `docs/roadmap.md` section's `**Status**:` to Shipped |

### Step 8c: Commit + push the dashboard sync

The Progress dashboard sync is a doc-only orchestrator update. `master` protection has `required_pull_request_reviews` set, and the repo's history is 100% PR-merges (no direct-to-master commits), so a plain `git push origin master` will be **rejected** unless you're a repo admin (admins bypass via `enforce_admins: false`). Default to a small docs-only PR; use a direct push only as an admin fast-path. Use this commit message format either way:

```
docs(<plan-slug>): sync plan with merged wave-<N> state

<1-2 sentence summary>

Updates:
- <bulleted list of dashboard changes>

{{MODEL_TRAILER}}
```

Then publish it:

```sh
# Default (non-admin): a tiny docs-only PR, then surface for CODEOWNER approval per Phase 7b.
git checkout -b sync-<plan-slug>-w<N> && git add <plan-doc> docs/roadmap.md && git commit -m "..." && git push -u origin sync-<plan-slug>-w<N>
gh pr create --title "docs(<plan-slug>): sync plan with merged wave-<N> state" --body "Dashboard sync for wave <N>."
# Admin fast-path only: `git push origin master` directly (allowed because enforce_admins: false).
```

Re-check protection at runtime (`gh api repos/caesium-cloud/caesium/branches/master/protection`); it can change.

### Step 8d: Final user report

Use the format from `SKILL.md` § Output. Be terse. Include all PR URLs and call out any PR still awaiting a CODEOWNER approval.

---

## Common gotchas

- **`gh pr merge --delete-branch` exit code 1**: the merge IS done — do not retry — but do NOT assume the remote branch was deleted. When the local branch is checked out in a worktree, `gh` often fails the local delete AND skips the remote delete, so `worktree-agent-*` branches pile up on origin (this wave left 49). Verify with `git ls-remote --heads origin <branch>` and clean leftovers in Phase 7.5c. The worktree-directory cleanup (Phase 7.5a) is separate and always needed.
- **`gh pr view <pr> --json merged`**: `merged` is not a valid field; use `mergedAt` (truthy when merged).
- **Sleeping after a push before retrying merge**: GitHub's `mergeStateStatus` doesn't update instantly. After a conflict-resolution push, sleep ~5s before re-attempting `gh pr merge`.
- **`UNSTABLE` `mergeStateStatus`**: normal for "mergeable but at least one non-required check failed" — caesium has no required checks, so a squash-merge succeeds anyway (subject to the CODEOWNER review gate).
- **Two CI runs per PR commit**: push (`branches: ['**']`) + pull_request both fire. Dedupe by `event` and prefer the `pull_request` run when reading PR checks.
- **`unit-test` ≠ integration**: `just unit-test` never compiles `test/` (it's behind `//go:build integration`). A "unit tests pass" claim is NOT integration-passing — the Phase 6.5 gate is mandatory for runtime PRs.
- **Both unit-test and integration-test `touch ui/dist/index.html`** before running Go, because `ui/embed.go` embeds `ui/dist`. A worktree without that file can fail to link the binary — the `just` targets handle it, but a hand-run `go test` won't.
- **arch-specific failures**: check both `unit-test` and `unit-test-arm64` (and both `build-and-integration-test` jobs). An arm64-only failure is real, not a flake.
- **builder is the CI root**: if `builder`/`builder-arm64` fails, every dependent job is skipped — diagnose the builder job first; downstream "failures" may just be "skipped".
- **Codex publish + verify is the orchestrator's job by default**: codex sandbox blocks DNS for github.com AND has no Docker, so it can run none of caesium's verify chain. The codex stream prompt tells the agent to gofmt + stage + STOP; Phase 4.5 runs `just lint` + `just unit-test` and publishes. Three worktree shapes to handle: standard, `/private/tmp/caesium-agent-<id>-impl` (on `master`), and already-committed.
- **§ codex-companion background dispatch is unreliable — use `codex exec`**: `node codex-companion.mjs task --background` spawns a detached job-worker that dies at thread start with `failed to load configuration: No such file or directory (os error 2)` (~440ms after dispatch), while `codex exec` from the same shell works. It is NOT a codex-cli version regression (the identical error is recorded against much older plugin versions in `~/.claude/plugins/data/codex-openai-codex/state/`); the trigger is the detached-worker environment inherited from a sandboxed sub-agent shell. Dispatch codex streams via `codex exec` piped from a prompt file inside a `Bash run_in_background` call (Phase 3 § codex), and rely on the harness's background-task completion notifications for status instead of `codex-companion status`. `codex exec` respects the user's `~/.codex/config.toml` (model/effort/sandbox), so the model is set by config, not a per-dispatch flag.
- **Worktrees share a git stash store**: a `git stash` from one worktree can be popped into a sibling, silently corrupting foreign work. Stream agents commit WIP (`git add -A && git commit -m "wip" --no-verify`, then `git reset HEAD~1`) instead. Codified in the stream prompts.
- **Security stub-on-merge is a P0**: an agent stubbing `validateSignature` / `ValidateKey` / `HasRole` / `CheckScope` / a `subtle.ConstantTimeCompare` / the SAML `recordAssertion` / the OIDC nonce check AND merging ships a forgery / privilege-escalation / replay hole. Treat these as merge-blocking; re-dispatch as a fix-forward (sonnet → opus) before Phase 7. Verify any "dep would conflict" stub-justification against `go.mod`/`go.sum` first.
- **`docs/roadmap.md` is lowercase**: there is NO `ROADMAP.md`. A dashboard sync that edits "ROADMAP.md" creates a duplicate file. Always edit `docs/roadmap.md`.
- **Commit trailer**: the snippets use a `{{MODEL_TRAILER}}` placeholder — substitute the `Co-Authored-By:` trailer your environment / `CLAUDE.md` currently mandates (at time of writing, `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`). Using the placeholder rather than a baked-in literal keeps trailers from going stale across model rotations (an archived plan still carries the older `Claude Opus 4.7`). Resolve `{{MODEL_TRAILER}}` from your current instructions, not from this line.

## When something genuinely doesn't fit the playbook

Stop and report. The user wrote `exec-plan-wave` to handle the typical case end-to-end, but they'd rather you stop and say "X surprised me, here's what I found" than push through with a fabricated solution.

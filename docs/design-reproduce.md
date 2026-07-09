# Design: `caesium reproduce` — Re-Execute a Historical Production Task Locally

> Status: Shipped/active — implemented via
> [`exec-plans/completed/reproduce.md`](exec-plans/completed/reproduce.md) (PRs
> #334-#340). Siblings: [`design-quarantined-replay.md`](design-quarantined-replay.md)
> (server-side what-if), [`design-backtesting.md`](design-backtesting.md)
> (N-run server-side sibling), [`design-agent-in-the-loop.md`](design-agent-in-the-loop.md)
> (consumes the repro one-liner in escalations).

## Problem

A production task failed at 03:00. Today the debug loop is: open the run page,
squint at the persisted log snapshot, guess which of the forty env vars
mattered, hand-rebuild an approximation of the container invocation on your
laptop (`docker run -e ...` from memory), and discover your reconstruction
differs from what actually ran in four ways you find one at a time. It is
"works on my machine" inverted: the machine where it *doesn't* work is prod,
and you cannot iterate there.

Caesium already records everything needed to do better. Every task run persists
an immutable `TaskExecutionDescriptor`
(`internal/models/run.go`, `TaskRun.ExecutionDescriptor`): the exact image and
resolved digest, argv, workdir, the full container spec env (literals verbatim,
`secret://` refs unresolved), mounts, run params, the recorded outputs of every
predecessor, schema-validation config, and retry/timeout knobs. Quarantined
replay (`internal/replay/replay.go`) already proves this envelope is sufficient
to reconstruct a task byte-for-byte — it recomputes the baseline identity hash
from the descriptor alone (`computeDescriptorHash`).

`caesium reproduce <run-id> --task transform` pulls that identity from the
server and re-executes the single task on the operator's laptop via the local
Docker engine — the same runtime `caesium dev` rides — with upstream outputs
materialized as `CAESIUM_OUTPUT_*` env from the recorded values. `caesium why`
tells you *which input changed*; `reproduce` lets you *run the task with those
exact inputs*, tweak one thing, and run it again. A local debug loop instead of
an SSH session.

## Fit with Design Principles

- **Container-native execution.** The whole feature is possible only because a
  task *is* a container invocation: image digest + argv + env is the complete
  behavioral interface. No SDK, no agent in the image — any historical task of
  any language reproduces the same way.
- **Data-plane memory payoff.** The descriptor/receipt substrate was built to
  answer EXPLAIN/REPRODUCE/SKIP ([`design-data-plane-memory.md`](design-data-plane-memory.md)).
  The receipt is the *attestation* half of REPRODUCE; this verb is the
  *execution* half.
- **Local-first dev story.** `caesium dev` runs tomorrow's pipeline locally
  before it ships; `reproduce` runs yesterday's production task locally after
  it broke. Together they close the loop: author → deploy → debug → fix →
  author, all on one machine.
- **Zero-dependency simplicity.** One new read-only GET endpoint server-side.
  Everything else is CLI + the operator's own Docker daemon.

## Overview

`reproduce` is a **client feature**. The server's only involvement is serving
the stored descriptor over a new authenticated endpoint. Nothing executes
server-side: no `JobRun`/`TaskRun` rows are created, no events are emitted, no
metrics move, no quarantine machinery engages. This deliberately **diverges
from the quarantined-replay safety model**: replay re-executes inside Caesium
and therefore needs the `replaySafe` gate, quarantine markers, and the full
side-effect-producer audit. Reproduce runs on the operator's machine, under the
operator's local credentials and network — their Docker daemon is the sandbox
boundary. That is honest and simple: there is nothing server-side to quarantine
because nothing server-side runs. The corollary is equally honest: **side
effects are not suppressed**. The task really executes; if it can reach a
production database from your laptop with locally resolved credentials, it
will. The default posture is therefore secrets-unresolved (below).

The `replaySafe` mark is *not* required to reproduce — it gates Caesium
re-executing on the operator's behalf inside prod infrastructure, which is not
what happens here. The CLI surfaces the recorded `replaySafe` value and the
unresolved-secrets default as its safety story.

## UX Walkthrough

```console
$ export CAESIUM_API_KEY=...
$ caesium reproduce 4f9c… --job-id 7a1b… --task transform
Fetching descriptor from https://caesium.internal … ok (captured 2026-07-02T03:00:14Z)
Pulling etl/transform@sha256:9e2f… … ok (recorded digest, content-identical)
Reconstructing environment:
  12 literal env vars from recorded spec
   4 CAESIUM_PARAM_* from recorded run params
   6 CAESIUM_OUTPUT_* from recorded predecessor outputs (extract, validate)
  WARNING: 2 secret refs left UNRESOLVED (env omitted): DB_PASSWORD, API_TOKEN
           pass --resolve-secrets to resolve from your local providers
  WARNING: bind mount /var/data/staging not remapped; pass --mount /var/data/staging=<local-path>
Running via local docker …
##caesium::output {"row_count": "0"}
transform exited 0 in 4.2s
Output diff vs recorded:            (--diff)
  row_count: recorded "184223" → reproduced "0"    MISMATCH
exit status 3
```

The three modes:

1. **Run mode (default).** Fetch → `docker pull <image>@<recorded digest>` →
   reconstruct env → run the recorded command → parse `##caesium::output`
   markers from stdout (reusing `pkg/task.ParseMarkers`) → with `--diff`,
   compare against the recorded `TaskRun.Output`.
2. **Shell mode (`--shell`).** Same fetch/pull/env reconstruction, but instead
   of the recorded command, drop into an interactive shell
   (`docker run -it --entrypoint <shell>`) inside the exact environment. For
   poking at the inputs by hand. (Distroless images without a shell fail here
   with a clear error; run mode still works.)
3. **Fix-testing mode (`--image`).** Override the image (e.g. a locally built
   candidate fix) while keeping the recorded env, params, and predecessor
   outputs — "does my patch produce the right output *given the exact inputs
   that broke prod*?" The output line prominently marks the run as
   image-overridden so it is never mistaken for a faithful reproduction.

Tweaks: `--set key=value` overrides run params (re-deriving the affected
`CAESIUM_PARAM_<KEY>` var, matching how both executors inject params), and
`--set-env KEY=VALUE` overrides/adds a raw container env var. `--dry-run`
prints the fully reconstructed envelope (env, image, command, mounts, warnings)
as JSON without executing — the inspection surface, and the integration-test
hook.

## Backend

One new endpoint, read-only:

```
GET /v1/jobs/:id/runs/:run_id/tasks/:task/descriptor
```

- Resolves `:task` by task **name** within the run (the ergonomic handle; a
  UUID is also accepted), then returns the stored
  `TaskRun.ExecutionDescriptor` JSON verbatim, plus a small wrapper
  (`task_run_id`, `status`, `result`, recorded `output`, `replay_safe`,
  `log excerpt pointer`). The loader is the existing
  `run.Store.TaskExecutionDescriptor(ctx, runID, taskID)`
  (`internal/run/store.go:541`), which already enforces presence and schema
  version — reuse it, do not re-decode.
- **Why not the receipt endpoint?** The receipt
  (`internal/receipt/receipt.go`) is a Merkle digest for *verification* —
  per-task identity hashes + image digests, nothing runnable. The descriptor is
  the full runtime envelope. Reproduce needs the envelope; `verify` keeps the
  receipt. Different artifacts, different endpoints.
- **Auth scoping.** `TaskRun.ExecutionDescriptor` is `json:"-"` today — never
  serialized on any existing surface — because it contains literal
  (non-secret-ref) env values. The new route sits under the `/v1/jobs/:id`
  prefix, so the existing scoped-key arm in
  `api/middleware/auth_scope.go` (`authorizeScope` → `resolveScopedJobAlias` →
  `auth.CheckScope`) covers it with **zero new middleware code**, exactly like
  the receipt and `why` routes bound in `api/rest/bind/bind.go:86,104`. A
  scoped key sees descriptors only for jobs in its scope.
- **No secret values, structurally.** The descriptor stores `secret://` refs
  verbatim in `ContainerSpec.Env` and provider identity metadata (version /
  resourceVersion / server-keyed HMAC) in `SecretRefs` — never values
  (`internal/models/run.go:256`, invariant inherited from the replay design).
  The endpoint therefore cannot leak a credential even to a fully privileged
  caller. This is a feature: **prod secret values never leave the server
  because they were never on it.**

That is the entire server surface. Reproduce stays a client feature; resist any
future temptation to add a "reproduce on the server for me" mode — that verb
exists and is called replay/backtest.

## CLI

```
caesium reproduce <run-id> --job-id <id> --task <name> [flags]

Flags:
  --job-id string        Job ID (required; same convention as receipt/replay)
  --task string          Task (step) name within the run (required)
  --set k=v              Override a run param (repeatable; re-derives CAESIUM_PARAM_*)
  --set-env K=V          Override/add a raw container env var (repeatable)
  --image ref            Override the image (fix-testing mode; marks output OVERRIDDEN)
  --shell                Interactive shell with the exact env instead of the command
  --resolve-secrets      Resolve secret:// refs via LOCAL providers (default: omit + warn)
  --mount old=new        Remap a recorded bind-mount source to a local path (repeatable)
  --platform string      Passed to docker (e.g. linux/amd64) for cross-arch pulls
  --diff                 Compare parsed ##caesium::output against recorded output
  --dry-run              Print the reconstructed envelope as JSON; do not execute
  --json                 Machine-readable result on stdout (logs/warnings on stderr)
  --timeout duration     Local task timeout (default: recorded TaskTimeout)
  --server string        Server base URL (default http://localhost:8080)
  --api-key string       Bearer key (prefer CAESIUM_API_KEY; flag is visible in ps)
```

Exit codes: `0` task succeeded (and `--diff` matched, if set); `1` task ran and
failed; `2` fetch/auth/reconstruction error (including missing descriptor);
`3` task succeeded but `--diff` found an output mismatch.

Per the repo's hard-won stream rules: `--json` and `--dry-run` write **only**
the JSON document to stdout; every log line, warning, and progress message goes
to stderr. The integration test asserts stdout is parseable in isolation
(`runCLIStdout`, never the stream-merging `runCLIRaw`).

Env reconstruction order (later wins): recorded `ContainerSpec.Env` literals →
`CAESIUM_PARAM_<KEY>` from recorded `Run.Params` (both executors inject every
param this way; `internal/job/job.go`, `internal/worker/runtime_executor.go`) →
`CAESIUM_OUTPUT_*` via `pkg/task.BuildOutputEnv` over
`descriptor.DAG.PredecessorOutputs` (name-keyed, exactly as
`internal/replay/replay.go:474` does for hash reconstruction — reuse that
mapping code, do not reimplement) → `--set` param re-derivation → `--set-env`.

## What Reproduces Faithfully vs Best-Effort

This table is the contract. The CLI prints a per-run fidelity summary derived
from it; anything in the right column produces an explicit warning, never
silence.

| Dimension | Fidelity | Detail |
|---|---|---|
| Image content | **Faithful** when `ResolvedImageDigest` recorded (digest pinning on): pull by digest is content-identical. | Unpinned mutable tag → pull by tag, marked **DEGRADED** (same honesty rule as the receipt: never attest a mutable tag). |
| Command / argv / workdir | **Faithful** — recorded verbatim in `Runtime.Command`/`CommandRaw`/`WorkDir`. | — |
| Literal env vars | **Faithful** — stored verbatim in the descriptor. | — |
| Run params (`CAESIUM_PARAM_*`) | **Faithful** — `Run.Params` recorded per task. | — |
| Predecessor outputs (`CAESIUM_OUTPUT_*`) | **Faithful for scalars** — recorded typed values, incl. `_DIGEST` companions for refs. | Large **output-refs**: env carries the recorded path + digest, but the payload lives in BYO storage. If the volume/object store isn't mounted locally (see `--mount`), the task sees a dangling path — warned, degraded. |
| Schema config | **Faithful** — `outputSchema`/`validationMode` recorded; reproduce re-validates locally and reports violations. | — |
| Secret values | **Not reproduced by default** — refs omitted, warned. With `--resolve-secrets`, resolved from the **operator's local** providers, which may hold different values than prod did at baseline. | Drift warning is best-effort: ref string + provider identity fields (Vault version, k8s resourceVersion) compare when the local provider matches; the recorded HMAC is server-keyed (`SecretRefs.Identity`) and deliberately not client-verifiable. |
| Host bind mounts / volumes | **Not reproduced** — recorded sources are prod paths. Warn + `--mount old=new` remap. PVC / claimTemplate / k8s volumeSource mounts cannot exist under local Docker: warned and skipped. | |
| Engine & workload identity | **Best-effort** — podman/k8s tasks run under local Docker; `ServiceAccountName`, pod annotations, node selector, Kueue queue have no local equivalent (listed in the fidelity summary, not applied). | |
| CPU architecture | **Best-effort** — a prod arm64 digest on an amd64 laptop runs under qemu emulation if configured (`--platform`), with real behavioral/perf differences. Warned when the manifest platform mismatches. | |
| Resource limits | **Not recorded** in descriptor v1. When [`design-resource-right-sizing.md`](design-resource-right-sizing.md) lands recorded limits, reproduce applies them — reproducing an OOM locally becomes one command. | |
| Wall clock / time | **Not reproduced** — the task runs *now*. `date`, TTLs, "yesterday's partition" logic all see today. | |
| External system state | **Not reproduced — say it loudly:** reproduce replays the task's *env and inputs*, not the world. The database the task queried has moved on; the API it called returns today's answer. For tasks that read external systems, reproduction is best-effort by construction. | |
| Side effects | **Not suppressed** — the task really executes. Blast radius = whatever the operator's laptop and locally resolved credentials can reach. Hence secrets-unresolved by default. | |

## Security

- **Default-deny on secrets.** Without `--resolve-secrets`, every env var whose
  recorded value is a `secret://` ref is omitted and named in a warning. The
  reproduction is inert against anything credential-gated until the operator
  explicitly opts in — and then the credentials used are *their own*, resolved
  by their local `secret.Resolver` config, never fetched from the server.
- **Descriptor endpoint scoping.** Auth-mode-on deployments require a key; a
  scoped key is confined to its job aliases by the existing
  `/v1/jobs/:id` arm. The descriptor exposes literal env values, so this
  endpoint is job-read-equivalent sensitivity — same tier as logs, which
  already leak the same values.
- **No new secret surface server-side.** The endpoint serializes bytes that
  already exist in `task_runs.execution_descriptor`; the never-store-values
  invariant is enforced at capture time, not response time.
- **API key hygiene** follows the established CLI pattern: `CAESIUM_API_KEY`
  preferred, `--api-key` accepted with a visible-in-ps warning
  (`cmd/why/why.go`, `cmd/run/diff.go`).

## Interplay

- **Quarantined replay** — the server-side ancestor. Reproduce reuses its
  descriptor decode/validation (`decodeBaselineTask` invariants) and env/hash
  reconstruction, and inherits its never-store-secret-values invariant, but
  discards its quarantine machinery because nothing runs inside Caesium.
- **[`design-backtesting.md`](design-backtesting.md)** — the N-run server-side
  sibling: same descriptor substrate, fanned across history, executed in
  quarantine. Reproduce's `--diff` output-compare (recorded vs reproduced
  `##caesium::output` maps) is the same comparison primitive backtesting needs
  per run; build it once as a shared package.
- **[`design-agent-in-the-loop.md`](design-agent-in-the-loop.md)** — every
  diagnosed page gets a repro command. The escalation the agent writes
  ("transform failed, discriminating input: `CAESIUM_OUTPUT_EXTRACT_ROW_COUNT`
  changed") appends
  `caesium reproduce <run> --job-id <id> --task transform --diff` — the human
  starts their shift with a one-liner that puts the failure in a container on
  their laptop.
- **[`design-resource-right-sizing.md`](design-resource-right-sizing.md)** —
  once recorded limits enter the descriptor, `reproduce` applies them by
  default, making "repro the OOM locally, then re-run with `--set-env` /
  a limit override to find the ceiling" a tight loop.
- **`caesium why` / receipts** — `why` names the changed input; `reproduce`
  executes with it. `verify` attests digests; `reproduce` consumes them.

## Testing

Per the repo gate, this ships with integration coverage in `test/` that drives
the real surface — no unit-test-on-the-handler substitute:

1. Run a job with structured outputs + digest pinning on the live integration
   server; capture run/task IDs.
2. `caesium reproduce … --dry-run --json` via `runCLIStdout`: assert stdout is
   clean parseable JSON and the reconstructed env exactly equals the recorded
   envelope — literals, `CAESIUM_PARAM_*`, `CAESIUM_OUTPUT_*` (the load-bearing
   assertion), with secret refs listed as omitted.
3. Full local execution against the harness Docker daemon: assert exit 0 and,
   with `--diff`, a match against recorded output; then a mutated `--set` run
   asserting exit 3 on deliberate mismatch.
4. Auth lane: scoped key fetches an in-scope descriptor (200) and is refused an
   out-of-scope job (403), following the receipt/why scope tests.
5. Failure honesty: a run whose descriptor is absent (pre-descriptor row) exits
   2 with the "descriptor unavailable" error, not a partial reconstruction.

## Phasing

- **P0** — Descriptor endpoint (+ scope tests); CLI fetch, digest pull,
  env/param/predecessor-output reconstruction, secrets-omitted default,
  `--mount` remap, `--dry-run`, `--json`, run mode + exit codes; integration
  scenarios 1–2–4–5.
- **P1** — `--shell` mode; `--diff` output-compare (shared with backtesting);
  fidelity summary block.
- **P2** — `--image` fix-testing mode; `--resolve-secrets` with best-effort
  secret drift warnings from recorded `SecretRefs` identity metadata.

## Non-Goals

- **Not a time machine for external state.** Reproduce replays recorded env and
  inputs; it does not restore the world the task observed.
- **No server-side execution.** No runs, rows, events, or metrics are created
  server-side; any "run it for me" ask is replay/backtesting.
- **No multi-task local DAG re-run.** One task, its recorded inputs. Whole-DAG
  local runs are `caesium dev`; historical whole-DAG what-ifs are
  replay/backtesting.
- **No side-effect suppression.** Honesty over illusion: the container really
  runs. The safety controls are unresolved-secrets-by-default and the
  operator's own machine boundary.
- **No secret-value transport, ever** — in either direction.

## Open Questions

1. **Descriptor retention.** Descriptors ride `task_runs` rows, which have no
   pruner today (only events/webhook rows expire, `pkg/env/env.go:154`). If a
   run-retention policy lands, pruned descriptors must yield the clean exit-2
   error — should the endpoint distinguish "pruned" from "predates descriptors"
   for better operator messaging?
2. **Runtime reuse.** Ride `internal/localrun` by synthesizing a one-step
   definition (gets timeouts, marker parsing, log capture for free, at the cost
   of an in-memory SQLite it doesn't need), or call the Docker engine adapter
   directly? Leaning localrun for behavioral parity with `caesium dev`.
3. **Attempt semantics.** Reproduce runs exactly once — should recorded
   retry/backoff config be applied, or is single-shot the right debug-loop
   default (current lean)?
4. **`--shell` on distroless images.** Fail with guidance, or offer a
   `--shell-image` sidecar-style fallback that mounts the env into a
   busybox:1.36.1 container?
5. **Explain integration.** Should the endpoint also return the redacted
   `HashInputBlob` so the CLI can print "which field differs from the previous
   run" inline (a mini-`why`), or is that scope creep past the debug loop?

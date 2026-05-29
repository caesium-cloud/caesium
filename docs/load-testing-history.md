# Load Testing History

> **Status:** Consolidated historical record of the Phase 0 → Phase 2B
> distributed-execution scaling effort (May 2026). This is a retrospective
> narrative assembled from seven point-in-time baseline docs; detail-level
> per-run logs were folded into the phase summaries below.

This effort hardened Caesium's job-execution path from a single-node baseline
through a distributed run-owner architecture, using load measurements to gate
each step. The standard workload throughout was a **100-task stress test**
(10 jobs × fan-out=4 × depth=3, 500ms tasks, 5 concurrent runs) run on a
single-node local Docker deployment and on a **3-node dqlite Raft cluster**
(Docker Desktop k8s, kubernetes engine, ephemeral storage). Each phase below
preserves the headline numbers and — most importantly — the decision the
measurement drove. The narrative was consolidated from seven baseline documents
spanning 2026-05-23 to 2026-05-25.

## Phase 0 — Single-node baseline (2026-05-23)

Established the write-category baseline against the local Docker engine
(`CAESIUM_EXECUTION_MODE=local`, commit `bda2491`).

**100-task workload (single-node local):** 720 total row writes, ~7.2 writes/task.
- `task_run_status` was the dominant category at **44.4%** (320 rows), edging out
  `event_insert` at 41.7% (300 rows) and `task_run_insert` at 13.9% (100 rows).
- This **reversed the analytical prediction** that `event_insert` would dominate;
  the gap came from predecessor-counter UPDATEs in `CompleteTask` that the model
  underweighted.
- End-to-end p50/p99: **4s / 6s**. `db_busy_retries`: 7 — measurable contention
  even on a single-node sandbox. `lease_renewal` and claim writes were 0 (local
  mode bypasses the distributed claimer).

**Decision:** `task_run_status` dominance confirmed the design's Phase-2 gate
condition. Recommended adding predecessor-counter UPDATE batching (Phase 1.4)
alongside event coalescing (Phase 1.1), and deferred the Phase 2 go/no-go to
real-cluster numbers.

## Post-Phase-1 re-baseline (2026-05-23)

After PRs **#165** (1.2 lease batching) and **#167** (1.1 event coalescing +
1.4 predecessor batching) merged (commit `648b118`), the same workload was
re-run.

- Row counts were **unchanged** (720 total, identical category split): the
  `caesium_db_writes_total` counter increments by row count, not statement
  count, so batching N INSERTs into one multi-row INSERT is invisible to it.
- Distributed-mode short-task run (500ms × 100 tasks): 920 rows (+200 vs local,
  = 2 extra writes/claim for the claim UPDATE + `task_claimed` event), p50/p99
  6s/6s, 10 retries, 100 claims.
- Long-task run (45s × 8 tasks) exercised `lease_renewal`: 16 rows across just
  **2 statements** — confirming PR #165's batching works (2 renewal cycles ×
  8 in-flight claims).

**Decision:** Phase 1's per-statement win is invisible to the row counter;
added the need for a `caesium_db_statements_total` counter (shipped as **#169**).
Phase 2 gate decision explicitly deferred to a multi-node distributed run.

## Distributed baseline — the Phase 2 gate justification (2026-05-24)

First real numbers from the 3-node dqlite Raft cluster after all Phase 1 work
plus the #169 statement counter. **This is the canonical Phase-2 gate
justification.** Pod logs visibly showed `database is locked` errors on INSERT —
exactly the symptom the run-owner design targets.

**100-task stress workload, single-node vs distributed (headline numbers):**

| Metric | Single-node local | 3-node distributed | Delta |
|---|---|---|---|
| Total run time | 18s | 67s | **3.7× slower** |
| End-to-end p50 | **4s** | **31s** | **7.8× slower** |
| End-to-end p99 | 6s | 32s | 5.3× slower |
| `db_busy_retries` | 9 | **65** | **7.2× more contention** |

The 100-task distributed run completed 10/10 but at 489 rows / 311 statements
(lower than single-node only because slow completions fell outside the metric
window — itself a signal). A 25-task moderate workload showed the same pattern:
p50/p99 17s/31s, 26 retries (~1 per task). `task_run_status` remained the
dominant write category (40-44%) across both workloads. `lease_renewal` stayed
at 0 (tasks finish faster than the `lease_ttl/2` renewal threshold).

**Decision: proceed with Phase 2.** The dominant-remaining-category test
(`task_run_status` from claim/start/complete transitions) was met, and 7.2×
contention with 5-8× worse latency is precisely the regime the run-owner pattern
addresses. Recommended starting with **Phase 2A** (run-lease table + owner
election + dispatch RPC) before the checkpoint/replay machinery.

## Phase 2A — substrate merged, dispatch never wired (2026-05-24)

First measurement of Phase 2A (PR **#171**, commit `8e484eb`,
`CAESIUM_RUN_OWNER_ENABLED=true`).

- **Regression, not improvement:** vs the pre-Phase-A distributed baseline,
  total rows +32% (489→645), statements +46% (311→455), and
  `db_busy_retries` **+56%** (65→101). p50/p99 and wall-clock unchanged.
- Lease acquisition and renewal worked correctly (`run_leases_owned`=10, 13
  batched renewal cycles). The mTLS warn-only startup notice fired as expected.
- **But the executor-side dispatch call was never wired up:** workers still
  pulled via `ClaimNext`. `complete_rejected_total`=0 (endpoints never called),
  `worker_claims_total`=63 (claimer still the hot path). Phase A added lease
  writes without removing any claim contention — hence the +56%.

**Decision:** Phase A is substrate only. Wire the executor-side dispatch loop
in a small follow-up (Phase A2) before Phase B can be cleanly measured.

## Phase 2A2 — DB-poll dispatch loses the race (2026-05-24)

Measurement of the executor-side dispatch loop (PR **#173**, commit `6ac4735`).

- **Finding 0 (operational gotcha):** without `CAESIUM_INTERNAL_WAKEUP_TOKEN`
  set, every `/internal/dispatch` POST returns 401, counted as `worker_rejected`
  — `dispatch_sent_total`=0, `db_busy_retries`=116. Run-owner mode requires the
  wakeup token; startup should warn (or refuse) when it is missing.
- **Finding 1 (structural):** with the token set, dispatch could succeed but
  stalled at `dispatch_sent_total`=6 — ClaimNext is wakeup-driven (sub-second)
  while the dispatch loop polls every ~1s, so **ClaimNext wins the race almost
  every time**. A2 added the push path but never made ClaimNext defer to the
  owner for owned runs; both ran concurrently and the lower-latency one won.
- **Finding 2:** under stress the per-tick DB polling destabilized dqlite —
  **5/10 runs failed**, p99 **5m47s** (vs 31s prior), from `checkpoint in
  progress` / poisoned-connection errors.

**Decision: abandon the DB-poll dispatch loop, go to Phase B.** The fix
requires ClaimNext to defer to the owner for owned runs plus an in-memory
ready-set (no per-tick poll) — both squarely Phase B, not a small A3 tweak.
Shipped two small fixes first: a startup guard for the missing wakeup token,
and keeping A2's loop default-off.

## Phase B1 — deferral works, but push path never executes (2026-05-25)

Measurement after PR **#175** (ClaimNext run-lease deferral) and PR **#176**
(global dqlite retry) merged (commit `11ff432`).

- **0/10 runs succeeded** (2 failed, 8 timed out at the 30-min poll cap).
  `worker_claims_total`=**0**, `dispatch_sent_total`=7 across the whole hour,
  `task_run_status` writes=3. caesium-0 owned all 10 runs.
- `claims_total`=0 proves B1's deferral works *perfectly* — ClaimNext correctly
  steps aside for every live-leased run. But this **exposed that the receiving
  half of run-owner mode was never built**: `HandleDispatch` claims the task
  (marks it `running`) but never submits it to the runtime executor;
  `PostComplete` is defined but has zero callers; `/internal/complete` is
  implemented but never invoked. Dispatched tasks are orphaned and runs stall.
- Why A2 *appeared* to work: ClaimNext was silently executing everything. B1's
  deferral removed that crutch and surfaced the pre-existing gap. Not a
  regression in #175/#176 (both correct in isolation; owner mode is default-off).

**Decision:** Phase B is materially larger than "in-memory state" — the
execution path itself must be built (dispatch→execute→complete cycle). Re-scope
Phase B around that cycle first, in-memory state as a later layer. Process note:
pair every measurement with a full lifecycle code-path trace, not just metric
deltas. This became PR **#178**.

## Phase B2 — execution cycle closed, 0/10 → 10/10 (2026-05-24)

Measurement of the dispatch→execute→complete cycle (branch
`phase2-b2-dispatch-execute-complete`, base commit `b00c869`).

- The B2 commit wired the cycle: owner pushes via `/internal/dispatch`, the
  worker executes on its existing pool, and reports back via `/internal/complete`
  (a `CompletionSink` routing ClaimNext'd tasks locally and dispatched tasks to
  the owner). First stress run scored **8/10** — a step change from B1's 0/10.
- **Reaching a stable 10/10 required two contention-hardening fixes**, both
  validated firing under real load:
  - **Finding 1 (lost completions):** owner-side `CompleteTaskClaimed` hit
    `database is locked`, fell through to a generic 409, which the worker treated
    as a terminal fence rejection — dropping a successful task's completion. Fix:
    on `dqlite.IsContentionError`, `HandleComplete` returns **503** (retryable,
    surfaced as `ErrOwnerBusy`); the worker retries with bounded backoff.
  - **Finding 2 (run-start killed by a blip):** run-start reads failed the whole
    run on a transient `checkpoint in progress` surfacing *during row iteration*
    (after `QueryContext` returned cleanly, so the pool-layer #176 retry missed
    it). Fix: `retryOnContention` wraps the five idempotent run-start reads.
  - Finding 3: the apparent duplicate execution was downstream of Finding 1 (lost
    completion → expired lease → recovery re-run), not a separate bug.

**Result:** **10/10 across three consecutive runs**, `dispatch_sent_total`
exactly 100/run (every task dispatched once), 8 `complete_retryable` events all
recovered, zero lost completions. p50 ≈ 17s, p99 ≈ 31s.

**Decision:** B2 delivers run-owner execution — the owner is the single writer
for its run's coordination rows. Contention hardening was the gating work, not
the cycle mechanism. `CAESIUM_RUN_OWNER_ENABLED` stays default-off until
internal mTLS lands (still warn-only at this point).

## Phase B3 — in-memory state, checkpoint/replay, mTLS, failover (2026-05-25)

Measurement of in-memory owner run-state + checkpoint/replay plus internal-endpoint
mTLS, with `CAESIUM_RUN_OWNER_IN_MEMORY=true`.

**Shipped to master / on the B3 branch (`phase2-b3-checkpoint-replay-and-internal-mtls`):**
- **Internal mTLS verified:** dedicated `:8443` listener came up on every node;
  owner→worker dispatches reached `HandleDispatch` over mutual TLS. Run-owner
  mode hard-fails at startup without all three `CAESIUM_INTERNAL_MTLS_*` files.
- **In-memory advancement verified:** **10/10 on both single-node and 3-node.**
  A completion advances in-memory state, stamps a monotonic `terminal_sequence`,
  readies successors, and finalizes with terminal-only DB writes (no
  per-transition predecessor UPDATEs) plus periodic `run_checkpoints`.
- **Stall bug fixed:** the worker's `ClaimTaskForDispatch` required
  `outstanding_predecessors = 0` in the DB, but the owner advances in memory and
  doesn't decrement that counter — so in-memory-ready successors were rejected
  (409) and re-dispatched in a loop. Fix: a `trustOwnerReadiness` flag drops the
  predicate in in-memory mode (SQL mode unchanged).
- Two early failover bugs fixed here: the dead-peer dispatch hang (added a 4s
  per-dispatch timeout) and `ResetInFlightTasks` not clearing `claimed_by`.

**Verified on the unmerged `phase2-failover-hardening` branch:**
- **Owner-crash failover proven end-to-end:** owner killed mid-run (1 task
  in-flight, 6 pending); a survivor took the expired lease ~10s later (= lease
  TTL), recovered at generation 2 from checkpoint + post-checkpoint terminal
  rows, re-dispatched the in-flight task with zero rejections, completed all 8
  tasks; 2/3 quorum held throughout.
- Two further failover bugs fixed on this branch: takeover was gated *behind*
  peer discovery (the expired-lease sweep ran after discovery, which fails
  during the very quorum disruption an owner crash causes — moved the sweep to
  the top of the tick, independent of membership); and the claim fence rejected
  the new owner's generation (changed `owner_generation = ? OR = 0` to
  `owner_generation <= ?` so a task touched by an older generation can be
  re-claimed). Guarded by `TestFailover_TakeoverAndResume`.

**On the unmerged `phase2-dispatch-peer-liveness` branch:** a circuit breaker
that benches a peer failing with a network error (so a restarted owner's stale
peer IP no longer wastes ~4s per dispatch). Unit-verified; a 3-node k8s
confirmation is the last step before flipping the in-memory default on.

**Decision:** steady-state advancement (10/10) and owner-crash failover are both
verified, with a deterministic test guarding the failover claim path.
`CAESIUM_RUN_OWNER_IN_MEMORY=true` is a viable default-on candidate. A single
voter kill resumes cleanly; killing multiple voters in one session drops below
quorum (a cumulative-churn sandbox artifact, not a failover-logic fault).

## Outcome

The distributed-execution scaling effort reached a verified run-owner
architecture: the run owner is the single writer for its run's coordination
rows, driving a working **dispatch → execute → complete** cycle (100-task stress
workload stable at 10/10 on both single-node and 3-node). The owner advances DAG
state **in memory** with terminal-only DB writes plus periodic checkpoints —
eliminating the per-transition `task_run_status` contention that the 2026-05-24
distributed baseline measured at 7.2× the single-node retry rate. Internal
endpoints are protected by **mTLS** (hard-fail startup without certs).
**Owner-crash failover is proven end-to-end**: a survivor acquires the expired
lease, replays from checkpoint, and drives the run to completion. The execution
cycle, in-memory state, checkpoint/replay, and mTLS shipped to master via the B2
and B3 work; failover hardening and dispatch peer-liveness were verified on the
`phase2-failover-hardening` and `phase2-dispatch-peer-liveness` branches and
remained the final items before flipping `CAESIUM_RUN_OWNER_IN_MEMORY` default-on.

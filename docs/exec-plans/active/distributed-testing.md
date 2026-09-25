# Distributed Testing and Performance Confidence

Last updated: 2026-09-25

Make a passing required CI result meaningful evidence that Caesium preserves
its backend guarantees, developer workflows, and Console behavior under normal
operation and supported failures, without exceeding an agreed performance
regression budget. This plan extends the existing tests; it does not replace
full engine coverage with smoke suites or claim that finite tests prove every
possible execution correct.

The user requested comprehensive testing improvements and a plan PR for review.
This is a proposed testing initiative, not an implementation or a change to
current branch protection. Runtime defect fixes discovered by these tests need
their own scoped change with a reproducing regression. New product capabilities,
new execution guarantees, resource right-sizing, and pipeline backtesting are
not implicitly authorized by this plan.

This plan follows the native `exec-plan-wave` structure: Progress records merged
evidence, Streams owns the backlog, and Sequencing defines dispatch readiness.

## Source-Of-Truth Note

The request above anchors intent. [AGENTS.md](../../../AGENTS.md) defines the
real-surface and containerized-verification requirements. The
[CI runbook](../../ci.md), [workflow](../../../.github/workflows/ci.yml), and
[execution operations guide](../../parallel-execution-operations.md) describe
existing wiring and operational contracts. The contract record lives in this
file under Strategic Decisions; A1 and F1 refine it without introducing another
design document. Stream A must reconcile those contracts with code before
asserting stronger guarantees. A gap between desired and implemented behavior
must remain visible; neither weakening the checker nor
silently redefining a product guarantee is an acceptable resolution.

The [roadmap](../../roadmap.md) and [closed-loop arc](closed-loop-arc.md) retain
product priority and sibling ownership. This plan is cross-cutting test work,
not a new numbered phase of that arc. Sibling product changes remain sequenced
by their owning plans. The completed [Trust the Substrate](../completed/trust-the-substrate.md)
plan is historical context, not a backlog to reimplement.

## Strategic Decisions

### Findings verified while drafting

Inspected checkout: `97733090` on 2026-09-09; upstream was refreshed to
`9caf594b` before publication. Upstream also contains dataset operator work in
PR #442; refresh the selected base and sibling dashboards before execution.
No suite, live branch-protection
audit, or current runtime benchmark was performed as part of this inspection.

| Surface | Existing behavior and evidence | Gap this plan addresses |
| --- | --- | --- |
| Required CI | `scripts/ci-ok.py` rejects missing, failed, cancelled, and unexpectedly skipped required jobs; `test/shard_test.go` partitions the real suite. | `integration-extra`, Podman, Helm, and arm64 integration are outside the current aggregate. Promotion must be explicit and preserve coverage. |
| Distributed lane | `justfile` recipe `integration-up-distributed` starts one server with owner/worker mode enabled; `internal/run/failover_test.go` simulates lease expiry in a test DB. | These exercise distributed code paths but cannot demonstrate a real multi-node partition, quorum loss, or takeover after process failure. |
| Kubernetes lane | CI selects one replica without persistence. The existing `just k8s-distributed` recipe deploys three replicas through the Helm StatefulSet and headless discovery, but uses a Docker Desktop registry path and disables persistence. CI already provisions kind. | Reuse the chart and CI kind lifecycle with three replicas and persistent volumes first; do not run the local recipe unchanged against an arbitrary user cluster. |
| Release CLI | `scripts/ci-cli-smoke.sh` already tests the static CLI natively on amd64/arm64; publication checks its checksum markers. | D1 and release qualification extend these existing gates with complete developer/upgrade journeys. |
| UI | Vitest, live-backend Playwright, auth projects, and `ui/e2e/operator-flow.spec.ts` already exist. | `ui/playwright.config.ts` allows two CI retries; the workflow does not upload the generated browser diagnostics. Recovery, visual/accessibility, and scale coverage need expansion. |
| Performance | `test/load/harness.go` generates DAGs and reports run outcomes and metrics; `ui/scripts/check-bundle-size.mjs` budgets the largest JS chunk. | Failed run counts do not determine harness exit status. Sampling starts after a concurrency-limited submission loop, so it can miss much of the workload. There is no base/candidate regression gate or route-level total asset budget. |
| Coverage and generation | `just unit-test` runs root Go tests with race and coverage. Four native fuzz targets exist in three files; UI unit tests exist. | No sustained fuzz invocation or `Benchmark` functions were found. Coverage is not collected from the actual CLI/server exercised by integration and browser tests. |

These findings identify verification gaps, not proof that the affected product
paths are currently broken. Existing full suites and their expected engine
skips remain the starting point.

### Proposed guarantees to resolve in A1

Assign stable contract IDs and classify each as documented, inferred and needing
confirmation, or proposed product behavior. At minimum cover acknowledged-run
durability; one authoritative owner generation; stale completion rejection;
terminal state within an execution generation; frozen retry recipes; DAG and
fan-in rules; cancellation races; event delivery/deduplication; authorization;
and bounded recovery after the required quorum, capacity, and dependencies
return. A1 must also resolve behavior **during quorum loss**: measure request
cancellation and dqlite retry behavior, then record which mutation endpoints
return a bounded error and the deadline/status contract. Until that is reviewed,
bounded server rejection is explicitly undefined: client timeout alone does
not prove server cancellation or a rejected write. B3 records unresolved writes
as possibly committed and cannot claim bounded rejection for that operation. State the fault and clock assumptions for each bound.

Separate duplicate task attempts, duplicate terminal commits, and duplicate
external effects. The operations guide already documents at-least-once event
delivery. Do not infer exactly-once arbitrary task side effects from scheduler
fencing. Timed-out operations may have committed. Linearizability applies only
to operations whose contract supports a sequential specification; asynchronous
progress and eventual delivery need separate checkers.

### A1 decision record — W1-alpha, 2026-09-09

**Audit base: `5a89c851593930de95990b6c5dea0709bb9956aa`.** This is a
source/route/chart audit and an executable design for B1, not runtime fault
evidence. No cluster, proxy, clock, or quorum-loss experiment was run for A1.
`D` below means documented and traced in code; `I` means inferred from code and
awaiting conformance evidence; `U` means unresolved or stronger product behavior
that these tests must not silently promise. Even a D entry still needs an
executed scenario before it is coverage. Q1–Q6 remain open except for the narrow
B1 design choices here.

#### Operation identities and checkable contracts

The recorder assigns a unique **operation ID** before each HTTP invocation and
retains endpoint/node, arguments, invocation/response monotonic times, response
body/status, and transport errors. This ID is a recorder correlation key, not
a server idempotency token. Preserve job UUID, run UUID, task-run UUID, partition
identity, recorded attempt, lease owner/generation, and event sequence separately.
A catalog task UUID alone is ambiguous for fan-out; task `attempt` resets on
operator retry and is not a globally unique execution epoch. No server field
called `execution_generation` exists at this base. A2 must keep the following
contract IDs stable; C1 must model only the resolved subset of each entry.

| Contract / class | Operation and acknowledgement | Safety oracle and identity | Liveness, assumptions, and unresolved scope |
| --- | --- | --- | --- |
| DT-ADMIT-01 / I, safety | `POST /v1/jobs/:id/run`: 202 **with a run body and UUID** follows `Store.Start` and its run-creation transaction. A bare 202 can mean queued/skipped and is a different outcome. | The acknowledged run UUID remains publicly readable after the selected one-process crash and cannot become a different job. Record the job/run and request parameters; do not retry a lost response as if it were idempotent. | Quorum, retained storage, healthy executor, reachable fixture image and sink required. No HTTP admission latency bound. Lease acquisition happens **after** the run transaction and failure is logged, so 202 alone does not prove an owner lease or registered DAG. B1 waits for both. |
| DT-OWNER-01 / D+I, safety/liveness | Run creation, owner dispatch, and lease takeover; there is no public lease-mutation endpoint. The harness observes `run_leases` through the existing read-only database HTTP query. | `AcquireExpiredLeases` atomically changes the expired lease owner and increments `generation`; require one observed authoritative lease row, new surviving owner, and increasing generation. Distinguish this authority from already-running external attempts. | Lease expiry uses node wall clocks. B1 uses one host with no injected clock skew, one killed owner, two surviving voters, and no concurrent retry/cancel. Recovery is expected after expiry and successful dispatch/recovery; an exact production time bound is unmeasured. |
| DT-COMPLETE-01 / D+U, safety | `POST /internal/complete`, 200 with `accepted=true`; request carries run/task/task-run UUIDs, worker node and owner generation. | `internal/dispatch/dispatch.go` rejects wrong/expired owner and stale generation with 409 before the normal completion path, then checks task identity/claim. Preserve refusal reason and assert no task/state/effect mutation for a rejected stale request. | An HTTP precheck is not proof of an atomic lease-and-completion fence: takeover between validation and commit, especially `CompleteTaskOwner`, remains a B3 adversarial case. Do not call every old-generation race resolved from the handler checks alone. |
| DT-TERMINAL-01 / I+U, safety | Run/task completion observed by `GET /v1/jobs/:id/runs/:run_id` and partition reads; no standalone public completion API. | `CompleteIfActive` conditionally finalizes a nonterminal run and suppresses a second terminal event. Within a run execution with no retry, a terminal outcome must not regress. Partition identities and terminal sequences are checked separately. | Across-retry terminal fencing is **unresolved**: `readmitRetryTx` reopens the same run UUID; `retryResetColumns` clears claims/evidence and resets attempt to 1; checkpoint invalidation and task claim guards do not supply a durable run execution epoch to `CompleteIfActive`. No claim of stale-coordinator rejection across retry is made. |
| DT-RETRY-01 / D+I, compatibility/safety | `POST /v1/jobs/:id/runs/:run_id/retry` returns 202 after retry reset; partition retry uses `/tasks/:task_id/partitions/:index/retry`. These are distinct policies. | Reconcile retained succeeded instances and reset failed instances by task-run UUID. Targeted partition retry must satisfy the existing compatibility/quiescence checks and use frozen persisted recipe fields; rejected requests must not alter state. | Whole-run retry must not be assumed to have every targeted-partition restriction or every agent retry admission check. `RetryFromFailureAdmitted` and ordinary REST `RetryFromFailure` differ. Edit-after-failure, concurrent retries and stale completion need separate scenarios; no new replay or exactly-once guarantee. |
| DT-DAG-01 / D+I, safety/liveness | Job apply/create followed by run trigger and public DAG/partition/run reads; apply acknowledgement is not acknowledgement of a run. | Frozen instance identity, explicit edges, branch/trigger rules and whole predecessor groups govern readiness. A satisfied fan-in cannot be inferred from one successful partition. Assert starts against independently recorded predecessor completions for a selected deterministic fixture. | All required successful predecessors, capacity, engine and dependencies must return. Halt/continue/tolerant-rule behavior follows the execution base, not an unmerged sibling fix; no starvation-freedom promise. |
| DT-CANCEL-01 / D+U, safety/liveness | Trigger under concurrency `replace` reaches `CancelRun`; `DELETE /v1/jobs/:id/queue/:queue_id` cancels queued work. This base has **no standalone active-run cancel REST route**. | Observe old run/task rows become cancelled, claims cleared, and fixture task stop/effect timestamps. Preserve races with completion as distinct histories. A 202 for the replacement is not an acknowledgement that the old external process is already dead. | Local bus notification has periodic cancellation reconciliation; distributed execution checks claims. Progress requires runnable worker, reachable runtime/DB and enabled reconciliation. Neither zero post-cancel effects nor a universal stop deadline is established, especially during quorum loss. |
| DT-EVENT-01 / D+I, delivery | `GET /v1/events?run_id=...` with `Last-Event-ID`; persisted execution events are also observable through the read-only DB surface. First stream bytes are not an acknowledgement that a run finished. | Event store sequence identifies a persisted event **within the selected database/store lifetime**, not a gap-free global counter across clusters/shards. Keep duplicate and out-of-order live deliveries, filters and resume cursor. Check persisted rows versus catch-up and an independent raw effect ledger. | Pending durable events are replayed; delivery is at-least-once. In-process subscriber buffer overflow can drop delivery even when a row is marked dispatched; reconnect/catch-up and downstream reconciliation are separate from uninterrupted-stream reliability. Retention, recorder loss or missing store scope makes completeness inconclusive. |
| DT-AUTH-01 / D+I, authorization/safety | Public authenticated operations plus cross-node `POST /internal/dispatch` and `/internal/complete` on the dedicated mTLS listener; a TLS-authenticated peer with a wrong internal token returns 401 before DB/task handling. Public role/scope denial uses the configured middleware. | For each denied operation, record principal/scope without secrets and compare task/run/effect observations before/after. Test invalid certificate, valid-certificate/wrong-token, and valid-token stale generation separately. | Startup requires all explicit mTLS material paths or automatic provisioning from a shared token. This base enforces the dedicated TLS listener; the operations guide's older recommendation-only wording is stale. Public authorization scenarios must enable their actual auth configuration. |
| DT-RECOVER-01 / I, liveness | B1 owner process crash after DT-ADMIT-01 plus a verified lease and blocked DAG. | Surviving owner finishes the **same accepted run**, legal successors execute, final public state agrees with raw starts/completions, and restarted old member rejoins with retained volume. Duplicate task attempts are retained, not automatically failures. | Recovery needs majority, surviving capacity, readable durable state, no clock jump, healthy Kubernetes API and released sink barrier. A test watchdog is a finite regression limit, not a production SLO. No arbitrary-effect exactly-once, power-loss, disk-loss, partition or concurrent-retry claim. |
| DT-QUORUM-01 / U, availability/safety | All mutating routes in this table, especially trigger, retry, apply/create, replace/queue cancellation and internal completion. | A transport timeout/disconnect is **possibly committed**. Reconcile by recorded identities after healing; do not report a rejected mutation from client timeout or an empty response. | No mutation endpoint currently has an A1-verified bounded quorum-loss rejection/deadline/status contract. Missing response, late commit and late error remain legal uncertainty in B3 until the endpoint-specific experiment below resolves them. |

Code anchors for this record are `api/rest/bind/bind.go`,
`api/rest/controller/job/run/{post,retry,partitions}.go`,
`api/rest/service/run/run.go`, `internal/run/{store,store_instance,lease,owner_manager}.go`,
`internal/dispatch/{dispatch,loop}.go`, `internal/worker/runtime_executor.go`,
`internal/event/{store,bus,bus_dispatch}.go`, and
`api/rest/controller/event/stream.go`. These qualify the operations guide's
historical ClaimNext-only failover description: the current dispatch loop also
acquires expired owner leases and recovers owner state. No runbook changes are
claimed in A1; N-1 owns that reconciliation.

#### Quorum-loss cancellation evidence still required

`api/rest/service/run/run.go` stores the request context but its `Start`, `Get`
and `List` delegate to context-free store methods. REST whole-run retry similarly
calls `RetryFromFailure` without passing the request context. Job creation uses
`db.Transaction(ctx, ...)`; other context-aware operations pass contexts into
GORM, but that alone does not prove native-driver cancellation. The configured
dqlite busy timeout is 5 seconds; the eight contention backoffs in
`pkg/db/retry.go` sum to 2.27 seconds **before jitter and time spent in calls**.
These are not HTTP deadlines. Whole-transaction and pool retry layers, native
leader discovery, and `context.WithoutCancel` for event marking must not be
summed into an invented end-to-end bound. The internal listener has 15-second
read/write timeouts (`internal/dispatch/internal_server.go`); socket write expiry
does not cancel or roll back an already-running handler transaction.

After B1 supplies the owned cluster, a bounded follow-up records each selected
endpoint on the isolated minority and surviving side: send one identified
mutation while two voters are unreachable, use 5-second and 15-second client
deadlines in separate cases, hold the fault for at most 30 seconds, heal, then
observe for up to 120 seconds. Capture actual request cancellation/handler or
driver return evidence when available, HTTP status/body, retry logs/counters,
and post-heal public state. Repeat with response loss after an observable commit.
These durations bound the **experiment**, not the product. If existing diagnostics
cannot establish server cancellation, leave it unresolved; adding diagnostics
outside A1/B2's allowed event hook requires a scoped amendment. A 409 from a
retry handler or completion handler can wrap a DB error and does not by itself
prove an application-level refusal without state reconciliation.

#### B1 harness selection and command contract

Choose the existing Helm StatefulSet, headless discovery and kind lifecycle,
with three persistent Caesium replicas. No Testcontainers dependency or custom
database bootstrap is necessary. To make a process crash last through lease
takeover without deleting its network identity, the initial design uses **one
kind control-plane node and three kind workers on one CI host**, required pod
anti-affinity across worker hostnames, and one Caesium replica per worker.
The runner/recorder lives on the control-plane node, outside all faulted pods.
The recorder must not run on a stopped worker; cordon the intended owner worker
**before triggering the fixture**, and require every fixture task pod to run on
surviving workers. Kubernetes engine completion depends on kubelet publishing
PodSucceeded/PodFailed to the API; a task container on the stopped worker cannot
supply that evidence even if it exits. This costs
more containers than one kind node but allows a bounded, externally observed
SIGKILL and controlled restart without changing production code.

B1 creates its existing assigned `test-values-robustness.yaml` and generated
temporary kind/runner manifests. Set `replicaCount=3`, persistence enabled,
`kubernetes.engine.enabled=true`, `CAESIUM_EXECUTION_MODE=distributed`,
`CAESIUM_RUN_OWNER_ENABLED=true`, `CAESIUM_RUN_OWNER_IN_MEMORY=true`,
`CAESIUM_DATABASE_SHARDS=1`, `CAESIUM_DATABASE_VOTERS=3`,
`CAESIUM_DATABASE_STANDBYS=0`, shared test-only internal/manual API keys,
`CAESIUM_DATABASE_CONSOLE_ENABLED=true`, run lease TTL `30s`, dispatch interval
`1s`, worker lease TTL `30s`, reclaim interval `1s`, and worker pool size `2`.
Record effective environment on every member. Use bounded polling and an initial
120-second recovery watchdog after the fault/barrier release; calibrate it in
B1 and report its result without presenting 120 seconds as a production promise.
Do not inherit unrelated data-plane/cache gates from a mutable sibling values file.
For B1, leave all three explicit mTLS path settings unset and use a generated
shared internal token of at least 32 bytes: `cmd/start/start.go` provisions the
cluster CA and node certificates through `internal/dispatch/pki` and serves peer
traffic on `https://podIP:8443`. Require successful provisioning on all members
and actual dispatch/capability traffic before faulting. The dqlite membership
RPC on 9001 is separate from this HTTPS listener; the test helper under B1's
owned `test/robustness/cluster/` can use the existing dqlite client without the
dispatch client certificate. B2 authenticated negative cases must obtain valid
test material using the supported provisioning/static-material paths, then vary
the token or certificate independently; no TLS-verification bypass.

The following is the exact **proposed B1 interface**, not commands already shipped
or run in A1. B1 must implement these inputs and fail-closed observations in
`scripts/robustness.sh` and `build/Dockerfile.robustness`, then record actual
image IDs/digests, kind node image digest, versions and exit statuses in its PR.
`TASK_IMAGE` and `KIND_IMAGE` must be resolved/pinned for the test architecture,
not guessed digests. Build the candidate without image-skip mode:

```sh
CANDIDATE_SHA=$(git rev-parse HEAD)
ROBUSTNESS_ID="robustness-$(uuidgen | tr '[:upper:]' '[:lower:]')"
ARTIFACTS=$(mktemp -d)
CAESIUM_SKIP_IMAGE_BUILD=false just tag="$CANDIDATE_SHA" build-release
docker image inspect "caesiumcloud/caesium:$CANDIDATE_SHA"
docker build --build-arg "BUILDER_IMAGE=caesiumcloud/caesium-builder:$CANDIDATE_SHA" \
  --build-arg "CAESIUM_IMAGE=caesiumcloud/caesium:$CANDIDATE_SHA" \
  -f build/Dockerfile.robustness -t "caesiumcloud/caesium-robustness:$CANDIDATE_SHA" .
CAESIUM_ROBUSTNESS_ID="$ROBUSTNESS_ID" CAESIUM_ROBUSTNESS_ARTIFACTS="$ARTIFACTS" \
  CAESIUM_ROBUSTNESS_IMAGE="caesiumcloud/caesium-robustness:$CANDIDATE_SHA" \
  CAESIUM_ROBUSTNESS_SERVER_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
  CAESIUM_ROBUSTNESS_KIND_IMAGE="$KIND_IMAGE" CAESIUM_ROBUSTNESS_TASK_IMAGE="$TASK_IMAGE" \
  bash scripts/robustness.sh
```

The script uses `kind create cluster --name "$ROBUSTNESS_ID" --image "$KIND_IMAGE"
--config "$ARTIFACTS/kind.yaml" --kubeconfig "$ARTIFACTS/kubeconfig" --wait 120s`;
every `kubectl`/Helm call supplies that kubeconfig explicitly. Load candidate,
runner and task images using `kind load docker-image --name "$ROBUSTNESS_ID"`.
Install with `helm install caesium ./helm/caesium --kubeconfig
"$ARTIFACTS/kubeconfig" --namespace "$ROBUSTNESS_ID" --create-namespace --values
helm/caesium/ci/test-values-robustness.yaml --set image.tag="$CANDIDATE_SHA"
--wait --timeout 240s`. Verify three bound PVCs, three distinct pod UIDs/IPs and
worker nodes, and running image IDs matching the loaded candidate per platform.
A healthy `/health` or three Ready pods is insufficient membership proof.

Inside the builder, compile `go test -tags=integration -c ./test/robustness
-o /out/robustness.test`; root `./test` does not contain this package. The runner
image includes the test binary, matching CLI, runtime libraries and recorder.
An in-cluster runner pod executes `/bin/robustness.test -test.v
-test.run '^TestOwnerCrash$' -test.timeout 15m`, with required `owner_is_leader`
and `owner_is_not_leader` subtests counted explicitly. Its HTTP sink listens on
`0.0.0.0:8090`, exposed by an owned `robustness-recorder` Service; test task pods
use `http://robustness-recorder:8090`. Require a real task pod to POST/GET a
unique connectivity probe before triggering the fault fixture. The controller
copies raw append-only records to `$ARTIFACTS`; missing recorder data is
inconclusive. Runner RBAC is restricted to its owned namespace; host controller
alone owns kind-node process control.

For discovery, the runner invokes the existing go-dqlite client dependency's
`Leader` and `Cluster` RPCs directly against each advertised `podIP:9001`, with
bounded contexts. Require three distinct members with voter roles and an agreed
leader, plus a successful HTTP write/read. `GET /v1/system/nodes` supplements
membership with seed/local fallbacks and does not expose the leader, so it is
diagnostic only. No new product API is needed. Apply the blocked two-step fixture
through `POST /v1/jobdefs/apply` or the candidate CLI. Select the intended owner
pod, map its kind worker, and cordon that worker **before** triggering any fixture
task. Trigger directly against
the selected leader/nonleader pod's HTTP address, retain its 202 run UUID, and
query the actual lease via the existing read-only surface:

```sh
curl --fail-with-body -H 'Content-Type: application/json' \
  --data "{\"sql\":\"SELECT run_id, owner_node, generation, lease_expires_at FROM run_leases WHERE run_id = '$RUN_ID'\",\"limit\":1}" \
  "http://$SURVIVOR_IP:8080/v1/database/query"
```

This request runs from the in-cluster runner with any required public credentials;
the documented SQL interpolates only a validated returned UUID. `claimed_by`
identifies the worker, not the run owner. The fixture sends `CAESIUM_RUN_ID`, an
explicit step name and a newly generated attempt nonce to the sink before
blocking; the sink persists that start before releasing the response. Run/task
public reads correlate the nonce/step with the actual task-run identity. Require
lease, ready topology and blocked-effect evidence before killing the mapped
owner, and assert every pre-fault fixture task pod is placed on a surviving
worker. Refresh the leader immediately before the kill; a changed owner/leader
relationship invalidates that subcase rather than passing the wrong case.

The owned host controller maps `$OWNER_POD` to `$OWNER_KIND_NODE` and extracts
its current Caesium container ID from `status.containerStatuses`. With all
identities checked against the created cluster, use:

```sh
# Before fixture trigger (not merely before SIGKILL):
kubectl --kubeconfig "$ARTIFACTS/kubeconfig" cordon "$OWNER_KIND_NODE"
# After triggering and verifying owner, task placement and blocked-effect evidence:
docker exec "$OWNER_KIND_NODE" systemctl stop kubelet
docker exec "$OWNER_KIND_NODE" ctr -n k8s.io tasks kill --signal SIGKILL "$OWNER_CONTAINER_ID"
docker exec "$OWNER_KIND_NODE" ctr -n k8s.io tasks list
# After survivor takeover/completion observations, including fault evidence:
docker exec "$OWNER_KIND_NODE" systemctl start kubelet
kubectl --kubeconfig "$ARTIFACTS/kubeconfig" uncordon "$OWNER_KIND_NODE"
```

Strip the verified `containerd://` prefix before passing the container ID.
Require observed process exit/stopped task before the controller opens the sink
barrier, then generation increase and completion on a survivor while the old
kubelet remains stopped. Register restart cleanup **before** stopping kubelet.
The untouched pod sandbox and PVC preserve address/storage for rejoin; verify
that they actually do. Failure to restart/rejoin is a failing or blocked B1
result, not permission to silently replace the disk. The script must diagnose
an unavailable `systemctl`/`ctr` in the selected node image before faulting.
`kubectl delete --force` alone does not prove process death. Teardown deletes
only the recorded cluster/name and artifact-owned resources. Demonstrate two
distinct IDs concurrently before claiming isolation; neither uses host API ports
or the user's current Kubernetes context.

#### Proxy and clock decision; EX-HOOKS handoff

The bounded **static** proxy check inspected the chart's discovery ConfigMap,
StatefulSet environment, `pkg/dqlite/dqlite.go`, and
`internal/dispatch/loop.go` at this base. Bootstrap seeds use headless DNS, but
`CAESIUM_NODE_ADDRESS=$(POD_IP):9001` is passed to dqlite `WithAddress`; dispatch
derives peers from the advertised host and, in production owner mode, the
dedicated HTTPS internal port 8443 (API-port HTTP is the test/non-mTLS fallback).
Replacing only
`CAESIUM_DATABASE_NODES` with Toxiproxy seeds therefore does not establish that
later Raft or worker routes cross the proxy. **Do not adopt proxies in B1.**
A live routing spike remains unrun: budget one owned cluster and at most 30
minutes, record discovered addresses and per-proxy connection counters, disable
one proposed peer route in each direction, and verify both Raft and worker HTTP
traffic use the injector with no direct bypass before/after rejoin. Failure or
insufficient visibility selects external namespace/network fault controls for
B2, subject to their own activation proof; it does not authorize an address
refactor or call a seed-only proxy a partition test.

| Clock option | Decision and limits |
| --- | --- |
| Injectable `Now`/timer seam through lease store, dispatch/owner renewal and worker renewal | Deferred. All participants and expiry comparisons would need the same explicit clock contract and quiescence control. It adds overlapping production-file ownership without helping the first real crash/commit scenario; any later proposal needs an enumerated amendment and sibling handoff. |
| Existing Go `testing/synctest` | Use C2 for isolated Go timer/channel/cancellation logic. The pinned builder is Go 1.27.1; the [Go documentation](https://go.dev/blog/testing-time) describes fake time and bubble quiescence. Real sockets, process lifetime and native dqlite do not become deterministic. No full-cluster simulated-time claim. |
| Short test-only lease/config intervals | Use supported environment settings in B1 with measured fault/expiry observations and a generous watchdog. Shortening a lease can create contention artifacts; keep defaults unless a measured runtime need justifies the recorded change. It does not simulate skew or prove a hard recovery SLO. |
| External process STOP/CONT | B2 can freeze an actual owner past its lease and resume stale work without changing the host clock. Record signals and observed process state. Pause is distinct from SIGKILL, disk failure and clock skew; none substitutes for the others. |

The only justified production boundary for B2 is **after an event is durably
committed but before its first `bus.Publish`**. In `internal/event/bus_dispatch.go`
both `PublishAndMarkBusDispatched` (after its deferred-transaction early return)
and `BusDispatcher.DispatchOnce` (after reading pending rows, before the publish
loop) can publish the same event. A test-only predicate keyed by run/event must
cover both paths so the background dispatcher cannot bypass an armed pause.
Arm it on every publisher process before the fixture operation, observe the committed row independently,
record entry into the hook, kill that process, then disarm survivors/restart and
check durable replay. Store marking remains after publication. No store, lease,
owner completion, worker runtime or new request hook is allocated. Controls must
be absent/inert in release images and not expose an unauthenticated product fault
endpoint; B2 must demonstrate a release-image baseline.

EX-HOOKS snapshot: the last commit touching `bus_dispatch.go` is
`573edfee95c3a93d8c5058c1b067a27b6e28f915` (PR #423), present in this base.
PR #442 is merged at `995ae8b3c29208c7a87ce6f028460d0c3256cd9a`.
Read-only inspection of all eight open PR file lists on 2026-09-09 (#449, #450,
#454, #456, #459, #460, #462, #463) found no `bus_dispatch.go` writer.
The active arc, circuit-breaker, right-sizing, backtesting and window plans
reserve neighboring `bus.go`, notification, runtime and metrics surfaces, not
this dispatch file. In particular #459 changes event types/startup, and #449
changes runtime/resource and CI surfaces; neither is a transferred ownership
claim. **Current file owner for the proposed hook is distributed-testing B2,
but future dispatch still requires a refreshed open-PR/base check and exclusive
reservation.** This snapshot is not a perpetual handoff or evidence that those
open sibling PRs merged. No review messages were sent.

### F1 decision record — W3-ε, 2026-09-14

**Audit base: `f3f5ce3ab68d57709127a0b2807e670e7d39726a`.** This is a
source, chart, artifact and registry audit plus two bounded local container
experiments against the published `v0.1.0` image. It is a lifecycle design and
policy record, not upgrade certification: no cluster upgrade, no rollback, no
membership replacement and no restore was executed for F1. `D` means documented
and traced in code, `I` means inferred from code and awaiting executed evidence,
`U` means unresolved or stronger behavior these tests must not silently promise.
Q4 is resolved below; Q1–Q3, Q5 and Q6 remain open. Where F1 records a missing
capability, F2/F3/F4 must report it blocked with evidence; none of them may ship
a runnable placeholder that passes by asserting nothing.

#### Schema evolution mechanism and its limits

There are **no hand-written SQL migration files and no schema-version table**.
`pkg/db/Migrate()` (`pkg/db/db.go:236`) calls `conn.AutoMigrate(model)` once per
entry of `internal/models.All` (`pkg/db/db.go:293`); the struct tags in
`internal/models/` are the schema. A repository-wide grep for `schema_version`
returns only unrelated JSON payload versions (`internal/models/run.go:239`,
`internal/reproduce/reconstruct.go:172`), never a migration ledger. The schema a
database ends up with is therefore a pure function of the binary that last
booted against it, and nothing on disk records which binary that was.

Consequences the lifecycle tests must respect:

- **Exactly one explicit DDL step exists.**
  `MigrateTaskRunUniquePartitionIndex` (`pkg/db/migrations.go`) idempotently
  drops a pre-fan-out `idx_taskrun_jobrun_task` so AutoMigrate can recreate it
  in its UNIQUE `(job_run_id, task_id, partition_index)` shape. It runs on the
  catalog, every hot shard and the cold database before AutoMigrate
  (`pkg/db/db.go:238-254`).
- **AutoMigrate is additive and name-blind.** It adds tables, columns and
  indexes and never drops a column, and it matches an index by NAME only, so a
  re-shaped index that keeps its name survives forever unless an explicit repair
  precedes it — the defect `pkg/db/migrations.go:17-47` exists to fix. Any future
  re-shape needs its own repair step; F2/F4 may not assume the generic path
  covers one.
- **Every node migrates, unconditionally, on every boot.**
  `cmd/start/start.go:163-165` calls `db.Migrate()` with no leader gate and
  `log.Fatal`s on failure. In a three-member cluster all three members issue this
  DDL through Raft during a rolling upgrade. **U**: concurrent AutoMigrate from a
  mixed-version fleet is untested; F2 must capture each member's migration log
  and any DDL error rather than assuming serialization.
- **Shard count is part of the on-disk identity.** The logical databases are
  `caesium`, `caesium_history` and `caesium_hot_%02d` (`pkg/db/db.go:25-27`) and
  a run's rows live in `fnv32a(run_id) % len(hot)` (`pkg/db/router.go:148-156`).
  `docs/database-sharding.md` records that increasing `CAESIUM_DATABASE_SHARDS`
  does not rebalance existing hot rows. **DECISION:** the shard count is frozen
  for the life of a data directory. F2/F4 must assert the value is byte-identical
  on both sides of every transition and must treat a changed count as an
  unsupported transition, not a configuration option.

**Measured delta for the qualified pair.** `GET /v1/database/schema` against
`caesiumcloud/caesium:v0.1.0` on a fresh volume returns dialect `dqlite`,
`sqlite_version` 3.53.4 and 40 catalog tables. `internal/models/models.go` at
this base adds two: `dataset_metrics` and `dataset_holds`. `git diff v0.1.0..HEAD
-- internal/models/` adds only these columns to existing tables:
`callback_runs.http_status/response_body/retry_count`,
`dataset_declarations.assertions_json/on_violation/release`,
`jobs.on_upstream_hold`, `job_runs.skip_reason` and
`task_runs.data_violations`. **The last two live on different tables**:
`SkipReason` is a field of `models.JobRun` (`internal/models/run.go:43`) and
`DataViolations` is a field of `models.TaskRun` (`internal/models/run.go:165`);
there is no `job_runs.data_violations` column and never will be. `versions.json`
must carry the delta as explicit `table -> column` pairs, not a shared prefix, and
the F4 schema assertion must look for each column on the table named here —
asserting `data_violations` on `job_runs` would fail a correct upgrade while
leaving the real `task_runs` column unverified. Every added column is
either nullable or carries a non-null DEFAULT, so a v0.1.0 binary can still
INSERT into a candidate-migrated table — a necessary but **not** sufficient
condition for rollback. `TaskRun`'s composite index tag is byte-identical at
v0.1.0 and at this base, so this pair does **not** exercise the index repair:
the first qualified upgrade is a purely additive case and F4 must label it so
rather than implying general migration coverage. `go.mod` is unchanged between
v0.1.0 and this base, so both sides link go-dqlite v3.0.4 and share the on-disk
raft/dqlite format.

#### Release artifacts, version identity, and the qualified pair

- **One release exists.** `gh release list` returns only `v0.1.0`, published
  2026-09-08 at tag `16a4c8fcfd7580e4eec2c0c79a7a54e237c926c0`; this base is 46
  commits ahead. There is no release-to-release pair, so the only old→new path
  that can be tested today is `v0.1.0 → candidate SHA`.
- **Release assets** (`gh release view v0.1.0`, and the published `SHA256SUMS`):
  `caesium-linux-amd64` sha256 `b890ca8d1a76420ef04c81dbc20612da57cf415c9f465e6cb98b241475df552c`,
  `caesium-linux-arm64` sha256 `8b9b924288c9ae77ec8f167cc33cb77ecb05238abee92a95474f9ff810cea586`.
- **Published images** (Docker Hub tag API): multi-arch
  `caesiumcloud/caesium:v0.1.0` = `sha256:2e6996f965ab7899ac3f2d80a7607e26a96ff607d24baf8566033d6d7aa73917`,
  amd64 `sha256:d93d21e776665039bbf5cc3dc41be7a0b402ae9f2e83d446f2fb05a16d52288b`,
  arm64 `sha256:56569860e7bfc33ca84998648a0d0d72bb87c35194299cb02f54da52104d932d`.
  The `publish` job (`.github/workflows/ci.yml`) pushes only the `release`
  target plus the reagent images; the DinD `-test` image and the integration
  runner are never published, so a previous release cannot supply a DinD server.
- **Registry trap.** The `latest` tag in that repository was last pushed
  2021-03-23 and is amd64-only. It is not a release and the publish chain never
  updates it. No lifecycle fixture may reference `latest`; pin `v0.1.0` and
  verify the resolved digest.
- **There is no version surface.** `caesium --version` returns
  `Error: unknown flag: --version`, there is no version subcommand
  (`caesium --help`), and `api/rest/bind/bind.go` binds no version route —
  `/v1/system/nodes` and `/v1/system/features` are the only system reads. Build
  identity must therefore be established **externally**: the pinned image
  digest, the container's running image ID, and in Kubernetes
  `status.containerStatuses[].imageID`, exactly as B1 already does
  (`test/robustness/cluster/imageid.go`). A useful secondary, behavioral
  discriminator for this pair only: `GET /v1/system/features` gains the key
  `data_assertions_enabled` at this base and lacks it on v0.1.0 (observed).
- **Internal protocol.** `InternalProtocolVersion = 2`
  (`internal/dispatch/dispatch.go:98`) and the same constant holds at v0.1.0, so
  the qualified pair is protocol-compatible in both directions. The code records
  that an OLD owner dispatching to a NEW peer fails closed with 409
  `ReasonAmbiguousTask`, while a NEW owner dispatching to a **pre-v2** peer is
  unguarded since the capability probe was removed for #358 (commit `bd0d5416`,
  PR #372) — see the comment at `internal/dispatch/dispatch.go:78-93`.
  `GET /internal/capabilities` reports a peer's `protocol_version`.

**DECISION (supported version paths).** Supported old→new is the single adjacent
pair `v0.1.0 → candidate`, on one node and on three nodes. Each future release
adds exactly one more adjacent pair. Skip-version upgrades (N-2 → N) are
**unsupported** until a test exercises them. Mixed-version operation is supported
only between builds that both advertise internal protocol 2 and run the same
`CAESIUM_DATABASE_SHARDS`; introducing a pre-v2 member is unsupported and must
not be tested as if it were.

#### Q4 decisions

| Q4 sub-question | Decision | Evidence |
| --- | --- | --- |
| Shipped CLI platforms | `linux/amd64` and `linux/arm64` only, statically linked. No darwin or windows binary; macOS runs the CLI through the container image. D1 adds no native platform. | `build/Dockerfile` `cli-static` stage (CGO against musl, `GOOS=linux`); the publish job's release notes; open enhancement #418 "Build a darwin `caesium` CLI binary via a separate toolchain path". |
| Supported browsers | Chromium only. `ui/playwright.config.ts` declares two projects (`default`, `auth`) and sets no `browserName`, so both run Playwright's Chromium default. D2 keeps Chromium required and adds **no** Firefox/WebKit project in this plan; adding one is a separate product-support decision with its own runner cost under Q1. | `ui/playwright.config.ts`. |
| Previous release(s) | Exactly `v0.1.0`, pinned by digest. One adjacent pair only. | `gh release list`; Docker Hub tag API. |
| Mixed-version operation | Permitted only within internal protocol 2 and an identical shard count; F2 may run a mixed window **during** a rolling upgrade but may not claim general N-1/N interoperability, and must not introduce a pre-v2 member. | `internal/dispatch/dispatch.go:78-99`. |
| Rollback vs restore | **Restore-from-snapshot is the *intended* recovery; binary rollback is not.** Restore is not yet qualified: until F2's restore case carries pre-startup snapshot evidence and a passing negative control (below), a rejoin that reads matching rows is peer catch-up, not proof of restore, so this row states policy rather than a demonstrated guarantee. AutoMigrate never removes what it added and no down-migration exists, so a candidate-migrated data directory cannot be returned to its old schema. F4 still runs a rollback attempt as an explicitly labelled exploratory case and records the observed outcome; a passing attempt does not become a product guarantee without a recorded product decision. | `pkg/db/db.go:236-293`; the additive-only delta above. |
| Backup/restore support | **Absent as a product capability** — an external prerequisite, not a test gap. There is no backup or restore command, route or scheduler; `docs/kubernetes-deployment.md:199-204` delegates to storage-platform tooling. `caesium job export` reconstructs a job manifest only (`cmd/job/export.go`), and `/v1/database/query` is read-only and capped at 1000 rows (`api/rest/service/database/database.go:22-32`), so neither is a state backup. "Restore" in F2 therefore means a **storage-level** snapshot/copy of a stopped member's volume taken by the harness. | as cited. |
| Storage failure model | Four distinct cases, enumerated below. No process-kill test may be labelled power-loss or disk-loss qualification. | below. |

#### Failure and durability model — four distinct cases

These are separate faults with separate evidence; F2/F3 must name which one a
scenario actually injects.

1. **Process kill (SIGKILL) with the filesystem intact.** The only case B1
   exercises today: the volume and every byte already written to it survive, so
   recovery is ownership takeover, already contracted as DT-OWNER-01 and
   DT-RECOVER-01. **D**. What a *page-cache-resident* write survives here is
   case 3's question, not this one's.
2. **OS or power loss.** The host disappears between fsync boundaries. Caesium
   does not configure dqlite's snapshot parameters — `pkg/dqlite/dqlite.go:164-175`
   passes only address, cluster, voters, standbys, log func and busy timeout — so
   raft snapshot/trailing thresholds are the unconfigured dqlite C defaults and
   are not readable from Go. The `PRAGMA synchronous=NORMAL` in
   `pkg/dqlite/dqlite.go:196-208` applies only to the injected-connection
   (non-native) path; go-dqlite rejects SQL PRAGMAs. **U**: no measured
   power-loss durability bound exists, and F3 may not claim one from a
   container kill.
3. **Lost unflushed writes / torn tail.** `pkg/dqlite/dqlite.go` does **not**
   pass `WithAutoRecovery`, so go-dqlite's default `AutoRecovery: true` applies
   (`go-dqlite/v3@v3.0.4 app/options.go:279-291`), documented as: "raft snapshots and segment files
   may be deleted at startup if they are determined to be corrupt … can lead to
   data loss." A node that lost its unflushed tail therefore may silently
   truncate at startup rather than failing loudly. On one node that can lose an
   acknowledged write; with a healthy quorum the survivor log re-replicates it.
   **U, safety-relevant.** F3 must record whether auto-recovery fired (it is
   visible only in the dqlite log stream) and must never treat a silent start as
   proof of retention.
4. **Actual disk loss.** The volume is gone. Recovery is member replacement, for
   which no product surface exists (below). Distinct from every case above and
   from a PVC that merely detaches.

Related shutdown facts: `cmd/start/shutdown.go:55` closes the GORM router's
connection pools only. Nothing in the repository calls the dqlite app's `Close`
or `Handover` (grep for `Handover`/`dqliteapp` outside `pkg/dqlite/dqlite.go`
returns nothing), so **every** termination — including a planned rolling upgrade
— is an ungraceful Raft departure with no leadership or voter handover. Role
rebalancing is left to go-dqlite's 30-second `RolesAdjustmentFrequency` default.

#### Blocking finding — node address identity survives the volume, not the pod

The chart sets `CAESIUM_NODE_ADDRESS` to `$(POD_IP):9001`
(`helm/caesium/templates/statefulset.yaml:103-105`), while go-dqlite records the
node's address in `info.yaml` inside the data directory and **refuses to start**
if a supplied address differs: `go-dqlite/v3@v3.0.4 app/app.go:117-121` returns
`address %q in info.yaml does not match %q` whenever `info.yaml` exists and the
supplied `WithAddress` disagrees with it.

Verified locally on the published release, not inferred:

```sh
docker volume create f1auditvol
docker run -d --name a -v f1auditvol:/var/lib/caesium/dqlite caesiumcloud/caesium:v0.1.0 start
# /health healthy; info.yaml records Address: 127.0.0.1:9001
docker rm -f a
docker run -d --name b -e CAESIUM_NODE_ADDRESS=127.0.0.2:9001 \
  -v f1auditvol:/var/lib/caesium/dqlite caesiumcloud/caesium:v0.1.0 start
# exits 1: {"level":"fatal","caller":"db/db.go:56","msg":"failed to connect to
#   database","error":"address \"127.0.0.1:9001\" in info.yaml does not match
#   \"127.0.0.2:9001\""}
```

A StatefulSet pod keeps its PVC across recreation but **not** its IP: Kubernetes
guarantees a stable network *identity* (the headless-service DNS name), never a
stable pod IP. Any pod recreation — `helm upgrade`, node drain, eviction,
rescheduling — therefore starts a coin flip on whether the CNI happens to hand
back the same address; when it does not, `caesium start` exits 1 and the pod
CrashLoopBackOffs with its data intact. B1's harness never meets this because it
kills the *container* with `ctr` and leaves the pod sandbox (and its IP) in
place, exactly as the A1 record describes; the CI Helm lane never meets it
because it runs one replica with `persistence.enabled: false`
(`helm/caesium/ci/test-values-k8s.yaml`), so every boot writes a fresh
`info.yaml`. `helm upgrade` is not executed anywhere in CI at all — the only
invocation in the repository is `just k8s-distributed`, which also sets
`persistence.enabled=false` (`justfile:1074-1100`).

**DECISION:** this is a product defect with an external prerequisite
("stable dqlite node address", below), not a test defect. F2 must therefore be
written to *expose and classify* it rather than to pass: record every member's
`info.yaml` address before the transition and its pod IP after, and label an
upgrade that succeeded only because the CNI reused the address as **not**
evidence of a supported upgrade.

#### External product prerequisites (not runnable placeholder tests)

1. **Backup and restore.** No command, route or scheduled capability. Until one
   exists, "restore" means a harness-taken storage snapshot of a stopped
   member's volume. F2's restore case is *blocked* if the harness cannot take a
   consistent copy; it is never skipped as passing.
2. **Member removal / replacement.** Nothing in the repository calls go-dqlite's
   `client.Remove`, and `api/rest/bind/bind.go` binds no membership-mutating
   route; `GET /v1/system/nodes` is read-only. The new-ID/join path is
   **ordinal-dependent, not universal**: `app.New` generates a new ID and writes
   the join marker only when a cluster address list is supplied
   (`go-dqlite/v3@v3.0.4 app/app.go:96-111` — `if len(o.Cluster) == 0 { info.ID =
   dqlite.BootstrapID }`), and the chart's peer-discovery script always writes an
   **empty** peer list for ordinal 0
   (`helm/caesium/templates/configmap.yaml:14-17`). A replaced pod with a fresh
   PVC therefore behaves differently by ordinal:
   - **Ordinals ≥ 1** get a non-empty `--cluster` list, generate a new node ID
     and join; the dead member's ID stays in the cluster and can only be demoted
     by go-dqlite's role adjustment, never removed. F2 records the resulting
     membership as evidence.
   - **Ordinal 0** gets an empty list, so a fresh directory takes the fixed
     `dqlite.BootstrapID` and initializes a **self-only** node store instead of
     joining the surviving quorum. There is no product rejoin or re-bootstrap
     procedure for this; a stable pod address does not help, because the defect
     is the empty peer list, not the address.
   No test may therefore promise that "a replaced member joins" without naming
   the ordinal it replaced; recovering a lost ordinal 0 is prerequisite 5 below.
3. **Build/version reporting.** No `--version`, no version subcommand, no
   version route. Every lifecycle assertion of "which build is serving" depends
   on external image identity.
4. **Stable dqlite node address across pod recreation.** Per the finding above.
   Resolving it is product work (a stable per-ordinal address, or reconciling
   `info.yaml` on start); this plan does not authorize that change.
5. **Ordinal-0 rejoin / re-bootstrap after disk loss.** Per item 2: the chart
   hands ordinal 0 an empty peer list unconditionally
   (`helm/caesium/templates/configmap.yaml:14-17`), so a `caesium-0` replaced
   with a fresh PVC self-bootstraps under `dqlite.BootstrapID` instead of
   rejoining the surviving quorum. There is no command, route or chart path that
   recovers it. Resolving it is product work (seed ordinal 0's peer list from its
   siblings when its data directory is empty, or supply an explicit rejoin
   procedure); this plan does not authorize that change, and F2's ordinal-0 case
   reports **blocked-by-prerequisite**.

None of these may be replaced by a test that asserts nothing. A scenario that
depends on an unresolved prerequisite is reported **blocked** with its evidence.

#### F4 procedure — single-node previous-release upgrade qualification

Owns `test/lifecycle/standalone_test.go`, `test/lifecycle/versions.json` and
`scripts/lifecycle-tests.sh`. Integration-tagged (`//go:build integration`);
compiled explicitly in the builder, since the precompiled `./test` runner does
not discover subpackages. F4 does **not** need a cluster, B3, or new paid
infrastructure. `test/lifecycle/versions.json` is the version matrix: for each
pair, the previous image reference **and resolved digest**, the release asset
checksums, the protocol version both sides advertise, the shard count, and the
expected table delta expressed as explicit `{"table": ..., "column": ...}` pairs
(so `job_runs.skip_reason` and `task_runs.data_violations` stay on their own
tables). F4 must not edit `test/contracts/scenarios.json`; that
file's writer order is A2 → G3 → B3 → D3 → G6, and G6 registers the lifecycle
scenarios.

```sh
CANDIDATE_SHA=$(git rev-parse HEAD)
LIFECYCLE_ID="lifecycle-$(uuidgen | tr '[:upper:]' '[:lower:]')"
ARTIFACTS=$(mktemp -d)
PREV_IMAGE="caesiumcloud/caesium:v0.1.0"
PREV_DIGEST="sha256:2e6996f965ab7899ac3f2d80a7607e26a96ff607d24baf8566033d6d7aa73917"
CAESIUM_SKIP_IMAGE_BUILD=false just tag="$CANDIDATE_SHA" build-release
docker image inspect "caesiumcloud/caesium:$CANDIDATE_SHA"
docker pull "$PREV_IMAGE" && docker image inspect "$PREV_IMAGE" \
  --format '{{index .RepoDigests 0}}'   # must resolve to $PREV_DIGEST
docker volume create "$LIFECYCLE_ID-data"
CAESIUM_LIFECYCLE_ID="$LIFECYCLE_ID" CAESIUM_LIFECYCLE_ARTIFACTS="$ARTIFACTS" \
  CAESIUM_LIFECYCLE_PREV_IMAGE="$PREV_IMAGE" \
  CAESIUM_LIFECYCLE_CANDIDATE_IMAGE="caesiumcloud/caesium:$CANDIDATE_SHA" \
  bash scripts/lifecycle-tests.sh
```

Volume and identity rules, verified locally: a **fresh named volume** inherits
`10001:10001` from the release image's `/var/lib/caesium/dqlite`
(`build/Dockerfile`, `release` stage), so no `--user` override is needed and the
production UID is preserved. A host **bind mount** does not inherit that
ownership and is forbidden. If the lane must grant the Docker socket, prefer
`--group-add "$(stat -c '%g' /var/run/docker.sock)"`; if `--user 0:0` is used
instead it must be used identically on **both** sides, and the choice recorded.
Each side's matching CLI is extracted from its own image with
`docker cp "$ctr":/bin/caesium` — the pattern `just integration-test-podman` and
the CI Helm lane already use — so the old release is driven by the old CLI.

Both containers run with the same env block, of which
`CAESIUM_DATABASE_CONSOLE_ENABLED=true` (schema assertions),
`CAESIUM_MANUAL_TRIGGER_API_KEY`, `CAESIUM_RUN_QUEUE_ENABLED=true`,
`CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED=true` and
`CAESIUM_RUN_QUEUE_DEQUEUE_INTERVAL=500ms` are load-bearing. `git diff
v0.1.0..HEAD -- pkg/env/env.go` adds variables but removes and renames none, so
the same block is valid on both sides; the six candidate-only variables
(including `CAESIUM_DATA_ASSERTIONS_ENABLED`) are inert on v0.1.0 and no fixture
may depend on the old release honoring them.

**Retained-state fixture, created through v0.1.0's own public surface** (job
YAML per `docs/caesium-job-llm-reference.md`, steps on `alpine:3.23`):

| Class | How it is created | Recorded identity |
| --- | --- | --- |
| Definitions | `caesium job apply` two jobs, one with `concurrency` queue behavior | job UUIDs, aliases, exported manifests |
| Succeeded history | trigger one run to completion | run UUID, task-run UUIDs, terminal status and timestamps |
| Failed history | trigger a run whose step exits non-zero | run UUID, recorded error, retry eligibility |
| Queued work | trigger the queued job while its predecessor is running | `run_queue` row id, `claimed_by`/`claimed_at` |
| Events | the runs above | for each selected store and run, the full pre-upgrade set of `(sequence, type, task_id, payload)` tuples read from `GET /v1/events?run_id=`, plus the explicit resume cursor chosen below the lowest retained sequence — **not** a high-water mark |
| Schema | `GET /v1/database/schema` | the 40-table list |

Stop with `docker stop -t 60` so the 30-second grace period
(`cmd/start/shutdown.go:15`) can run, then start the candidate on the same
volume and assert:

1. The candidate reaches `/health` healthy, logs `migrating database`
   (`cmd/start/start.go:163`), and exits non-zero nowhere.
2. `GET /v1/database/schema` lists every table the v0.1.0 snapshot listed, plus
   exactly `dataset_metrics` and `dataset_holds`, and each added column above is
   present on its table.
3. Every recorded job/run/task-run UUID is readable with unchanged terminal
   status, error text and timestamps; the failed run is still failed.
4. Replaying `GET /v1/events?run_id=` with `Last-Event-ID` set to the recorded
   resume cursor returns, **as a set**, every pre-upgrade `(sequence, type,
   task_id, payload)` tuple recorded for that run above the cursor. Duplicate
   and additional deliveries are legal and must not fail the assertion.
   **A gap-free range or high-water-mark check is invalid here** and must not be
   written: `ExecutionEvent.Sequence` is a table-wide `autoIncrement` primary key
   (`internal/models/execution_event.go:12`) while `Store.ListSince` applies
   `sequence > ?` and *then* filters by `run_id`
   (`internal/event/store.go:113-150`), so any single run's sequences are
   legitimately sparse — two interleaved runs can hold `{1,3}` and `{2,4}` and
   both be complete. Conversely, reaching the same maximum proves nothing about
   an earlier event surviving. This is exactly DT-EVENT-01's restriction that a
   sequence identifies an event *within the selected store lifetime* and is not a
   gap-free counter; the assertion above is the only form that respects it.
5. The queued row is dequeued and reaches a started run — asserting it merely
   still exists is insufficient, because a stale claim would look identical.
6. The candidate CLI's `job export` of each alias re-lints and produces no
   changes under `job diff` against the candidate server.
7. The serving build is the candidate: container image ID equals the built
   candidate's, cross-checked by `data_assertions_enabled` appearing in
   `GET /v1/system/features`.

**Required unsupported-transition case** (deterministic, no cluster needed):
restart the candidate on the same volume with `CAESIUM_NODE_ADDRESS` set to a
value other than the one in `info.yaml` and assert exit status 1 and the
`address … in info.yaml does not match` fatal from `pkg/db/db.go:56`. This is
F1's documented unsupported transition and the single-node form of the blocking
finding above.

**Required recorded-outcome cases**, run and reported without a pre-judged
expectation, because F1 does not promise them: (a) rollback — restart v0.1.0 on
the candidate-migrated volume and record whether it starts and whether every
pre-upgrade identity reads back; (b) shard-count change — restart the candidate
with a different `CAESIUM_DATABASE_SHARDS` and record the observed behavior. A
passing (a) does not create a supported rollback guarantee; a silently
successful (b) that strands rows is a finding to file, not a pass.

G6 later wires this standalone runner into the declared CI/release matrix; F4
ships with a single containerized command and same-SHA evidence and does not
wait for that.

#### F2 procedure — persistent cluster upgrade, replacement, and recovery

Owns `test/lifecycle/cluster_test.go` and
`helm/caesium/ci/test-values-lifecycle.yaml`, and extends F4's
`test/lifecycle/versions.json` and `scripts/lifecycle-tests.sh`. It **consumes**
B1's completed harness — `scripts/robustness.sh`, `build/Dockerfile.robustness`,
`test/robustness/cluster/` and the recorder — and owns separate scenario files;
it does not edit B1's owned files or the scenario manifest.

`helm/caesium/ci/test-values-lifecycle.yaml` is F2's own values file, derived
from `helm/caesium/ci/test-values-robustness.yaml` minus the fault-tuning knobs:
`replicaCount: 3`, `persistence.enabled: true`, `CAESIUM_DATABASE_SHARDS=1`,
`CAESIUM_DATABASE_VOTERS=3`, `CAESIUM_DATABASE_STANDBYS=0`,
`CAESIUM_DATABASE_CONSOLE_ENABLED=true`, the run-queue variables from F4, and
the worker-hostname anti-affinity. Leave `updateStrategy`, `podManagementPolicy`
and `persistentVolumeClaimRetentionPolicy` **unset** so the test measures the
chart as shipped (`helm/caesium/templates/statefulset.yaml` declares none, so
Kubernetes defaults `RollingUpdate` / `OrderedReady` / `Retain` apply). Do not
inherit cache, lineage, freshness or assertion gates from a mutable sibling
values file.

Install the previous release, not the candidate, into an owned kind cluster and
namespace, reusing B1's lifecycle:

```sh
docker pull caesiumcloud/caesium:v0.1.0
kind load docker-image --name "$LIFECYCLE_ID" caesiumcloud/caesium:v0.1.0
helm install caesium ./helm/caesium --kubeconfig "$ARTIFACTS/kubeconfig" \
  --namespace "$LIFECYCLE_ID" --create-namespace \
  --values helm/caesium/ci/test-values-lifecycle.yaml \
  --set image.tag=v0.1.0 --wait --timeout 300s
```

Before the transition, prove the starting state and capture identity: three
bound PVCs, three distinct pod UIDs/IPs on distinct worker nodes, three dqlite
members with voter roles and an agreed leader through B1's direct `Leader`/
`Cluster` RPCs against each `podIP:9001`, and — for each pod — the address
recorded in `/var/lib/caesium/dqlite/info.yaml` alongside its current pod IP.
Seed the same retained-state fixture classes F4 defines, plus in-flight work: a
run executing at the moment the upgrade starts, and a queued run behind it.

Then upgrade with the **same target and the same values**:

```sh
helm upgrade caesium ./helm/caesium --kubeconfig "$ARTIFACTS/kubeconfig" \
  --namespace "$LIFECYCLE_ID" \
  --values helm/caesium/ci/test-values-lifecycle.yaml \
  --set image.tag="$CANDIDATE_SHA" --wait --timeout 600s
```

Every one of those flags is load-bearing and none may be dropped. `helm upgrade`
does **not** carry the installed overrides forward unless it is given them again
or `--reuse-values`; re-rendering this chart with only an image override falls
back to the chart defaults, where `replicaCount: 1` (`helm/caesium/values.yaml:2`)
and `config.extraEnv` is empty. That would silently scale the release to a single
member and drop the distributed/run-owner configuration, the three-voter dqlite
settings, the worker-hostname anti-affinity and the Kubernetes executor RBAC — so
the run would not exercise the three-member rolling transition this case is
about, while still reporting success. Omitting `--kubeconfig` and `--namespace`
likewise risks addressing a different cluster or namespace than the install. F2
must therefore diff `helm template`/`helm get manifest` before and after the
upgrade and assert that the **only** change is the image reference: replica
count, env block, affinity and RBAC must be byte-identical across the
transition, and a topology change is a harness bug, not a result. If F2 instead
chooses `--reuse-values`, that is a deliberate choice that must be recorded, and
the same rendered-diff assertion still applies.

A `--wait` timeout is a symptom, not a result: F2 must
never assert on the helm exit status alone, and must inspect each pod's phase,
container exit code and log regardless of how helm returned. Assert, per member
and in this order:

1. The pod was recreated; record its new IP and compare it with the `info.yaml`
   address captured before the upgrade.
2. If the address changed, the container must exit 1 with the
   `info.yaml does not match` fatal; F2 records this as the expected
   manifestation of the open prerequisite and the upgrade case reports
   **blocked-by-prerequisite**, never flake and never pass.
3. If the address was reused, the member must rejoin with its retained volume,
   run AutoMigrate without error, and the cluster must return to three voters
   with an agreed leader. This outcome is labelled *address-reuse dependent* and
   is not evidence of a supported upgrade.
4. Retained state: every pre-upgrade job/run/task-run identity readable with
   unchanged terminal status; the recorded per-run event tuples replay as a set
   from the explicit resume cursor, allowing duplicates, under exactly the F4
   rule above (never a gap-free or high-water-mark check); the queued run reaches
   a started run; the in-flight
   run reaches a legal terminal outcome reconciled against the raw effect ledger
   B1's recorder already provides, with duplicate task attempts retained rather
   than automatically failed.
5. Mixed-version window: while members straddle versions, record each member's
   image ID and `GET /internal/capabilities` `protocol_version`, and assert
   dispatch/completion traffic actually crossed a version boundary before any
   interoperability claim is made. Both sides must report protocol 2.

Additional F2 cases, each reported pass / fail / blocked with evidence:

- **Membership replacement (a joining ordinal).** Delete **`caesium-1`** — a
  non-zero ordinal, chosen precisely because the chart gives it a non-empty peer
  list — *and* its PVC; assert the replacement generates a new node ID and joins,
  and record the dead member's leftover entry and role, which no product surface
  can remove. This case says nothing about ordinal 0 and may not be generalized
  to it.
- **Ordinal-0 disk loss (expected blocked).** Delete **`caesium-0`** and its PVC
  separately. Per the prerequisite above, the empty peer list at
  `helm/caesium/templates/configmap.yaml:14-17` makes `app.New` take
  `dqlite.BootstrapID` on the fresh directory and initialize a self-only node
  store rather than joining the surviving quorum. F2 records the replacement's
  `info.yaml` ID, its node store, and the surviving members' view of membership,
  and reports the case **blocked-by-prerequisite** (missing rejoin/bootstrap
  recovery procedure). A run in which ordinal 0 appears healthy must be checked
  against the survivors' membership before it is called a rejoin — a self-only
  bootstrap also answers `/health`.
- **Snapshot catch-up.** Stop one member long enough for the leader to truncate
  past its index, restart it, and assert it catches up. dqlite's snapshot
  thresholds are unconfigured C defaults (`pkg/dqlite/dqlite.go:164-175` passes
  no `WithSnapshotParams`), so F2 must **measure** the threshold it actually
  crossed rather than assume a number.
- **Restore.** Stop one member, copy its volume, corrupt or delete the original,
  restore the copy, restart. **Rejoining and reading matching rows does not
  prove the copy was restored** and may not be asserted as such: `dqApp.Open`
  hands SQL to go-dqlite's driver, which connects through
  `protocol.NewLeaderConnector` (`go-dqlite/v3@v3.0.4 driver/driver.go:307`), so
  an HTTP read served by the restarted member can be answered by a **surviving
  leader**, and any log data missing locally is simply re-replicated from the
  quorum. A restore that silently lost the copied bytes satisfies both
  observations. F2 must instead produce:
  1. **Pre-startup evidence** that the expected snapshot was restored — the
     copy's manifest (per-file sizes and checksums, and `info.yaml` node ID and
     address) compared against the restored directory **before** the container
     starts, not inferred afterwards.
  2. **An assertion that depends on the snapshot's contents and cannot be
     supplied by healthy peers** — read the restored member's local state
     directly rather than through the leader-connected SQL path (for example,
     inspect the restored data directory, or exercise the member with the other
     two stopped so no peer can serve or re-replicate the answer).
  3. **A negative control**: repeat the case with the snapshot omitted or
     damaged and show the assertion **fails**. An assertion that passes without
     the snapshot is measuring peer catch-up, not restore.
  If the harness cannot take a consistent copy, or cannot run the negative
  control, report the case **blocked** — or scope it explicitly to *member
  catch-up* and stop using it to qualify the supported snapshot-recovery policy.
  There is no product restore to exercise.
- **Rollback.** Only as the recorded-outcome case F1 defines; a `helm rollback`
  that starts is not a supported path.

Single-node migration qualification is F4's and does not wait for F2. F2 does
not begin before B3's fault vocabulary is available for the in-flight-work case.

#### F3 procedure constraints from F1

F3 inherits this record's vocabulary and may not widen it. Its seeded fault
sequences may combine the four failure cases above only when each is separately
injected and separately evidenced; a container or process kill is never labelled
power-loss or disk-loss qualification, and a detached volume is never labelled
disk loss. The storage faults F3 is permitted to inject are exactly the
volume-level operations the harness owns on a **stopped** member — detach and
reattach, snapshot and restore, truncate or corrupt a copy, and fill the volume
to exhaustion — plus whatever the provisioned topology supports under Q2; a
fault applied to a running member's volume through the container runtime is a
different, unmodelled case and must be recorded as such. Clock faults remain
outside this plan's scope per A1's clock decision. Repeated failover and node replacement in F3 must reuse
F2's replacement procedure and inherit its blocked-by-prerequisite status until
the stable-node-address prerequisite is resolved. Every soak scenario keeps a
short locally reproducible form, and retains its actual fault schedule as well
as its seed.

### Open decisions and external prerequisites

These questions were offered during brainstorming. All remain unanswered except
Q4, which the F1 decision record resolves above. None
prevents drafting or the first-wave items. Implementations may proceed only
within their resolved contract and available test infrastructure.

| ID | Decision or prerequisite | Proposed direction and evidence required | Blocks |
| --- | --- | --- | --- |
| Q1 | Required PR wall-clock and runner-cost budget | Measure current cold/warm CI, then agree the full matrix budget; 20–30 minutes is a proposal, not a measured commitment. The early G5 gate uses existing hosted runners and reports incremental cost. Keep complete existing suites. | G6 full-matrix promotion and G4 cadence/capacity. |
| Q2 | Controlled performance and multi-host infrastructure | Three separate Caesium processes/containers on one CI host for PR robustness; isolated repeatable performance runners and separate hosts for broader qualification. Record provisioned runner identity, CPU/memory/storage, networking privileges, cleanup owner, and access proof. Provisioning paid infrastructure is not part of this plan PR. | E4 controlled calibration and F3/G4 multi-host operation. |
| Q3 | Supported fault, execution, delivery, and recovery contracts | A1 produces the per-operation contract and selects the first conformance scenarios. Product-owner review resolves stronger or ambiguous guarantees with a recorded decision. A1 can finish with unresolved entries; dependent scenarios cannot claim those guarantees. | B2/B3, C1, D3, and F2 only for unresolved scenario contracts. |
| Q4 | Supported OS/browser and release compatibility matrix | **Resolved by the F1 decision record (W3-ε, 2026-09-14).** CLI platforms are `linux/amd64` and `linux/arm64` only; browsers are Chromium only; the one supported old-to-new path is `v0.1.0 → candidate`, adjacent pairs only; mixed-version operation is limited to internal protocol 2 with an identical shard count; restore-from-snapshot is the supported recovery and binary rollback is not; backup/restore, member removal, build/version reporting and a stable dqlite node address are recorded external product prerequisites. The storage failure model separates process kill, OS/power loss, lost unflushed writes and disk loss. | Unblocked: D1/D2 platform scope, F4 and F2. F2's upgrade case is blocked-by-prerequisite until the stable-node-address defect is fixed, which is product work outside this plan. |
| Q5 | Performance SLOs and regression tolerances | E2/E3 supply workloads and repeated comparisons; E4 measures variance and records minimum samples, acceptable relative degradation, absolute SLOs, and bounded inconclusive handling. No arbitrary global percentage becomes a gate. | E4 sign-off and G6 performance promotion. |
| Q6 | Repository settings and new gate promotion | Read current required checks/rulesets and permissions at execution time; record the selected merge-candidate strategy and settings owner. Apply settings only within the execution request's authorization. | G5 enforcement and G7 candidate/queue policy. |

## Progress (as of 2026-09-25, W6)

**W1–W5 are closed, and W6 implementation is underway.**
A1, A2, B1, B2, C1, C2, D1, D2, E1, E2, E5, F1, F4, G1, G2, G3, G5 and G7 have
merged implementation and recorded acceptance evidence and are checked. B3 and
E3 have merged implementation PRs but stay **unchecked**: B3's live `TestCore`
kind run failed, and E3's two-image `performance.sh` returned a failing comparison.
Do not re-dispatch those two. C3 and F2 are underway in W6; D3, E4, F3, G4 and
G6 remain undispatched. This checkpoint is based on merged master `1b0d20e2`,
which includes all three W5 items and W5/N-1. W5 adds the fenced core-failure
suite, coverage-instrumented CLI/server collection, and a fail-closed
base/candidate performance comparator. None of them is a CI job.
**No repository settings have changed** — `ci-ok` is still absent from master's
required status checks, and no merge queue exists. See Q6 and the G7 row.

| W1 stream | Item / PR | Current evidence and disposition |
| --- | --- | --- |
| α | A1 / [#465](https://github.com/caesium-cloud/caesium/pull/465), merged | Merge `39957b9d67621a88049decd39879a74b79520618`. Decision record independently reviewed; pre-trigger task-placement finding fixed. Structural/link checks pass. This completes the decision record, not runtime fault certification: cluster, proxy and quorum-loss experiments remain unrun and stronger contracts remain unresolved. |
| β | E1 / [#466](https://github.com/caesium-cloud/caesium/pull/466), merged | Merge `646c92197ae2f8c4277583712e5a164ce3a2a005`; verified candidate `9174179615341cc9059d9bad0ecb3931facb20b4`. [CI run 34499466191](https://github.com/caesium-cloud/caesium/actions/runs/34499466191): all 35 executed checks pass; tag-only publish skipped. Full local containerized lint/unit pass. Fresh product live success/bad-image/unavailable cases return 0/1/1 with reconciled counts; successful serial workload has 3/3 runs and 37 samples across 7.0212 seconds, with early/middle coverage in each run. Independent and external reviews have no actionable findings. |
| γ | G1 / [#464](https://github.com/caesium-cloud/caesium/pull/464), merged | Merge `80273d3fe29ecf3a9df84d868a085df944959d3c`; verified candidate `7fcc1fd75b235066b27d66328d4293a34ce424b9`. [CI run 34499470605](https://github.com/caesium-cloud/caesium/actions/runs/34499470605): all 35 executed checks pass; tag-only publish skipped. All 15 Python validators and actionlint pass. Native Chromium fail-once proof using unchanged G1 configuration exits 1 despite a passing retry. Fresh hosted browser lanes pass (28 default + 8 auth, no skips/flaky outcomes); both uploaded artifacts were recursively checked for API-key tokens. Tested merge `ffe74c5a` has parents `dd35ba42` and `7fcc1fd7`. Independent and external reviews have no actionable findings. |
| Index prerequisite | [#467](https://github.com/caesium-cloud/caesium/pull/467), merged | Merge `2d28607feb50412b273abefc0aae94da64bf0099`. The one-line README convention repair is incorporated in both candidates. The formerly failing index guardrail now passes locally and on both CI architectures. No guardrail or test floor changed; this is not N-1 completion. |

**W1/N-1 — [#469](https://github.com/caesium-cloud/caesium/pull/469), merged.**
Merge `79d8add59386881c763fc3185c7ad91e606b1470`. It synchronized `docs/ci.md`
with shipped load-harness exit/report behavior, browser diagnostics and Python
validator discovery, and updated the README and roadmap status links. W1 is
closed; no further status-only PR is owed for it.

| W2 stream | Item / PR | Current evidence and disposition |
| --- | --- | --- |
| α | B1 / [#472](https://github.com/caesium-cloud/caesium/pull/472), merged | Merge `f3f5ce3ab68d57709127a0b2807e670e7d39726a`; verified candidate `b716c6dbe0b65757f8e4c5984e2e7f98993b9ac4`. Live kind proof `B1_KIND_OK` on 2026-09-12: `TestOwnerCrash` PASS in 123.22s, `owner_is_leader` 63.21s (recovery 33.086s) and `owner_is_not_leader` 57.38s (recovery 35.090s), generation 1→2 in both, owner `10.244.1.3` → `10.244.3.3`. Three persistent replicas on distinct nodes/IPs/UIDs with bound PVCs; dqlite reported 3 voters and leader `10.244.1.3:9001`; connectivity probe observed before the fault; kill path was cordon → `systemctl stop kubelet` + `ctr tasks kill SIGKILL` with observed death; old kubelet restarted and uncordoned; two isolated cluster IDs coexisted before the extra was deleted; 2 duplicate block attempts retained and sink nonces correlated to public task-run UUIDs. `helm lint`/`helm template` against `test-values-robustness.yaml`, `bash -n scripts/robustness.sh` and `git diff --check` pass. **Limits:** the live proof is orchestrator-owned (image build plus a 4-node cluster) and the lane is **not** wired into CI — G3 owns that. Remaining product risk recorded by the PR: `POST /v1/database/query` issues `PRAGMA query_only`, which native dqlite may reject; a 500 there is a product-console gap, not permission to skip lease observation. |
| β | E5 / [#471](https://github.com/caesium-cloud/caesium/pull/471), merged | Merge `4bc19ac98389ccd3fd61382e9f9ec40ad37949a6`; verified candidate `86917428a4eb5c29d4aec9a2608634a94e88b2c8`. Live `just integration-test` from the item worktree PASS in 610.355s: `TestStatementBudgetFixedWorkload` PASS (2.42s) with `lease_renewal` and `callback` skipped with logged reasons (both deltas 0), the recorded comparator profiles PASS including the leftover-plus-lost-batching fixture, and `TestStatementBudgetParseCounterRejectsPrefixOnlyName` PASS. **Limits:** the scenario gates the measured `caesium_db_statements_total` / `caesium_db_writes_total` categories only — not latency, not uninstrumented queries. `lease_renewal` and leftover `callback` traffic are reported as skipped with reasons instead of false exact bounds; `command`/`checkpoint` are asserted zero. `test/contracts/scenarios.json` was deliberately not edited, so G3 still has to register `TestStatementBudgetFixedWorkload`. |
| γ | A2 / [#470](https://github.com/caesium-cloud/caesium/pull/470), merged | Merge `0c42e549f5983fb4c8bf78cbfdf9fae698b21f41`; verified candidate `56db7bf755035b57e905e389f060970b61c920e9`. `python3 -m unittest scripts/test_test_evidence.py -v` 29 tests OK; `python3 -m unittest discover -s scripts -p 'test_*.py' -v` 44 tests OK; `git diff --check` clean. **Limits:** every committed manifest row is `status: absent` with empty `gates`, so the catalog names planned B1/B2/B3/D3/E5/G3 scenarios and proves none of them; G3 registers selectors and the early gate after the tests exist. Verification was Python-only (no Docker), and this PR invented no passing CI evidence. |
| W2/N-1 | [#473](https://github.com/caesium-cloud/caesium/pull/473), merged | Merge `b6b1c054da201c4dfd7b8395511f7d5fec386cae`. Docs-only sync of `docs/ci.md`, this Progress dashboard, and the README/roadmap status lines to the three merged W2 items. W2 is closed; no further status-only PR is owed for it. |

| W3 stream | Item / PR | Current evidence and disposition |
| --- | --- | --- |
| α | G3 / [#477](https://github.com/caesium-cloud/caesium/pull/477), merged | Merge `2ef933e13d96f3e26b8f188fa19481d90257692f`. Delivered the `early-evidence` CI job on the existing kind/Helm/`run-integration` infrastructure, the `robustness-runner` bake target and its `robustness-amd64` artifact (compiling `./test/robustness` explicitly with `-tags=integration`, since the precompiled `./test` binary cannot contain subpackage tests), the `robustness-runner`/`robustness-test`/`integration-test-sql-budget`/`check-evidence`/`early-evidence` just recipes, and the stdlib-only artifact consumer `scripts/collect-evidence.py` with its unit module. Registered `b1-owner-crash-leader`, `b1-owner-crash-nonleader` and `e5-sql-work-budget` as `proven` with `gates: ["early"]`; every other manifest row stays `absent` with no gate. Corrected three A2 manifest defects: E5's selector matched no test (now suite-qualified `TestIntegrationTestSuite/TestStatementBudgetFixedWorkload`), its topology claimed `database_shards: 1`/`voters: 1` where `integration-up` runs four shards on one standalone process with `mode.execution: local`, and the B1 rows' `feature_flags` used pseudo-keys no observed environment could match (now the real `CAESIUM_*` names). Live lane on candidate `f3f5ce3a`: `TestStatementBudgetFixedWorkload` PASS (2.44s) and `TestOwnerCrash` PASS (125.94s; `owner_is_leader` 63.21s, `owner_is_not_leader` 60.18s), evidence validated with three scenarios `pass`, cluster and containers cleaned up afterwards. Two live fail-closed proofs on the same real artifacts (deleted fragment, removed scenario) exit 1. 88 Python tests OK, actionlint clean, `just helm-lint`/`helm-template`, `just lint` 0 issues, `just unit-test` pass. **Limits:** the Docker lane runs the `-test` image variant, so `e5-sql-work-budget` sets `require_candidate_digest: false` and records `observed_server_image_id` rather than claiming the release digest. At this merge the lane was deliberately outside `ci-ok` and `scripts/ci-ok.py` was untouched — G5 owns promotion. The full `just integration-test` suite was not run. |
| β | C1 / [#475](https://github.com/caesium-cloud/caesium/pull/475), merged | Merge `5ecd95d8374ebcf281c835a9f9cde21dd848b34c`. Delivered the untagged pure-Go `test/model/` reference model — readiness re-derived declaratively as a fixpoint over the outcome set, deliberately unlike the product's incremental predecessor counters — plus `internal/run/model_properties_test.go` and `internal/run/recovery_properties_test.go`, which drive the real `RunState` and the real `RecoverRunState`/`RecoverRunStateWithFanOut`/`Snapshot`/`Restore`/`ValidateCheckpointBlob` against it over generated bounded DAGs and every checkpoint index of a generated execution. Added `pgregory.net/rapid v1.3.0` and `github.com/anishathalye/porcupine v1.3.0` with toolchain-generated sums (`go mod tidy` also promoted the already-direct `prometheus/common` out of the indirect block). `independence_test.go` enforces the import allowlist and the no-build-tag rule mechanically, so the hermetic claim is a test rather than a convention. Porcupine checks only the run-status register; delivery, liveness and lease safety have separate models, each with a negative control proving the oracle rejects a planted defect. **Deliberate deviation, stated rather than silent:** minimized counterexamples are retained as deterministic source in `test/model/regression_test.go` rather than as Rapid `.fail` artifacts, with the `.fail` workflow (keep while a defect is open, transcribe on fix) documented in `doc.go`. **Limits:** the four counterexamples found were all in the new model — the product's `RunState` and recovery paths agreed with it under 4000+ generated cases per property, so **no product defect was found** — and a green run proves a decision function agrees with a specification on hermetic inputs, never that the wiring around it is correct. `just lint` 0 issues, `just unit-test` exit 0, integration-tagged compile green; `just integration-test` deliberately not run (test-only diff). |
| γ | D1 / [#476](https://github.com/caesium-cloud/caesium/pull/476), merged | Merge `b26fb6137e3a455a4ec9a9ec4bd188ac66c87a81`. Delivered `test/developer_journey_test.go` (ten `IntegrationTestSuite` scenarios extending, not rewriting, `test/local_dev_test.go`) and `test/developer_testdata/pipeline.job.yaml`: unparseable YAML rejected by both collection paths (`internal/jobdef.CollectDefinitions` for `test`/`dev` versus `cmd/job.collectDefinitions` for `lint`/`preview`/`apply`), a schema-invalid `dev --once`, a workspace directory and job filename both containing a literal space, an unreachable `kubernetes` engine, `--run-timeout` proving the container is stopped *and* removed, watch-mode re-run on edit plus graceful SIGINT with no surviving container, and `job apply` followed by `job export` asserting the round-tripped DAG topology and labels. Streams captured separately throughout. Full `just integration-test` green with 0 failures (`ok ... 602.102s`); all ten scenarios PASS. **Product defects found and filed rather than fixed** (the owning packages are outside this item's file scope): [#479](https://github.com/caesium-cloud/caesium/issues/479) — `dev --once` panics in `internal/atom/kubernetes.NewEngine` (exit 2) on an unreachable engine because `internal/job.buildLocalRunners` calls the factory with no `recover()`; and [#480](https://github.com/caesium-cloud/caesium/issues/480) — `dev --once` ignores SIGINT and orphans its container. The affected assertions require only a bounded nonzero exit, so they stay valid once those are fixed. |
| δ | D2 / [#482](https://github.com/caesium-cloud/caesium/pull/482), merged | Merge `894f645b85d4b17e5bb0abac6bd9d4903501fb12`. Delivered `ui/e2e/accessibility.spec.ts` (scoped axe over WCAG2/2.1 A+AA critical/serious findings plus keyboard/focus, with the react-flow DAG canvas and xterm terminal excluded as DOM-less by construction), `ui/e2e/visual.spec.ts` with committed `-linux.png` baselines, `ui/e2e/scale.spec.ts` (a real 18-node DAG, 24 individually reachable pipelines, a synthetic 240-row partition set, a real 2000-line log stream), `ui/e2e/network-recovery.spec.ts`, the shared `failOnUnexpectedPageErrors` guard plus `uniqueSuffix`/`buildFanDefinition` in `ui/e2e/helpers/fixtures.ts`, `toHaveScreenshot` defaults in `ui/playwright.config.ts`, and a pinned `@axe-core/playwright`. Adversarial-review findings were fixed in-PR (commits `ab450ddb`, `503517fa`, `527c737c`, `ac0582be`): the axe baseline is tracked per violating **node** rather than per rule id, matched by leaf class set and, for `color-contrast`, by `fgColor` within an RGB tolerance, because axe's selector minimization and these opacity-composited tokens both vary run to run against an unmodified UI; the Trigger Job dialog's single-Tab focus check became a real wrap-around boundary proof in both directions; the partition-table baseline was re-taken from CI's actual render; and the console-error guard's network-level `net::ERR_*` allowance ended up file-scoped to `network-recovery.spec.ts` alone, every other spec keeping the strict default. **Limits:** `KNOWN_VIOLATIONS`/`KNOWN_CONTRAST_TOKENS` is a tracked baseline of real pre-existing product defects, filed as [#483](https://github.com/caesium-cloud/caesium/issues/483) — not a pass; shrinking it is product-code work outside this stream. The visual tests self-skip off Linux, so the per-pixel comparison happens only in CI's `ui-e2e`. The large partition set and the credential/permission-denial cases are explicitly SYNTHETIC (this e2e server runs `CAESIUM_FANOUT_MAX_PARTITIONS=8` and no `CAESIUM_AUTH_MODE`); real scope denial stays covered live in `ui/e2e/auth/`. Local `just ui-lint`, `ui-test`, `ui-e2e` (41 passed, 3 darwin screenshot skips), `ui-e2e-auth` (8 passed), `just lint` and `just unit-test` all green. |
| ε | F1 / [#474](https://github.com/caesium-cloud/caesium/pull/474) + [#478](https://github.com/caesium-cloud/caesium/pull/478), merged | Merges `937a9feac5e51b7358331356bb95d8247617bff7` and `da08e7ef81dc0f5a0818e6283eba78bec9f054b1`. Delivered the F1 lifecycle decision record under Strategic Decisions and closed **Q4** there. Audited against real code, chart, workflow and registry: there are no hand-written migrations and no schema-version table (`pkg/db/Migrate()` AutoMigrates `internal/models.All` from struct tags, with one explicit DDL repair), every node migrates unconditionally on boot, and the shard count is frozen for the life of a data directory. Exactly one release exists (`v0.1.0`); the record pins its multi-arch and per-arch image digests and release asset checksums, and flags Docker Hub's `latest` as a 2021 amd64-only image the publish chain never updates. Q4: CLI platforms `linux/amd64`+`linux/arm64` only, Chromium only, adjacent-pair upgrades only, mixed-version operation limited to internal protocol 2 with an identical shard count, restore-from-snapshot supported and binary rollback not. **Blocking finding, reproduced locally against `caesiumcloud/caesium:v0.1.0` on a retained volume:** go-dqlite refuses to start when the supplied address differs from `info.yaml` (`pkg/db/db.go`), and the chart sets `CAESIUM_NODE_ADDRESS=$(POD_IP):9001`, so a persistent StatefulSet upgrade depends on the CNI reusing the pod IP — and `helm upgrade` is never executed in CI at all. Five external product prerequisites are recorded (backup/restore, member removal, build/version reporting, stable dqlite node address, ordinal-0 rejoin/re-bootstrap). #478 accepted and fixed all five P2 findings from the adversarial review of #474: `data_violations` belongs to `task_runs` and not `job_runs`; the event fixture records identities as a set with an explicit resume cursor instead of a forbidden gap-free high-water mark; the Helm upgrade command must keep its kubeconfig/namespace/values and diff `helm get manifest` before and after; ordinal-0 loss is a separate blocked case, with `caesium-1` named for replacement; and restore must be distinguishable from peer catch-up, which downgrades the rollback-versus-restore row from "supported" to "intended, not yet qualified". **Limits:** this is lifecycle policy and procedure, not upgrade certification — no upgrade, rollback, replacement or restore was executed. F4 and F2 are dispatchable without further design, but F2's upgrade case is blocked-by-prerequisite on the stable node address and its disk-loss case on ordinal-0 bootstrap. Docs-only; `just lint` 0 issues and `just unit-test` green on both PRs. |
| ζ | G5 / [#481](https://github.com/caesium-cloud/caesium/pull/481), merged | Merge `5d4bf73e0d194301090e147a17daf5c883f7e0bc`, merged by the CODEOWNER. `ci-ok` now requires `early-evidence` with `SELECTORS["early-evidence"] = ("go", "helm", "ci")` — exactly the lane's own `if:` condition, so a lane that vanishes cannot read as an allowed skip — and `helm-lint`, the lane's unconditional producer, is promoted with it so a chart failure reports as itself rather than as a skipped consumer. **A green job result is not accepted as evidence:** `ci-ok` downloads the report the lane uploaded and `scripts/ci-ok.py` requires it to exist and parse as an object, to bind to this run's `github.sha`, to have at least one registered `early` scenario (an ungated manifest cannot vacuously pass), to report every `early`-gated row `pass` with its required fault-activation kind and observations, and to pass `check-test-evidence.py --require early --strict`. Measured on hosted `ubuntu-24.04` runners across five green runs: 6m19s, 5m57s, 6m18s, 7m15s and 7m32s (mean ~6m40s), with no lane flake and no lane retry. The aggregate's verdict now lands ~2m07s later; total workflow wall clock grew 0s on one attempt (13m12s, `helm-integration-test` still the critical path) and +25s on the other, where `ci-ok` became the critical path, and the promotion costs no new runner minutes because the lane already ran on every `go`/`helm`/`ci` PR. It also fail-closed for real on [run 34868499191](https://github.com/caesium-cloud/caesium/actions/runs/34868499191) attempt 1, where a transient `images`/`bake-images` failure skipped the lane and the gate refused for both the missing dependency and the absent evidence report. **Q6 finding — `ci-ok` is NOT enforced at merge.** Master's protection lists eight required contexts (`lint`, `unit-test`, `unit-test-arm64`, `ui-test`, `ui-e2e`, `ui-e2e-auth`, `build-and-integration-test`, `build-and-integration-test-agent-auth`), `strict: false`, `enforce_admins: false`, one CODEOWNER review, and there are no rulesets; `ci-ok` is absent, so this promotion blocks `v*` tag publication (through `publish.needs`) but not PR merge. Exactly one repository-settings change closes that — adding `ci-ok` to `required_status_checks` — which is outside execution authorization and was **not** made; it is surfaced to the CODEOWNER. Existing optional lanes (`helm-integration-test`, `podman-integration-test`, `integration-extra`, `integration-arm64`) keep their unpromoted status, asserted by a test. |
| W3/N-1 | [#518](https://github.com/caesium-cloud/caesium/pull/518), merged | Merge `e864d780bb5fcb18bfe7e8f516b838a43d7cf6b9`. Docs-only sync of `docs/ci.md`, this Progress dashboard, and the README/roadmap status lines to the six merged W3 items, authored from merged master `894f645b`. It also carries the one-line G5 note refresh (`ed62d4a0`) that was raised on the W3-ζ branch but never merged. W3 is closed; no further status-only PR is owed for it. |

| W4 stream | Item / PR | Current evidence and disposition |
| --- | --- | --- |
| α | B2 / [#555](https://github.com/caesium-cloud/caesium/pull/555), merged | Merge `334f99b391e7d638ee42bc5580b26214d93daea2`; verified candidate `acd2cd9c53e30c0387567aebd1d21bf19de2cdf0`. Live-proven `TestTargetedFaults` on owned kind clusters: `external_pause_resume`, `asymmetric_partition` (external iptables on discovered Raft/dispatch addresses; A1 forbade proxies), `response_loss_possibly_committed` (client-side interposer after an observed commit; timeouts stay possibly committed), `event_history_correlation` (SSE vs persisted rows as a set inside DT-EVENT-01's store scope), and `bus_publish_pause` (test-only durable-event-before-publication hook on both `PublishAndMarkBusDispatched` and `DispatchOnce`). **EX-HOOKS** re-verified at the execution base: last `bus_dispatch.go` commit still `573edfee` (PR #423); open sibling #449 does not list the file. Compile-time absence of `internal/testfault` in the release image is scanned on every `scripts/robustness.sh` run. Default `CAESIUM_ROBUSTNESS_RUN` remains `^TestOwnerCrash$`, so the `early-evidence` merge gate is unchanged; `TestOwnerCrash` still PASS on the ordinary release image. `justfile`, bake files, the workflow and `test/contracts/scenarios.json` were not edited — B3 registers these scenarios. **Limits:** targeted faults are not a CI lane; missing recorder data, vanished hook logs, or a broken SSE subscription are inconclusive, never a pass. |
| β | E2 / [#552](https://github.com/caesium-cloud/caesium/pull/552), merged | Merge `12feeec0d48c6cc50aa9911ce6f58b309e178e48`; verified candidate `d18d0313bbe0f18d68d2a8aff1abe1b340e098c5`. Extended E1's containerized Go driver in place (no k6, no `justfile`/`go.mod` edit). `mode=open` places arrivals on an absolute clock grid; the ledger separates dropped / admitted (DT-ADMIT-01 UUID-202) / queued-or-skipped (bare 202) / rejected / `transport_uncertain` (DT-QUORUM-01, never a rejection). Report schema 2 keeps every schema-1 field. Live `go test -tags=integration ./test/performance` against the `integration-up` server: 10/10 catalog workloads plus two extra tests, `ok` in 350.766s; full `just integration-test` green (276 scenario PASS). Three adversarial-review rounds on #552, all findings fixed with regressions. **Limits:** `open-tiny-sustained` and `open-api-read-mix` gate on the backlog verdict; four other open workloads report `backlog_growing` on the shared host and pass only because they omit `require-sustained` (each carries an enforced `sustained_rationale`). Not a CI job and not a calibrated SLO — E3/E4 own comparison and budgets. No product-code fix was required. |
| γ | F4 / [#554](https://github.com/caesium-cloud/caesium/pull/554), merged | Merge `456a4102b6ca5a709785cc9d62d20e910694627c`; verified candidate `24abe043fcb06c03c5020c1d0f44b61f99753077`. Shipped `scripts/lifecycle-tests.sh`, integration-tagged `test/lifecycle/standalone_test.go` (compiled explicitly; the precompiled `./test` runner does not contain subpackages) and `test/lifecycle/versions.json` (pinned `v0.1.0` index digest `sha256:2e6996f9…73917`, per-arch digests, CLI checksums, protocol 2, shard count 1, explicit `{table, column}` delta). One command, candidate image built from this checkout unless supplied (supplied images are `unverified` and block unless `CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1`). Measured qualification **pass**: 17 pass + 3 recorded-outcome cases. **#536 re-grounding:** F1's required "changed `CAESIUM_NODE_ADDRESS` exits 1" case is stale on the candidate after PR #536 (`35bced63`); F4 asserts #536's recover-and-rewrite contract on the candidate and keeps the pinned v0.1.0 image as the deterministic failing transition. Recorded-outcome: rollback (v0.1.0 starts on the migrated volume), shard-count change (no stranding observed at this size — does not unfreeze the shard count), retained in-flight run still `running` and holding a concurrency slot (filed [#553](https://github.com/caesium-cloud/caesium/issues/553)). **Limits:** one node, one shard, local execution mode, purely additive pair so `MigrateTaskRunUniquePartitionIndex` is not exercised. Not wired into CI or `ci-ok` — G6 owns that; `test/contracts/scenarios.json` is untouched. |
| δ | C2 / [#551](https://github.com/caesium-cloud/caesium/pull/551), merged | Merge `0aa96d5556a52d64844d542ccd2214983acc8a21`; verified candidate `b2c990971dbabb63e958e7fff4bd6a419e7ab6a4`. Eight fuzz targets across four packages, each asserting a property beyond "didn't panic", plus `scripts/fuzz-tests.sh` (re-execs inside `caesium-builder:latest-full`, POSIX `sh`, discovers via `go test -list '^Fuzz'`, rejects seed-only/zero-exec runs, exports corpus artifacts) and `internal/worker/renewal_synctest_test.go` (real renewal goroutines through `testing/synctest`; no production clock seam). Measured `CAESIUM_FUZZ_SECONDS=20s`: 8/8 targets explored (61k–358k execs), 9/9 `-race -count=3` concurrency configs at `-cpu=1,2,4` passed. Three review-fix rounds on #551, all verified real. Product defect found by reasoning, filed not fixed: [#549](https://github.com/caesium-cloud/caesium/issues/549) (`mergeDescriptorSecretRefs` empty-existing fast path). **Limits:** real dqlite SQL, wall-clock jitter and HTTP dispatch stay out of synctest's claim. `justfile` untouched (G6-owned). `just integration-test` not run (test-only plus a script). |
| ε | G7 / [#550](https://github.com/caesium-cloud/caesium/pull/550), merged | Merge `d3199bc22c519626452179591400fb9cf8e4537c`; verified candidate `a23d97a3224b27b883e37b3596087981558637bf`. Settings evidence read twice (2026-09-16 and 2026-09-17) and **not modified**: required contexts remain the eight G5 listed; `strict: false`; `enforce_admins: false`; `require_code_owner_reviews: true`; `rulesets` → `[]`; `allow_update_branch: false`; `ci-ok` still absent. Wired `merge_group` (`types: [checks_requested]`), disjoint concurrency groups, fail-closed `changes` filter outputs, and `scripts/ci-ok.py` candidate-identity / base-freshness / pull-request parent checks. The two legacy `build-and-integration-test*` wrappers still omit `--candidate-sha` and stay exempt. **Live-queue limitation:** no ruleset exists, so `merge_group` has never fired; every queue-specific path is proven only statically (`actionlint` + `scripts/test_ci.py`). The `pull_request` path is live: [run 35221628892](https://github.com/caesium-cloud/caesium/actions/runs/35221628892) `ci-ok` logged `event='pull_request'` identity plus matching base freshness. Recommendation to the CODEOWNER (not applied): add `ci-ok` to required contexts, then either `strict: true` or a merge-queue ruleset. `allow_update_branch: true` is an optional convenience for non-strict protection, not a `strict: true` prerequisite. |
| W4/N-1 | [#556](https://github.com/caesium-cloud/caesium/pull/556), merged | Merge `459949298e745129bf86c09048f930d82e9b9d72`. Docs-only sync of `docs/ci.md`, this Progress dashboard, and the README/roadmap status lines to the five merged W4 items, authored from merged master `334f99b3`. W4 is closed. |

The W5 rows preserve their merge-time limits; W6 follow-up evidence is below.

| W5 stream | Item / PR | Merge-time evidence and disposition |
| --- | --- | --- |
| α | B3 / [#557](https://github.com/caesium-cloud/caesium/pull/557), merged | Merge `84c7f62b313c23f57b58d882e1ca3cf87e5ac4bb`; verified candidate `9dbde3eb4ca159d9bd95ef1cb48892abfcbf1087`. `TestCore` composes B2's pause, partition, response-interposer and bus-publish hook. Review on #557 (11 threads, all resolved in `605a698f` plus the unused-helper fix `9dbde3eb`) made the oracles fail closed: a 2–1 split needs nonzero iptables drop counters, fan-in waits until left is `succeeded` and holds 25s, auth snapshots use the sink barrier, benching is a cap on `network_error` during cooldown, and a duplicate completion must hit the lease owner with a terminal 409. Default `CAESIUM_ROBUSTNESS_RUN` is still `^TestOwnerCrash$`. Catalog rows for B2/B3 stay `status: absent` with empty `gates`. **Limits:** no live `CAESIUM_ROBUSTNESS_RUN='^TestCore$'` kind run was recorded. `early-evidence` on this head passed `TestOwnerCrash` only. The checkbox stays open until that live proof exists. |
| β | G2 / [#558](https://github.com/caesium-cloud/caesium/pull/558), merged | Merge `7fc1580bc8d75225e0aecae4a731591b5d435c85`; verified candidate `7dcef7f07d85cf460a26239e11bb30889ac9221a`. Separate `build/Dockerfile.coverage` image (not release, not the performance image). `scripts/integration-coverage.sh` refuses a dirty tree, stamps `org.opencontainers.image.revision`, treats `SKIP_BUILD` as unverified, and deletes stale covdata before collect. `scripts/check-coverage.py` ignores `init()` coverage, requires named apply/export/server functions from `server.out`, treats a missing provenance file as incomplete, and writes a baseline only on `verdict=pass`. `python3 -m unittest scripts.test_coverage`: 46 OK. CI on this head: `ci-ok` success, including `ui-e2e` (the earlier jobs-list / callback flakes did not recur). **Limits:** the isolated apply→export collect predates the review-fix commit, so it is not evidence for `7dcef7f0`. Browser coverage was not collected. Not a CI job. The checkbox stays open until a collect on this commit is recorded. |
| γ | E3 / [#559](https://github.com/caesium-cloud/caesium/pull/559), merged | Merge `498800d0981091acb34c65931b787e3a68bf9ed3`; verified candidate `3025032242cc26ab0d05f25152c5419ef9e04f7a`. `scripts/compare-performance.py` returns exit 0 only when every metric is `faster` or `no_significant_difference`. Direction follows Mann-Whitney U and the Hodges–Lehmann shift, not the mean. `scripts/performance.sh` logs to stderr, fails a `just build-release` that does not return, records per-SHA builder image ID and `go version`, interleaves warm runs, and treats bundle results as a deterministic budget from `node ui/scripts/check-bundle-size.mjs --json`. Browser series are keyed by metric, route and kind. `python3 -m unittest scripts.test_compare_performance`: 37 OK. CI on this head: `ci-ok` success. **Limits:** no live two-image `performance.sh` run. Not a CI job and not an E4 budget. The checkbox stays open until that live comparison is recorded. |
| W5/N-1 | [#561](https://github.com/caesium-cloud/caesium/pull/561), merged | Merge `1b0d20e288207f4142b574b2a350752130ff8b60`. Docs-only sync of `docs/ci.md`, this Progress dashboard, and the README/roadmap status lines to the three merged W5 items, authored from merged master `498800d0`. W5 is closed; the later acceptance evidence is recorded below. |

| W6 stream | Item / PR | Current evidence and disposition |
| --- | --- | --- |
| α | C3 / [#563](https://github.com/caesium-cloud/caesium/pull/563), open | Head `29dc691e` from merged master `1b0d20e2`. The isolated validator passed 12 candidate probes, six temporary named mutation failures and 12 parser tests. Independent reviews caught a cross-epoch acknowledged-identity blind spot and file/disk-exhaustion false-greens; the checker, sentinel, parser and negative controls were repaired. Exact-head `ci-ok` and required checks passed; CODEOWNER review is required before merge. |
| β | F2 / [#565](https://github.com/caesium-cloud/caesium/pull/565), draft | Fifth exact-head `a3d70415` lifecycle run on a fresh owned three-member persistent kind cluster verified the pinned v0.1.0 and candidate archive/config/layer identities and all four node imports. Seed passed three distinct PVC-backed voters and direct dqlite membership. Helm image-only upgrade exited 0, all candidate pods became Ready, and AfterUpgrade observed three retained voters. The mixed-version case was blocked, and a later durable task lookup returned zero rows because the harness queried `task_runs.id` with the public catalog `task_id`; this also discarded mixed-window attempts. The run exited 1, leaving retained-history and recovery cases unproved (`/tmp/caesium-w6-f2-final.3Fn3b4/cluster-qualification.json`; `.codex/runs/distributed-testing/w6/f2-fifth.log`). The owned cluster was deleted. An unpushed mapping repair now snapshots exact durable task-run IDs before upgrade and rejects row replacement afterward. Independent review found that raw nonce counts cannot prove which advanced attempt produced an effect; an F2-only pod/runtime/event join is being evaluated, with advanced attempts blocked if provenance is unavailable. Focused race tests passed. Rollback and ordinal-0 recovery retain external prerequisites; F2 acceptance stays open. |
| B3 acceptance repair | [#564](https://github.com/caesium-cloud/caesium/pull/564), draft | Third exact-head `9208d63e` `^TestCore$` on an owned three-member persistent kind cluster passed 9/12 subtests. Two cancellation probes stopped inconclusive because they used public task IDs where durable `task_runs.id` values were needed. `stale_generation_complete` failed before testing the fence: it paused the initial dqlite leader as run owner, and all 26 survivor SQL probes timed out during the takeover window, even after metadata named a survivor leader. This is a separate observed SQL availability regression; its code-level cause is not established. Other subtests, including the new terminal and post-refusal checks, passed (`/tmp/caesium-w6-b3-final.ZNGrxW/robustness/robustness.test.log`; `.codex/runs/distributed-testing/w6/b3-final.log`). The owned cluster was deleted. The earlier head passed `ci-ok`. Current head `c69c1c14` maps unique public catalog IDs to durable task-run IDs and selects an asserted non-dqlite-leader owner for the stale fence; focused tests, vet and independent review passed. Current-head `ci-ok` passed; Helm integration is still finishing. CODEOWNER review and the exact-head live `TestCore` rerun, now underway on a fresh owned cluster, remain pending; B3 acceptance stays open. |
| E3 acceptance repair | [#566](https://github.com/caesium-cloud/caesium/pull/566), draft | Current head `87546de6` shares exact benchmark blobs between the W4 base and candidate, records image/source/harness provenance, alternates Go samples, and rejects incomplete per-repeat and aggregate/compare-only benchmark evidence. Sixty-one host tests passed; exact-head `ci-ok` passed. One Helm integration shard first failed an unrelated timing-sensitive secret-log assertion (expected live, saw persisted); the failed-job rerun passed, making the exact-head full Actions workflow green on attempt 2. A full ten-repeat run at parent `d92378b6` had matched uninstrumented provenance, passing correctness, and an independently audited 20/20 complete benchmark samples, but exited 3 with `overall=inconclusive`: live action-to-render exceeded the browser noise limit and one Go bytes/op series measured slower (`/tmp/caesium-w6-e3-full-second.eRiHf0/report.json`; `.codex/runs/distributed-testing/w6/e3-full-second.log`). The exact-head full ten-repeat run at `87546de6` completed with matched uninstrumented provenance, 20/20 alternating zero-exit Go samples, passing workload/browser correctness and identical bundle budgets. Its 42 metrics yielded 40 no-significant-difference, one noisy synthetic jobs-list browser series (candidate CV 0.509 > 0.3) and a 5.05% slower warm workload (p=0.0376); exit 3, `overall=inconclusive` (`/tmp/caesium-w6-e3-final.W68DaT/report.json`; `.codex/runs/distributed-testing/w6/e3-final.log`). E3 acceptance stays open. |
| Existing W5 acceptance | B3, G2, E3 | G2's post-review collect on `7dcef7f0` passed: fresh labelled image, complete CLI/server/integration profiles, 7.6% integration coverage, and the apply→export write/read path covered (`/tmp/caesium-w6-g2.ax3fCH/report.json`; local log `.codex/runs/distributed-testing/w6/g2-coverage.log`). Browser coverage was not supplied and remains incomplete. B3's `^TestCore$` ran on the exact reviewed `9dbde3eb` image in an isolated persistent three-member kind cluster and **failed**: 6/11 subtests passed; `terminal_no_regress`, `frozen_retry_recipe`, `invalid_mtls_peer`, `cancel_completion_race`, and `stale_generation_complete` failed. The first four need contract/harness triage; the lease takeover timeout lacks enough query diagnostics to attribute a product defect. The runner exited 1 and collected evidence at `/tmp/caesium-w6-b3.lW1BGL`, with host log `.codex/runs/distributed-testing/w6/b3-testcore.log`; B3 acceptance remains open and the separate remediation branch owns the follow-up. E3 ran the exact `45994929` base and `30250322` candidate through five interleaved cold/warm repeats, Chromium, release-image and bundle checks, and the targeted Go benchmark command. The comparison exited 2 with `overall=fail`: the W4 base has none of E3's new benchmark functions (0 base versus 5 candidate samples), and two browser timing series were inconclusive. Provenance matched and was uninstrumented; missing baseline data remains a failure. Report `/tmp/caesium-w6-e3.w3LNdU/report.json`, host log `.codex/runs/distributed-testing/w6/e3-performance.log`. E3 acceptance remains open. |
| W6/N-1 | pending | Shared runbook and final Progress sync follows merged W6 implementation PRs. |

The overall 27-item plan remains active. **The minimum credible gate milestone
(G3 + G5) is delivered in CI but is not merge-enforced**, and G7 did not close
that Q6 gap. `early-evidence` still runs `TestOwnerCrash` only. W5's core
faults, coverage collector and performance comparator are **commands, not CI
jobs**. Console fault journeys, calibrated budgets and cluster upgrades have
not shipped; the G2 collect above is local evidence, not a CI lane.

### Resume and tracking rules

The committed plan is the shared progress record; PR bodies hold detailed
candidate evidence. The machine-local `.codex/runs/distributed-testing/w<n>/state.md`
is a recovery aid, not a replacement for this dashboard. The orchestrator keeps
Progress current when PRs are published, revised, verified or merged, including
at a review-only endpoint. Interim Progress corrections do not wait for N-1;
N-1 consolidates the shared runbook after implementation merges.

On every `exec-plan-wave` invocation, fetch the current base and reconcile these
rows against live PR state, head/merge SHAs, reviews and current-head checks.
Resume an unfinished W6 stream before allocating a later wave. Checkboxes mean
merged acceptance evidence; rows distinguish implementation and verification
from merge. W5/N-1 was verified merged as `1b0d20e2`; W6 selected C3 and F2,
preserving unresolved Q1–Q3, Q5 and Q6 and shared-file ownership. Dependency
readiness alone does not authorize dispatch before the current wave's checkpoint.

Do **not** re-dispatch B3, G2 or E3. Their implementation PRs are merged. G2's
post-review collect on `7dcef7f0` is recorded above. B3 and E3 checkboxes
stay open until a live `TestCore` kind run and a two-image `performance.sh`
comparison are recorded.

Dependency-ready after W5: **C3** (B3, C2), **D3** (B3, D2) and **F2** (F4, B3).
C3 and F2 were dispatched for W6; D3 remains undispatched to preserve the
plan's C3/D3 sequencing of `test/contracts/scenarios.json` (next after A2 →
G3 → B3). F2's lifecycle files are disjoint from that manifest. F2 must
re-evaluate the stable-address prerequisite against #536 rather than inherit
F1's block or F4's single-node pass.

Still blocked after W5: **E4** (E3 code is merged, plus unresolved Q2/Q5),
**F3** (C3, E4, F2, plus Q2), **G4** (F3, G6) and **G6** (C3, D3, E4, F2; B3,
G2 and G7 code is merged).

### Stream Status

| Stream | Scope | Priority | Status |
| --- | --- | --- | --- |
| A | Contracts and scenario evidence (2 items) | P0 | Complete: A1 merged #465, A2 merged #470. Three manifest rows remain `proven` with `gates: ["early"]`. B2/B3 rows now have selectors but stay `absent` until a live `TestCore` run; D3/G6 follow |
| B | Real multi-node robustness (3 items) | P0 | B1 merged #472 (`early-evidence`); B2 merged #555 with live kind proofs. B3 merged #557: `TestCore` oracles are fail-closed, but no live kind proof was recorded, so the checkbox stays open. Default selection is still `TestOwnerCrash` |
| C | Reference models, generated tests, and checker validation (3 items) | P0 | C1 merged #475; C2 merged #551. C3 is in progress in W6-α; D3 remains undispatched this wave |
| D | Developer and Console journeys (3 items) | P0 | D1 merged #476 and D2 merged #482 (#479/#480 later fixed by #532; #483 remains). D3 is dependency-ready for W6 |
| E | Correct load reporting and performance comparison (5 items) | P0 | E1 merged #466, E2 merged #552, E5 merged #471. E3 merged #559 as a fail-closed comparator; no live two-image run, so the checkbox stays open. E4 still needs Q2/Q5 |
| F | Upgrades, durability, and sustained faults (4 items) | P1 | F1 merged #474+#478; F4 merged #554. F2 is in progress in W6-β and must re-evaluate #536 rather than inherit F1's block. F3 needs C3/E4/F2 |
| G | Diagnostics, coverage, and CI enforcement (7 items) | P0 | G1, G2, G3, G5 and G7 merged. G2's reviewed commit now has a live CLI/server collect; browser contribution remains incomplete. `ci-ok` is still not a required check (Q6). G6 then G4 follow |

## Streams

Paths prefixed with `new` below are proposed files/directories. Directory
ownership covers cohesive helpers and test fixtures within that directory; it
does not grant adjacent product edits. Each implementation PR includes its tests and exact invocation/evidence in
its PR description and assigned plan notes. N-1 synchronizes the shared runbook
once the wave's implementation PRs have merged; the wave is not complete before
that sync. Any separately scoped product behavior fix still includes its
required product documentation in the implementation PR. Shared-file sequencing
below is mandatory, including append-only edits.

### Stream A — Contracts and evidence

- [x] A1. Resolve the first fault contracts and choose the smallest usable harness
  Files: `docs/exec-plans/active/distributed-testing.md` (A1 Strategic Decisions record only).
  Depends on: none.
  Verify: Name each public operation, identity, acknowledgement point, safety/liveness oracle, fault/clock assumptions, and unresolved guarantee, including rejection while quorum is absent. First compare the existing Helm StatefulSet/headless membership plus CI kind setup with Testcontainers/Toxiproxy; the default B1 design reuses the chart, enables persistence, and kills a pod. Record exact image, node/owner discovery, test runner, and recorder reachability commands. A bounded spike must establish whether peer address advertisement permits proxies before adopting them. Compare a small injectable clock seam in lease/owner/worker renewal with existing Go timer test facilities, short test-only lease configuration, and external process pause; a clock seam does not replace real crash/commit faults. Enumerate any indispensable event-dispatch hook and sibling handoff, rather than assigning five product files speculatively. B1's supported owner-crash contract can be settled independently of the later partition/clock/storage envelope; retain unresolved Q1–Q6 entries.

- [x] A2. Introduce a scenario manifest and a validator for complete evidence.
  Files: new `test/contracts/scenarios.json`, new `scripts/check-test-evidence.py`, new `scripts/test_test_evidence.py`.
  Depends on: A1, G1.
  Verify: each included contract maps to named real-surface scenarios and required topology/mode/feature flags, expected observations, and allowed skips with reasons. Validate schema and duplicate IDs, then reject synthetic reports with missing scenarios, unexpected skips, disabled gates, wrong artifact identity, absent fault-activation evidence, or checker timeouts. Distinguish pass, fail, and inconclusive. The manifest must not contain rows marked proven for scenarios that do not yet exist; G3 reconciles the early gate and G6 the full suite.
  Merged: [#470](https://github.com/caesium-cloud/caesium/pull/470), merge `0c42e549f5983fb4c8bf78cbfdf9fae698b21f41` (W2-γ). Delivered the catalog covering all 11 A1 contract IDs with topology, mode, feature flags, expected observations, skip policy, identity fields and fault-activation requirements, plus the stdlib-only fail-closed checker (exit 0 pass / 1 fail / 2 inconclusive, or 1 with `--strict`) and its unit module, which G1's wildcard already discovers. Limits: every committed row is `status: absent` with empty `gates`, so no scenario is proven and the unbuilt B1/B2/B3/D3/E5/G3 rows are named as planned only; G3 registers selectors and the early gate. Verified with Python alone — no Docker, no live report.

### Stream B — Real multi-node robustness

- [x] B1. Reuse the Helm topology and ship the first real owner-crash regression
  Files: new `test/robustness/cluster/`, new `test/robustness/owner_crash_test.go`, new `test/robustness/recorder/`, new `scripts/robustness.sh`, new `build/Dockerfile.robustness`, new `helm/caesium/ci/test-values-robustness.yaml`.
  Depends on: A1.
  Verify: Use the existing chart and CI kind lifecycle with replicaCount=3 and persistence enabled, candidate images loaded into an owned cluster, and unique namespaces/volumes. Do not invoke the Docker Desktop registry recipe unchanged or alter a user's current cluster. Put Docker/Kubernetes-dependent Go drivers and their helpers behind `//go:build integration`; compile their runner in the container toolchain. Observe three real dqlite members and a quorum, apply/trigger a blocked multi-step job through HTTP/CLI, identify its actual owner, terminate that pod without graceful application shutdown, and observe a surviving owner finish the accepted run. Exercise owner=leader and owner!=leader, then restart/rejoin the old pod with its retained volume. Use a small HTTP effect sink hosted by the test runner outside the faulted pods; persist raw starts/completions and verify public final state. This is a complete named regression, not scaffolding waiting for C1/B2/B3. Reject accidental single-node startup, missing task/sink connectivity, absent kill evidence, and wrong candidate image digests. Demonstrate two isolated harness instances coexist and clean up only owned resources; no Testcontainers dependency is required for this first topology.
  Merged: [#472](https://github.com/caesium-cloud/caesium/pull/472), merge `f3f5ce3ab68d57709127a0b2807e670e7d39726a` (W2-α). Delivered `scripts/robustness.sh` (owned kind cluster of 1 control-plane + 3 workers, candidate/runner/task image load, three persistent Helm replicas with required hostname anti-affinity), `build/Dockerfile.robustness`, `helm/caesium/ci/test-values-robustness.yaml`, and the integration-tagged `test/robustness/` runner with `TestOwnerCrash` covering `owner_is_leader` and `owner_is_not_leader`. The in-cluster runner sits on the control-plane node with namespace-scoped RBAC, records start/complete nonces through a sink on `:8090`, correlates them to public task-run identity after recovery, retains duplicate attempts, and reads membership through dqlite `Leader`/`Cluster` RPCs against `podIP:9001` plus a UUID-validated `POST /v1/database/query` lease read. The host controller cordons the owner worker before triggering and kills via `systemctl stop kubelet` + `ctr tasks kill`, requiring the container ID to appear in `ctr tasks list` and then stop or disappear. Limits: the passing run is an orchestrator-owned live kind proof of candidate `b716c6db` (2026-09-12), **not** a CI lane — G3 wires the runner, image and selectors. `PRAGMA query_only` inside `POST /v1/database/query` may be rejected by native dqlite; that is a recorded product risk, not permission to skip lease observation.

- [x] B2. Add targeted faults and correlate public event and effect histories
  Files: new `test/robustness/faults/`, new `test/robustness/history/`, new `test/robustness/faults_test.go`, `test/robustness/recorder/` (created by B1), new `internal/testfault/`, `internal/event/bus_dispatch.go` (only A1-approved dispatch hook; EX-HOOKS required), `build/Dockerfile.robustness` (created by B1).
  Depends on: B1, C1; external EX-HOOKS for the instrumented event case.
  Verify: Add external pause/resume, asymmetric network partition, and delayed/dropped-response control with independent activation/heal evidence. Verify discovered peer/worker addresses traverse the injector. Extend B1's test-runner recorder with an SSE subscriber using the existing `/v1/events` surface; retain duplicates and compare reconnection results with persisted public event reads using the documented sequence scope, not an assumed gap-free global counter. SSE is implementation-produced evidence and cannot reveal an external effect whose completion event was lost; preserve the separate raw task-effect ledger. Keep possibly committed timeouts and controller-observed ordering; recorder loss is inconclusive. Limit product instrumentation to an A1-justified durable-event-before-publication boundary in `internal/event/bus_dispatch.go`, with test-only controls absent/inert in release builds. Do not edit `internal/run/store.go`, owner completion, dispatch handlers, or worker runtime speculatively. Before dispatching the hook work, EX-HOOKS must identify the actual merged sibling base and exclusive file owner; otherwise that case is blocked, never skipped as passing. Run the ordinary release image without faults as a baseline.

  Note (W4-α): all five capabilities are live-proven on owned kind clusters; nothing in this item is asserted from a unit test alone. **EX-HOOKS re-verified at the execution base**: the last commit touching `internal/event/bus_dispatch.go` is still `573edfee` (PR #423), identical to A1's snapshot, and the only open PR (#449, resource-right-sizing W1-α) does not list the file, so W4-α held exclusive ownership. The hook is one test-only predicate consulted after the event row is durably committed and immediately before its first `bus.Publish`, on BOTH paths; live evidence shows member `caesium-1` entering it via `dispatch_once` AND `publish_and_mark` for the same sequence 40, so the background dispatcher cannot bypass an armed pause. Store marking stays after publication: while held the row read back `bus_dispatch_pending=1, bus_dispatched_at=NULL`, and after the test disarmed it (release reason `disarmed`, never `max_hold`) it became `pending=false dispatched_at=2026-09-17T13:14:08Z`. **Compile-time absence** (`internal/testfault` release twin with `const Enabled = false`): the release binary contains ZERO occurrences of `testfault` in any form (markers `caesium-testfault-control`, `CAESIUM_TESTFAULT_DIR`, `bus-publish-pause.json`, `internal/testfault`, `testfault` all 0; instrumented 1/1/1/13/18). `scripts/robustness.sh` now runs that scan against the built release image on EVERY run, including the merge-gate lane, and refuses to deploy a release image carrying a marker; it also requires the instrumented image to carry them so a build tag that failed to apply cannot make the assertions vacuous. The control surface is a file on the pod's own emptyDir written through the host controller's container runtime — no listener, port, route, token or new RBAC. **Fault controls** (`TestTargetedFaults`, all PASS, candidate `e6fbb698`, cluster `rbw4a-5de548eec4d5`): `external_pause_resume` (45.6s) froze a non-leader via `ctr tasks pause` with kubelet stopped first so a liveness probe could not turn resume into replace — runtime `PAUSED` plus the member ceasing to answer `/health` are two independent activation observations; held 42.0s against the 30s run lease (`past_lease=true`), resumed to `RUNNING` and reachable, membership back to 3. `asymmetric_partition` (58.4s) used EXTERNAL iptables rules inside the owned kind nodes (A1 forbade proxies without the routing spike; none was adopted), keyed by the addresses the dqlite `Cluster` RPC actually returned: the DROP rules' own packet counters prove both discovered peer routes traversed the injector (`drop-9001` 126 pkts / 9256 B for Raft, `drop-8443` 6 pkts for the dispatch route derived from the same advertised host; `dispatch_route_counted=10`), the blocked direction failed from inside the source member's own namespaces (`wget: download timed out`), and heal removed exactly the tagged rules with both directions plus a 3-member quorum recovering. **Limit found and recorded**: the destination→source direction ALSO fails while partitioned, because the cut-off member loses the Raft leader and its own quorum-dependent API degrades; that is a consequence of the fault, not a checker weakness, so the asserted open-direction oracle is an UNPARTITIONED third member still reaching the destination on the same port, and the reverse probe is retained as an observation. `response_loss_possibly_committed` (20.3s) is a client-side interposer on the runner's own route (the only place a loss can follow an OBSERVED commit): both the dropped-response and delayed-past-client-deadline variants recorded the upstream 202 and run UUID, marked the operation POSSIBLY COMMITTED, and reconciled the run by identity on a DIFFERENT member — a client timeout is never recorded as a rejection. `event_history_correlation` (24.1s) compares SSE delivery against persisted rows as a SET inside the read scope (`store=http://10.244.2.3:8080 min=8 max=17 rows=10 complete=true`); it observed 6 delivered against 10 persisted with `missing_from_delivery=[10 11 13 14]` and correctly reported those as LEGAL rather than defects, kept duplicates and out-of-order arrivals legal, made no gap-free or high-water-mark assumption, and correlated the separate raw effect ledger (2 raw completions against 2 completion events, no phantom). **Release baseline**: the unchanged `TestOwnerCrash` lane ran on the ordinary RELEASE image in the same hold — `owner_is_leader` 57.2s and `owner_is_not_leader` 61.7s both PASS (cluster `rbw4a-eb8ac12ef947`) — and the default selection renders byte-identical runner args to B1's, so the `early-evidence` merge gate is unaffected. **Scope extension** (orchestrator ruling, recorded so it is not silent): B2's Files line omits `scripts/robustness.sh` and `test/robustness/hostlogic.py`, but pause/partition/heal are host-controller actions and the ownership table serialises the cluster harness B1 → B2 → B3, so W4-α edited both as the next serialised writer; `justfile`, the bake files, the workflow and `test/contracts/scenarios.json` were NOT touched, and new tests are selected through `CAESIUM_ROBUSTNESS_RUN`, which defaults to `^TestOwnerCrash$`. B3 registers these scenarios. Nothing is reported blocked.**Round 1 adversarial review (PR #555) — five findings, all fixed and live re-proven on `67baee6f` (cluster `rbf-70598d545880`), not argued away.** (P1) The SSE reconnection could fail open: `Connect` errors were only logged, an empty resumed set passed, and the cursor sat at the stream tip so there was no catch-up to exercise (the first run's `resumed_distinct=0` was exactly that). The resumed attempt is now part of the comparison — not established, a non-cancellation transport/parser error, an empty resumed set, or no persisted rows above the cursor are INCONCLUSIVE and fail — the cursor is the run's LOWEST persisted sequence, and a persisted row above it that the resume did not replay is now a DEFECT, because `Last-Event-ID` catch-up is a durable-store read (`Store.ListSince`), not at-least-once bus delivery: live `cursor=8 established=true status=200 resumed_distinct=9 catch_up_expected=[9..17] missing_from_resumed=[]`. (P2) Dispatch-route traversal was recorded but not required, and the counter summed the blocked and open rules. The partition now puts real dispatch work in flight BEFORE the cut and reads both halves from the product — the run's owner from its `run_leases` row, the executing member from the task's `claimed_by` — so the worker's completion report to the owner's discovered `https://podIP:8443` must cross the cut; the BLOCKED rule's own counter must be positive or the subtest fails the route as UNPROVEN, and the open direction is recorded but never added to it: live `rule rbp-e5be0783cc-drop-8443 matched 2 packets / 104 B from caesium-2 toward the owner caesium-0's discovered dispatch address 10.244.2.3:8443, rising to 4 packets over the window; open direction 4 packets, recorded only`. (P2) The delayed-response variant asserted no ordering, so a slow upstream could have explained the loss; the client outcome time is now recorded and `UpstreamAt < client failure` plus `Applied == armed mode` are required, otherwise inconclusive: live `drop: upstream_202_at ... < client_failed_at ... lead 53µs applied=drop; delay: lead 3.945104835s applied=delay`. (P2) The response interposer had no heal evidence; a fresh matching request now goes through the SAME interposer after each disarm and must return a usable 202 within a bound, and `Once` was dropped from both policies so the disarm — not policy expiry — is what the probe proves: live `dropped_response 202 in 50.765ms, delayed_response 202 in 57.316125ms, both through the same interposer`. (P2) `CorrelateEffects` compared run-wide counts, so one task's duplicate completions could mask another task's phantom completion; task identity is now carried from SSE and from `execution_events.task_id`, mapped to fixture steps through the shipped `GET /v1/jobs/:id/tasks`, and compared per step with raw duplicates retained, with unattributable identity reported inconclusive: live `per_step first{raw 1, delivered 1, persisted 1, phantom 0} second{raw 1, delivered 1, persisted 1, phantom 0}`. The reviewer's exact counter-example is a hermetic negative control (`TestEqualAggregatesAcrossDifferentTasksIsADefect`), alongside five new reconnection controls. Subtest order changed for one reason, recorded here: `asymmetric_partition` deliberately leaves long-running dispatch work in flight, and `bus_publish_pause` holds EVERY `task_started` publication while armed, so the partition now runs last.
  **Second recorded scope extension (orchestrator, 2026-09-18).** Making the runner selection configurable removed the literal `"-test.run", "^TestOwnerCrash$"` and the per-subtest `pass_line '…'` lines that G3's guard `EarlyEvidenceLaneTests.test_registered_selectors_name_real_tests` in `scripts/test_ci.py` asserted, so `ci-config` went red on this PR. Rather than keep dead literals in `scripts/robustness.sh` to satisfy a stale assertion, that ONE test was updated to assert the equivalent facts about the new mechanism: the script's default is `^TestOwnerCrash$`, that pattern is what reaches the runner, neither the workflow nor the justfile sets `CAESIUM_ROBUSTNESS_RUN`, the owner-crash branch's `REQUIRED_SUBTESTS` contains both registered subtests, and the enforcement loop still dies on a missing PASS line. `scripts/test_ci.py` was reserved for G7 this wave; G7 (#550) had merged and no other W4 stream touches it, so this is a serialized edit, not a concurrent one. Nothing else in that file changed. **Round 2 adversarial review (PR #555) — two findings, both fixed and live re-proven on `fd2be6f1` (cluster `rbf-c2b947fe0db3`).** (P2) Only the RECONNECT attempt was carried into its comparison: the INITIAL subscription's outcome was logged and dropped, so a recorder that delivered one event and then broke left a non-empty set and every row it missed read as legal at-least-once loss — the opposite of B2's "recorder loss is inconclusive". `history.Compare` now takes the same `history.Connection`, and the subscriber goroutine also records termination that was NOT the test's own teardown, so a clean early EOF is inconclusive too: live `first connection: status=200 delivered=9 established=true err=""`, with the hermetic control `TestBrokenSubscriptionIsInconclusiveNotLegalLoss` (same sets, `Err: unexpected EOF` → not conclusive, no defect, undelivered rows still recorded). (P2) The hook-release poll skipped any member whose current log summarised empty, and the host's `testfault-log` action ran `cat … 2>/dev/null || true` and acked `ok`, so a vanished log looked like a member that never entered the hook. Releases are now reconciled against the holds ACTIVATION recorded — `faults.ReconcileReleases` requires, per member and per sequence, that the entry is still recorded and that a `disarmed` release exists for it — the host action reports a read failure instead of acking ok, and the last read error is carried into the failure message: live `hook entered on caesium-0: paths=map[dispatch_once:1 publish_and_mark:1] sequences=[60]` reconciled by `released on caesium-0: held=[60] releases=2`, `caesium-1`/`caesium-2` held=[60] releases=1. Six hermetic controls cover the vanished log, a missing member, an unreleased hold, a `max_hold` release and an unmatched sequence. `TestTargetedFaults` 5/5 PASS and the unchanged `TestOwnerCrash` 2/2 PASS on the release image (`rbo-4c75c6bbd9a4`); `python3 -m unittest discover -s scripts -p 'test_*.py'` → `Ran 194 tests … OK`.
- [ ] B3. Extend the core failure suite with fenced recovery, dispatch, and authorization cases
  Files: new `test/robustness/core_test.go`, new `test/robustness/testdata/`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B2, G3.
  Verify: Extend B1's owner-crash regression with owner pause past lease/stale completion, 2–1 split/heal, commit-before-response loss, durable-event-before-delivery crash, and cancel/retry/completion races, including fan-out/fan-in and frozen retry recipes. Make a worker unreachable, observe network-error rejection and peer benching through actual dispatch progress and available `caesium_dispatch_rejected_total`/`caesium_dispatch_stalled_total` metrics, then prove it receives new work after the cooldown. Send wrong-token `/internal/dispatch` and `/internal/complete` requests between real processes; cover valid-token stale-generation refusal separately and invalid peer credentials where mTLS is supported. Assert denied operations have no task/state/effect mutation. During quorum loss, enforce A1's bounded error contract only where resolved; retain uncertain writes and do not equate client timeout with rejection. Check accepted-state durability, generation authority, legal terminal/dependency behavior, raw external attempts, and recovery after healing. Persist histories/fault timelines/topology/digests; missing fault activation or stalling cannot pass.
  Note (W5-α): shipped `TestCore` (selected via `CAESIUM_ROBUSTNESS_RUN='^TestCore$'`; default remains `^TestOwnerCrash$`) composing B2 pause/partition/interposer/hook controls. Catalog B2 selectors now point at `TestTargetedFaults/*`; B3 rows stay `absent` with empty `gates` (no live kind proof in this PR; G6 owns promotion). Durable-event crash skips unless the instrumented image is deployed, and `scripts/robustness.sh` requires that subtest only then. DT-QUORUM-01 bounded rejection is not claimed.

### Stream C — Generated state transitions and checker strength

- [x] C1. Implement independent pure models and generated lifecycle tests
  Files: new `test/model/`, new `internal/run/model_properties_test.go`, new `internal/run/recovery_properties_test.go`, `go.mod`, `go.sum`.
  Depends on: A1.
  Merged: [#475](https://github.com/caesium-cloud/caesium/pull/475), merge `5ecd95d8374ebcf281c835a9f9cde21dd848b34c` (W3-β).
  Verify: Generate bounded DAGs and sequences of admission, completion, cancellation, retry, lease expiry, checkpoint, and recovery using Rapid; minimize and retain failures. The independent `test/model` package is deliberately untagged, pure Go, and has no cluster/client startup, Docker socket, real network, or product decision-function dependency, so it belongs in `just unit-test`. Any test that launches external infrastructure must instead be integration-tagged and run by the system runner. Use Porcupine only for valid sequential contracts, with separate liveness/event models. Check checkpoint/replay equivalence and partition accounting with hermetic fixtures; compare real execution modes in B3 rather than claiming the pure model proves wiring. C1 owns its used model dependencies; adding a future proxy/container library needs a separately assigned owner.
  Note: shipped with `pgregory.net/rapid v1.3.0` and `github.com/anishathalye/porcupine v1.3.0` (toolchain-generated sums; `go mod tidy` also promoted the already-direct `prometheus/common` out of the indirect block). `test/model` is untagged pure Go and `independence_test.go` enforces its import allowlist mechanically, so the no-product-dependency and hermetic claims are a test rather than a convention. Porcupine checks only the run-status register; delivery, liveness and lease safety have separate models, each with a negative control proving the oracle rejects a planted defect. Retained minimized failures are committed as deterministic tests in `test/model/regression_test.go`, not as Rapid `.fail` artifacts: a fixed defect's bitstream replays as "no longer valid" and documents nothing, so the workflow in `doc.go` keeps `.fail` files only while a defect is open. The four counterexamples found were all in the new model; the product's `RunState`/recovery paths agreed with it under 4000+ generated cases per property. No cluster evidence is claimed — B1/B3 remain the only source for that.

- [x] C2. Expand native fuzzing and deterministic concurrency regressions.
  Files: `pkg/jobdef/schemacompat/fuzz_test.go`, `internal/jobdef/diff/fuzz_test.go`, `internal/trigger/cron/fuzz_test.go`, new `internal/run/descriptor_fuzz_test.go`, new `internal/run/recovery_fuzz_test.go`, new `internal/worker/renewal_synctest_test.go`, new `scripts/fuzz-tests.sh`.
  Depends on: C1.
  Verify: the containerized script discovers/selects each intended fuzz target, performs a bounded exploration rather than seed-only execution, and preserves corpus artifacts. Persist minimized failures as normal regressions. Exercise isolated timer/cancellation/renewal logic with `testing/synctest` where supported; real sockets and CGO/dqlite remain outside its deterministic claim. Repeat selected concurrency tests with race detection and varied scheduling. No sleep-only success oracle or arbitrary valid-input rejection substitutes for a property.
  Note (W4-δ): 8 fuzz targets across 4 packages, each asserting a property
  beyond "didn't panic" — reflexivity (`Compare(s,s)` never reports a schema
  breaking against itself, `pkg/jobdef/schemacompat`), differential agreement
  between two independent real decode paths (`decodeDefinitions` vs.
  `schema.Parse`, `internal/jobdef/diff`; `extractExpression`/`extractLocation`
  vs. the real `ParseSchedule` consumer, `internal/trigger/cron`), round-trip
  fixed point at the byte level plus decode-gate determinism
  (`FuzzTaskExecutionDescriptorRoundTrip`, the frozen `TaskExecutionDescriptor`
  every replay/deadline/worker consumer decodes identically), idempotence and
  conservation (`FuzzMergeDescriptorSecretRefs`), and checkpoint/recovery
  agreement plus corrupted-checkpoint-fallback-equals-from-scratch-replay
  (`FuzzValidateCheckpointBlob`, `FuzzRecoverRunStateTerminalRows`, new
  `internal/run` targets, hermetic, no DB/build tag). `scripts/fuzz-tests.sh`
  re-execs itself inside the `caesium-builder:latest-full` image (POSIX `sh`,
  no bash in that image), mirrors `unit-test`'s repo mount and
  `ui/dist/index.html` touch, adds its own GOCACHE volume for corpus
  persistence, discovers targets via `go test -list '^Fuzz'` per package and
  fails on any declared/discovered mismatch, runs one target per `go test
  -run=^Name$ -fuzz=^Name$ -fuzztime=<budget>` invocation, parses `execs:`/`new
  interesting:` to reject a seed-only or zero-exec run, exports
  `testdata/fuzz/<Target>` plus the exploratory GOCACHE corpus to a host
  artifact dir, and repeats a selected `internal/worker` renewal-test set
  under `-race -count=3` at `-cpu=1,2,4` (one `go test` per cpu value rather
  than a single `-cpu=1,2,4` invocation, for clean per-configuration
  logs/results). Measured real run (`CAESIUM_FUZZ_SECONDS=20s`, this base):
  all 8 targets explored genuinely (23–75s each incl. per-target build,
  61k–358k execs, 123–242 new-interesting entries, zero crashers) and all 9
  concurrency-matrix configurations passed (72 pass/0 fail at each of
  cpu=1,2,4). Negative-proof evidence (uncommitted, reverted after): (a)
  temporarily narrowing a package's declared-target list made the script fail
  fast at the discovery step with the exact undeclared-target message, exit 1;
  (b) temporarily planting `t.Fatalf` at the top of `FuzzExtractLocation`'s
  body made the run report `CRASH: FuzzExtractLocation failed`, print the
  guaranteed-correct `go test -run=<Target> ./<pkg>` reproduction command, keep
  processing every remaining target/concurrency config, and exit 1 overall.
  `internal/worker/renewal_synctest_test.go` drives the real `runLeaseRenewal`/
  `runRunLeaseRenewal` goroutines (worker.go) through `testing/synctest`
  against the package's existing `LeaseRenewer`/`RunLeaseRenewer` fakes — no
  production edit, no new clock seam (A1 deferred that): renewal fires exactly
  once per configured tick in fake time, cancellation stops the goroutine with
  no leaked reader on the ticker (proven by advancing fake time further and
  observing zero additional calls, and by `synctest.Test` itself refusing to
  return over a still-blocked goroutine), a proven lease loss surfaces as task
  context cancellation on the very next tick (replacing
  `TestRunLeaseRenewalTickCancelsLostClaim`'s real
  `time.After(10*time.Second)` wait with a deterministic one), a transient
  renewal error is retried on the following tick rather than killing the loop,
  and a slow renew call structurally cannot overlap a second one (single
  select-loop goroutine; pinned against a future concurrent-dispatch
  refactor). Out of reach, stated per A1's Clock table: real dqlite
  `RenewLeases`/`RenewOwnedLeases` SQL behavior, real wall-clock/host-jitter
  interaction, and the HTTP dispatch path — B-stream's live cluster and the
  non-synctest tests in the same package remain the source for those. One real
  (very minor, currently unreachable) product defect found by reasoning about
  `mergeDescriptorSecretRefs`'s empty-existing fast path before ever running
  the fuzzer: filed as
  [#549](https://github.com/caesium-cloud/caesium/issues/549); not fixed (out
  of file scope) and `FuzzMergeDescriptorSecretRefs`'s properties are scoped to
  what the function actually guarantees rather than asserting the disproven
  claim. `just unit-test` (`-race`, full `./...`) and `just lint` both pass
  clean on the final state; `just integration-test` was not run — this
  stream's diff is test-only plus a script, with no `test/` scenario added.
  Review-fix round (PR #551, three verified P2 findings, all confirmed real
  before fixing): (1) `FuzzDecodeDefinitions`'s cross-check compared
  `decodeDefinitions` against `schema.Parse` whenever exactly one definition
  was yielded, but `len(viaDecode)==1` does not mean "one document" —
  `decodeDefinitions` silently skips a blank document
  (`isBlankDefinition`) before deciding what to hand its callback, so
  `"{}\n---\n<valid manifest>"` yields one accepted definition while
  `schema.Parse` (first-document-only) rejects the leading blank `{}` —
  a false positive. Fixed with `singleYAMLDocument`, an independent
  `yaml.Node`-based document-count check gating the comparison; the exact
  reported input is now a committed passing seed. (2) `fuzz-tests.sh`'s
  crasher export resolved Go's printed `testdata/fuzz/<Target>/<hash>` path
  (relative to the PACKAGE directory, since that's the test binary's cwd)
  against the repo root instead, so the file existence check always failed
  and the corpus-preserving `cp` silently no-opped; the same `continue` also
  skipped the correctly-pathed directory-level copy on any crash. Fixed by
  resolving the printed path against `$pkg` and moving the directory-level
  corpus copy before the crash branch so it always runs. Re-verified with a
  GENERATED (not seed) failing input — planted a single-byte trigger
  one bit-flip from a seed, `go test` discovered `[]byte("\x01")` via
  mutation in 6s, and the artifact dir ended up with
  `corpus/FuzzValidateCheckpointBlob/97dc7172b48e6ffd` containing that exact
  byte, reproduction command printed as
  `go test -run=FuzzValidateCheckpointBlob ./internal/run`. (3) the
  undeclared-target guard only ever inspected `go test -list` output within
  the four hardcoded `PACKAGES`, so a new `FuzzX` in any other package was
  invisible and the run still exited 0. Fixed with a repo-wide sweep (`find
  -prune` over `.git`/`vendor`/`node_modules` piped through `grep -H -n`
  against a `func FuzzX(<any identifier> *testing.F)` pattern, since BusyBox
  grep has no `--include`/`--exclude-dir`) cross-checked against the
  manifest in both directions. Re-verified by planting
  `FuzzNegativeProofUndeclaredPackage` in
  `pkg/dbtrace` (outside `PACKAGES`): the sweep caught it and failed before
  any target ran. All three fixes re-verified with a full clean
  `scripts/fuzz-tests.sh` run (all 8 targets genuinely explored, zero
  crashers, all 9 concurrency configurations passing) plus `just lint` and
  `just unit-test` (`-race`, full `./...`), both clean; all planted
  fixtures were uncommitted and reverted.
  Review-fix round 2 (PR #551, three more verified P2 findings): the
  concurrency matrix required only `rc==0`, so `-count=0`, a `-run` pattern
  matching nothing, or an all-skipped selection all reported a false
  `OK … pass=0 fail=0`. Fixed: `CONCURRENCY_COUNT` is validated as a
  positive integer up front, and every `-cpu` configuration now also
  requires `passed > 0` and `failed == 0`. Re-verified by swapping
  `CONCURRENCY_RUN` to a non-matching pattern: all three `-cpu` configs
  correctly reported `ran ZERO subtests`, exit 1. The repo-wide sweep regex
  hard-coded the `*testing.F` parameter name as literally `f`, so
  `func FuzzX(fuzz *testing.F)` was invisible to it. Fixed: the pattern now
  matches any valid identifier there. Re-verified by planting
  `FuzzProbe(fuzz *testing.F)` in `pkg/dbtrace` (outside `PACKAGES`): the
  sweep caught it, exit 1. The `execs > 0` exploration check accepted a
  budget that expired during baseline/seed replay (Go's `execs:` counter
  includes those), reporting a false `OK` with zero real mutation. Fixed:
  the completed-baseline denominator is parsed from Go's own
  `gathering baseline coverage: N/N completed` line and `execs` must exceed
  it. Re-verified with `-fuzztime=3x` (Go's `-fuzztime` also accepts an `Nx`
  iteration-count form, not just a duration) against seed counts of 4–11 per
  target: every target correctly failed as `at or below its baseline`,
  exit 1. All three re-verified together with a full clean
  `scripts/fuzz-tests.sh` run (8/8 targets genuinely explored, 0 crashers,
  9/9 concurrency configs passing) plus `just lint`/`just unit-test`, both
  clean; all fixtures uncommitted and reverted. (Round 2 also fixed an
  unrelated guardrail trip: `internal/guardrails`' bare-image-ref scanner
  scans `.md` among other extensions and treats a lowercase mention of the
  BusyBox project's name as an unpinned image reference — reworded to the
  capitalized project name throughout this Note.)
  Round 3 (orchestrator fix-forward): the sweep still required the literal
  selector `testing.F`, so a target declared through an aliased import
  (`import test "testing"` -> `f *test.F`) or with its parameter list split
  across lines stayed invisible. The sweep now matches ANY top-level
  `func FuzzX(` in a test file regardless of signature; over-matching is the
  safe direction, since a helper that merely happens to be named `FuzzX` is
  flagged and must be declared or renamed. Checked against the tree (exactly
  the 8 declared targets match) and re-verified by planting
  `FuzzAliasedImportProbe` and `FuzzMultilineProbe` in `pkg/dbtrace`: both
  were reported by name and the script exited 1 before running any target.

- [ ] C3. Prove the checkers still detect known classes of defects.
  Files: new `test/model/oracle_regression_test.go`, new `test/model/testdata/`, new `scripts/validate-test-oracles.sh`.
  Depends on: A2, B3, C2.
  Verify: lost acknowledged state, accepted stale generations, invalid fan-in, missing replay, and unaccounted external effects fail their respective checkers. Include legal duplicate delivery and ambiguous timeout histories that must not be falsely rejected. Reproduce selected historical defects or temporary intentional mutations in an isolated checkout with a recorded known-bad SHA/patch; the tests catch them and the fixed candidate passes. Never ship mutations or change the user's working tree to run this validation. Missing evidence and checker resource exhaustion cannot become green.

### Stream D — Developer and Console journeys

- [x] D1. Extend binary-driven developer workflows and cleanup assertions.
  Files: `test/local_dev_test.go`, new `test/developer_journey_test.go`, new `test/developer_testdata/`.
  Depends on: A1.
  Merged: [#476](https://github.com/caesium-cloud/caesium/pull/476), merge `b26fb6137e3a455a4ec9a9ec4bd188ac66c87a81` (W3-γ). Product defects found by the new scenarios and filed rather than worked around: [#479](https://github.com/caesium-cloud/caesium/issues/479) (`dev --once` panics on an unreachable kubernetes engine) and [#480](https://github.com/caesium-cloud/caesium/issues/480) (`dev --once` ignores SIGINT and orphans its container).
  Verify: use the container-built release CLI in an empty temporary workspace for lint, preview, dev-once, watch/edit, interrupt, apply, and inspect. Check malformed input, paths with spaces, unavailable engines, cancellation/timeouts, and owned-resource cleanup. Parse JSON exclusively from stdout captured separately from stderr and assert exit status. Preserve existing helpers; testscript is an optional future harness substitution, not a reason to rewrite working tests. Run the currently shipped Linux architectures; add native platforms only after Q4 confirms support. Inspect the job-definition reference before writing any YAML fixtures.

- [x] D2. Expand functional, visual, accessibility, and scale browser coverage.
  Files: new `ui/e2e/accessibility.spec.ts`, new `ui/e2e/visual.spec.ts`, new `ui/e2e/scale.spec.ts`, new `ui/e2e/network-recovery.spec.ts`, new `ui/e2e/visual.spec.ts-snapshots/`, `ui/e2e/helpers/fixtures.ts`, `ui/package.json`, `ui/package-lock.json`, `ui/playwright.config.ts`.
  Depends on: A1, G1.
  Merged: [#482](https://github.com/caesium-cloud/caesium/pull/482), merge `894f645b85d4b17e5bb0abac6bd9d4903501fb12` (W3-δ). Console accessibility debt found by the axe baseline is filed as [#483](https://github.com/caesium-cloud/caesium/issues/483); shrinking that baseline is product-code work outside this stream.
  Verify: against the live backend, check create/apply or existing authoring workflows, trigger, logs, failure diagnosis, retry/cancel, reload, permission denial, credential expiry, reconnect, and stale-request races. Require keyboard/focus behavior and scoped axe checks. Review deterministic screenshots with fixed fonts, viewport, timestamps, and dataset. Load large DAGs, many partitions, paginated history, and long logs; assert all expected data remains reachable through virtualization/pagination. Fail on unexpected console/page errors. Chromium remains required; Q4 selects additional supported browser projects. Synthetic response manipulation tests are explicitly labeled and do not replace live persistence tests.
  Note (W3-δ): create/apply, trigger, logs, failure diagnosis, and retry/cancel
  against the live backend were already real-surface-covered by existing
  spec files (`operator-flow`, `job-queue`, `replay`, `why`, `blame`); the four
  new files add accessibility/keyboard, deterministic-visual, scale, and
  reload/reconnect/credential/race coverage without duplicating that ground.
  `accessibility.spec.ts` scopes axe to WCAG2/2.1 A+AA critical/serious
  findings and records a `KNOWN_VIOLATIONS` baseline of real, pre-existing
  product defects it found (systemic icon-only buttons with no accessible
  name; several muted-text/badge color tokens below 4.5:1 contrast) — `ui/src/**`
  is out of this stream's scope, so the gate catches a NEW rule-id regression
  per page rather than asserting away debt it cannot fix; shrinking that
  baseline is a future product-code PR. `visual.spec.ts` commits only
  `-linux.png` baselines (generated in a pinned `mcr.microsoft.com/playwright`
  container matching the exact npm-resolved version) and each test
  self-skips off Linux, since `just ui-e2e` runs the browser on whatever host
  invoked `just`, not in a container — a local darwin/win32 run reports
  "skipped", and the real per-pixel comparison is CI's `ui-e2e` check.
  "Paginated history" is covered via the fanned-partition list's own
  `next_offset` cursor walk (the job run-history list itself has no
  pagination/virtualization to exercise). `network-recovery.spec.ts`'s
  permission-denial/credential-expiry cases are SYNTHETIC (this project's
  default e2e server runs without `CAESIUM_AUTH_MODE`); real scope-based
  denial is already covered live in `ui/e2e/auth/*`.

- [ ] D3. Exercise the operator journey across actual cluster failure.
  Files: new `ui/e2e/cluster-recovery.spec.ts`, new `ui/e2e/helpers/cluster.ts`, `ui/playwright.config.ts`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B3, D2.
  Verify: trigger from the Console, observe a run, fault its owner, reconnect through the supported entry point, and confirm the UI converges on the independently checked durable outcome and retained logs. Exercise both authenticated permissions and event-stream recovery. Observe the fault while the browser is connected; an API-only scenario with a final screenshot is insufficient. Reject duplicate/stale rows and false terminal success. Require available data to remain inspectable after reload.

### Stream E — Performance with correctness

- [x] E1. Make the existing load harness report and exit honestly.
  Files: `test/load/harness.go`, new `test/load/harness_test.go`, `justfile` (load-test recipe only).
  Depends on: none.
  Verify: reject invalid/zero configuration, use an overall deadline, sample metrics concurrently with submission, and return failure when expected runs fail, time out, cannot be triggered, or required samples are missing. Emit versioned machine-readable results with exact expected/observed counts and a usable failure classification. A live successful workload exits zero; deliberate bad-image and unavailable-server workloads exit nonzero. A controlled slow workload proves samples cover early and middle execution, including concurrency=1. Validate the reporter with fixtures as well as the live runs. Preserve useful existing human reports and execute the recipe inside the repository's containerized toolchain.

- [x] E2. Extend the Go load driver with open-loop workloads and lifecycle measurements
  Files: `test/load/harness.go`, `test/load/harness_test.go` (created by E1), new `test/performance/workloads.json`, new `test/performance/load_test.go`.
  Depends on: A1, E1.
  Verify: Extend the containerized Go harness E1 already owns instead of introducing k6 or an unowned toolchain dependency. Support arrival-rate scheduling independent of completion, with offered/dropped/admitted/rejected/completed/backlog counts and bounded overload handling. Cover tiny/realistic tasks, wide/deep DAGs, fan-out, queues, cache hit/miss, API reads, subscribers, and drain. Tag the live driver `//go:build integration`; keep pure scheduling/report tests hermetic. Measure actual lifecycle intervals from observed events, marking unavailable values explicitly. Collect existing metrics and external resource observations, separating statements from rows and timed background work. Reconcile every admitted run; faster admission with backlog growth is not improvement. Production per-task resource telemetry remains owned by right-sizing.

  Note (W4-β): Extended E1's containerized Go driver in place — no k6, no new
  toolchain, `go.mod`/`go.sum` byte-identical to master, no `justfile` edit.
  `mode=open` places arrivals on an absolute clock grid computed from
  `-rate`/`-arrival-window` before the first request, so the schedule never waits
  on a completion or on the previous response. Outcomes are deliberately not
  conflated: `dropped` (the driver's own `client_in_flight_cap` /
  `client_scheduler_lag` / `deadline_before_offer`), `admitted` (DT-ADMIT-01:
  202 **with** a run body carrying a UUID), `queued_or_skipped` (a bare 202 — its
  own outcome), `rejected`, and `transport_uncertain` (DT-QUORUM-01: possibly
  committed, never reported as a rejection). Overload is bounded by a hard
  in-flight cap, a plan fixed and capped at 100000 up front, a bounded
  reconciler pool and a drain deadline. Every admitted run is polled to a
  terminal status; arrivals carrying no run identity are reconciled against the
  server's own run census (`GET /v1/jobs/:id/runs`), and census-discovered runs
  are driven to terminal too. The reporter fails `accounting_mismatch`,
  `unreconciled_admission`, and (for `require-sustained` workloads)
  `backlog_growth` / `backlog_inconclusive`. Lifecycle: four intervals from
  `/v1/events` and four from the public run read's own server timestamps, deduped
  by event identity per DT-EVENT-01 with a durable catch-up after the drain;
  anything unobservable emits `"status":"unavailable"` with a counted reason
  (`cache_hit_no_task_start`, …) — never 0, never an absent key. Report schema
  bumped 1 -> 2 with every schema 1 field still meaningful; adds `arrival_plan`,
  `accounting` (+`server_run_census`), `backlog`, `throughput`, `lifecycle`,
  `resources`, `api_reads`, `subscribers`, `drain`, `cache`, and
  `metric_work_split` (workload-driven vs the timer-driven `lease_renewal`, rows
  kept separate from statements). External resource observation reads the server
  container's stats from OUTSIDE via the runtime API using only the stdlib;
  production per-task telemetry stays with resource right-sizing.
  Commands (all figures below are from the FINAL head, after three adversarial
  review rounds): `just lint` rc=0; `gofmt -l test/load test/performance` clean;
  `just unit-test` rc=0 (108 packages, 0 FAIL, `test/load` 87.8% coverage,
  `-race`);
  `go test -tags=integration -count=1 -timeout=60m -v ./test/performance` inside
  `caesiumcloud/caesium-builder:latest-full`, sharing the netns of the
  `integration-up` server, `ok ... 350.766s` with 10/10 catalog workloads plus
  `TestDriverRejectsUnknownCatalogWorkload` and
  `TestCacheMissWorkloadRepeatsAgainstAWarmServer`;
  `just integration-test` rc=0 (`ok .../test 689.041s`, 276 scenario PASS /
  0 FAIL). Both live stages under one `lane.sh` hold; a second short hold
  re-measured the queue workload after the synchronous-first-queue-read fix.
  Measured: `open-tiny-sustained` offered 20 / dropped 0 / admitted 20 /
  completed_ok 20 / unreconciled 0, verdict `sustained` (peak 1, slope 0.0075);
  `open-overload-bounded` offered 200 with **110 driver drops + 82 bare-202
  skips**, 8 admitted, 0 unreconciled, finished in 13.1 s (no hang);
  `open-queue-concurrency` offered 30 → 9 admitted + 21 queued, and the census
  found 30 runs in the window of which **21 were unaccounted, all 21 settled and
  0 failed**, with the queue verifiably drained to 0;
  `open-cache-hit` reported `read_task_execution` as `status=unavailable`,
  p50/p99 null, `cache_hit_no_task_start` ×6, and its measured workload-driven
  SQL is well below `open-cache-miss`'s for the same 6 arrivals, because
  the warm-up's cold executions are no longer charged to the window;
  `open-api-read-mix` verdict `sustained` (peak 4, slope 0.170) with 4
  subscribers whose coverage cleared the enforced 0.9 floor.
  Backlog verdict — recorded because a checker changed after a failure: round 1
  of `open-tiny-sustained` at 2 runs/s failed `backlog_growth` and that was a
  TRUE POSITIVE (offered 1.86/s vs completed 1.27/s, net +0.59/s matching the
  measured 0.573 slope, backlog 0→15), so the catalog rate was lowered to one
  this shared host sustains rather than the checker being weakened. The rule
  itself was then changed for a separate dimensional defect: the old bound
  `slope ≤ 0.2×rate` is per-second and window-independent, so at 0.5 runs/s it
  was 0.1 backlog/s while ONE unit of integer jitter across a ten-sample
  half-window already fits ≈±0.1/s — the threshold sat inside single-run
  quantisation noise. It is now two arms sharing one window-scaled allowance
  `max(1, 0.2×rate×halfSeconds)`: a level arm (second-half mean minus first-half
  mean) and a trend arm (second-half slope projected across that half, because
  the level arm alone halves late-onset growth). `TestBacklogVerdictSeries`
  keeps the round-1 series as a fixture that must still read `backlog_growing`,
  alongside slow steady growth, growth confined to the final quarter, noisy
  stationary, cold-start ramp and draining series;
  `TestBacklogVerdictArmsAreBothLoadBearing` proves neither arm is decoration.
  Adversarial review, round 1 (PR #552, two P1 + five P2, all verified real):
  `judgeOpenLoop` ignored the census entirely, so a workload could pass with
  possibly-committed work unaccounted — it now fails `census_unavailable`
  whenever identity-less arrivals exist and the census is not `ok`,
  `unreconciled_census_run` when a discovered run never settles, and applies the
  terminal-failure policy to discovered runs. `admitted` counted only UUID-202s
  while `terminal` counted census runs too, which drove the live queue workload
  to 7 − 30 = −23: the populations are now separate (`recordCensusTerminal` no
  longer touches `lg.terminal`), any negative sample fails `accounting_mismatch`,
  and queue depth is polled on the sampling cadence into the growth decision.
  Also fixed: a per-invocation arrival nonce (a repeat inside the cache TTL used
  to be all hits), ONE drain-deadline context bounding the reconcilers, queue
  polling and the census, a deep copy of the run timeline under the observer lock
  (`snapshotRun`), the measurement window fixed to `windowStart + arrivalWindow`
  rather than the last response, and a body-read error after 202 headers filed as
  `transport_uncertain` rather than a bare-202 queue/skip.
  Round 2 (three more P2 from the repo owner, all three verified real): the
  metrics baseline was taken before `warmCache`, so the cache-hit workload's
  deltas and peak rates included its own cold warm-up — a fresh baseline is now
  taken after the warm-up, warm-up-era periodic samples are discarded, and the
  warm-up's own delta is reported separately under `cache.warmup`. End-to-end
  latency paired the driver's `offeredAt` with the server's `completed_at`, so a
  skewed server clock could produce negative latency — the driver's own terminal
  observation (`arrival.reconciledAt`) is used instead, with server timestamps
  kept for the `run_read` lifecycle intervals where both ends are server-side.
  `streamEvents` returns nil on an ordinary EOF, so a subscriber that received
  one frame and disconnected was never counted and `min_subscriber_events` could
  be met with no fan-out for the rest of the window — any stream exit before the
  mix interval closes is now a lost subscriber, the driver reconnects, and an
  enforced `coverage_ratio >= 0.9` check (`subscriber_coverage` failure class,
  plus a `min_subscriber_coverage` catalog expectation) replaces the honour
  system. One defect found while fixing the drain deadline and not named by any
  reviewer: the post-drain residual queue read carried a hard-coded 30 s timeout
  that ran AFTER the deadline expired; it is now bounded by the drain timeout.
  Every fix carries a regression test, and the observer race is covered under
  `-race`.
  Round 3 (1 P1 + 6 P2, all seven verified real against the code before any
  edit). P1: queue observation failed OPEN — `waitForQueueDrain` returned
  silently on a `/queue` read error, `pollQueueDepth` republished its last good
  value, and `judgeOpenLoop` never consulted the queue, so with `/queue` erroring
  and `/runs` healthy the driver could exit 0 with queue rows outstanding. Now:
  failures are propagated, `queueDrainVerified` is set only where a successful
  read returned 0, `judgeOpenLoop` fails `queue_unobserved`/`queue_not_drained`
  when bare-202 arrivals exist, and an in-window sample with no queue observation
  makes the verdict `inconclusive_queue_unobserved`. P2s: a FINAL census after
  the reconcilers join catches runs the server commits after the first census
  (it admits on `context.Background()`, `internal/run/store.go`); offers and the
  residual queue read now run under the same absolute drain deadline (a timed-out
  offer stays `transport_uncertain`); measurement WAITS for `plannedEnd` instead
  of ending at the last scheduled arrival (which stopped sampling up to `1/rate`
  early and could make the drain duration negative); subscriber coverage counts
  from a confirmed SSE response via `streamEventsWithOpen`, so a stall before
  headers no longer reads as full coverage; census membership is a set difference
  against a pre-window run-identity baseline instead of `CreatedAt` vs the driver
  clock, failing explicitly when the baseline is unavailable; and the
  offered/admitted/terminal triple is sampled under one mutex — three independent
  atomic loads could fabricate backlog = -1, which round 1 had made a hard
  `accounting_mismatch`, i.e. a flake that FAILED healthy workloads (the new
  stress test reproduces it in ~10 ms against the old code: `-3 after 43815
  reads`). Two further defects found while fixing these and named by no reviewer:
  a queue that never empties consumed the whole drain deadline and starved the
  census, so the queue wait now takes 3/4 of it; and the first backlog sample
  always preceded the polling goroutine, so a healthy queue read as unobserved
  for the whole window — the first queue read is now synchronous.
  Limits: two workloads now gate on the verdict (`open-tiny-sustained` and
  `open-api-read-mix`); `open-realistic-drain`, `open-wide-dag`, `open-deep-dag`
  and `open-queue-concurrency` report `backlog_growing` on this host and pass
  because they do not set `require-sustained` — each carries a mandatory
  `sustained_rationale` in the catalog saying why, enforced by
  `TestWorkloadCatalogIsValid`. `read_admission_to_run_start` is a true 0 s
  because the server stamps `created_at`/`started_at` in one transaction; the
  event stream was sometimes `degraded` (`unexpected EOF`) with the durable
  catch-up closing the gaps; no 429/503 was observed live, so the `rejected`
  bucket is exercised only hermetically; the round-1 backlog fixture is
  reconstructed from recorded summary statistics, not a raw per-sample capture;
  and the live runner logs the subscriber coverage ratio only on failure, so the
  passing runs prove the gate held without printing the number.
  Nothing was found that required a product-code fix, so no issue was filed.

- [ ] E3. Compare base and candidate across backend and browser workloads.
  Files: new `scripts/compare-performance.py`, new `scripts/test_compare_performance.py`, new `scripts/performance.sh`, new `internal/run/owner_benchmark_test.go`, new `internal/run/recovery_benchmark_test.go`, new `ui/e2e/performance.spec.ts`, `ui/scripts/check-bundle-size.mjs`.
  Depends on: E2, D2, C2.
  Verify: build base and candidate through the same containerized toolchain and run release-equivalent uninstrumented images. Pin workload/data and settings, isolate competing load, interleave repeated runs, separate cold/warm cases, and record image/CLI/toolchain/host provenance. Report benchstat comparisons for targeted hot paths and distributions for system metrics. Browser comparisons include route readiness, action-to-render, long-session memory, and total route assets, so splitting a large chunk cannot evade the budget. All completion/error checks must pass before speed is compared. Test the comparator against known faster/slower/noisy/undersampled results and mismatched environments; it must not accept missing data or call every insignificant difference equivalent.

- [ ] E4. Calibrate and approve enforceable performance budgets.
  Files: new `test/performance/budgets.json`, new `test/performance/baseline.json`, `scripts/compare-performance.py` (created by E3), `scripts/test_compare_performance.py` (created by E3).
  Depends on: E3.
  Verify: satisfy Q2/Q5 with repeated same-code control runs on the chosen runner, sufficient tail samples, and a reviewed workload-specific decision rule. Record absolute SLOs, bounded relative degradation, uncertainty/non-inferiority method, aggregation/multiple-comparison policy, and minimum sample sizes. Pass only when evidence establishes the allowed bound and correctness/SLOs pass; fail material regression; classify inadequate evidence as inconclusive, with a bounded rerun policy that blocks the strict gate if unresolved. Keep both a target-base comparison and a versioned fixed baseline to expose cumulative regression. Intentional budget changes require visible rationale and review; neutral performance is valid and optimization claims must identify tradeoffs. No promotion before calibration evidence exists.

- [x] E5. Add an early SQL-work budget inside the existing integration suite
  Files: new `test/statement_budget_test.go`, new `test/statement_budget_testdata/`.
  Depends on: E1.
  Verify: Run fixed successful workloads through real HTTP/CLI and assert per-category deltas of the existing `caesium_db_statements_total` and `caesium_db_writes_total`. The tagged scenario is discovered by the current `IntegrationTestSuite` and sharding, so it gates with the existing required integration lane before E4. Isolate the server/workload and use bounded, workload-driven categories; timer-driven lease renewals, replay, and unrelated traffic are not assumed deterministic. Establish a repeatable count baseline and explicit justified tolerance on existing CI runners; where a category cannot be isolated, report that limitation rather than imposing a false exact bound. Demonstrate detection of an instrumented extra-statement/lost-batching mutation while completion counts still match. This guards the measured SQL-work classes and needs neither paid runners nor timing SLO decisions Q2/Q5; it does not prove latency or cover uninstrumented queries.
  Merged: [#471](https://github.com/caesium-cloud/caesium/pull/471), merge `4bc19ac98389ccd3fd61382e9f9ec40ad37949a6` (W2-β). Delivered `test/statement_budget_test.go` and `test/statement_budget_testdata/`. `TestStatementBudgetFixedWorkload` is an `IntegrationTestSuite` method, so the existing required Docker integration lane and its sharding discover it without workflow edits: it applies a 2-step sequential `alpine:3.23` HTTP job through the CLI, starts it with `run start` (stdout captured separately from stderr), waits on HTTP until the run and both tasks succeed, and asserts per-category deltas of `caesium_db_statements_total` and `caesium_db_writes_total` against `statement_budget_testdata/baseline.json`. `TestStatementBudgetComparator` replays recorded profiles from `cases.json` so lost batching, extra statements with matching completions, leftover work hiding unbatched inserts, and missing evidence all fail; the labeled scrape rejects prefix-only metric names. Limits: `lease_renewal` and leftover `callback` categories are declared `skip` with reasons rather than exact bounds, `command`/`checkpoint` must stay zero, and the budget guards measured SQL-work classes only. `test/contracts/scenarios.json` was left to G3.

### Stream F — Compatibility, durability, and sustained operation

- [x] F1. Resolve the upgrade and storage qualification matrix.
  Files: `docs/exec-plans/active/distributed-testing.md` (F1 decision record only).
  Depends on: A1.
  Merged: [#474](https://github.com/caesium-cloud/caesium/pull/474), merge `937a9feac5e51b7358331356bb95d8247617bff7`, corrected by [#478](https://github.com/caesium-cloud/caesium/pull/478), merge `da08e7ef81dc0f5a0818e6283eba78bec9f054b1` (W3-ε).
  Verify: add the lifecycle decision record to this plan rather than creating a separate design. Audit existing migrations, release artifacts, chart persistence, membership/replacement procedures, and backup/restore support. Record the exact supported old-to-new paths and allowed rollback/restore behavior, fixture-generation mechanism, retained-run/queue/event cases, and failure assumptions. Distinguish process kill, OS/power loss, lost unflushed writes, and actual disk loss. Deliver the Q4 decisions and exact F2/F3 test procedure; unsupported backup/rollback capabilities become explicit external product prerequisites, not runnable placeholder tests.
  Note: resolved by the F1 decision record under Strategic Decisions; Q4 is closed there. F4 and F2 are dispatchable without further design. The record is a lifecycle policy and procedure, not upgrade certification — no upgrade, rollback, replacement or restore was executed for F1. Five external product prerequisites are recorded (backup/restore, member removal, build/version reporting, stable dqlite node address, ordinal-0 rejoin/re-bootstrap); the stable-address one makes F2's upgrade case blocked-by-prerequisite rather than expected-green, and the ordinal-0 one does the same for its disk-loss case; both are product work this plan does not authorize.
  Corrected after review: PR #478.

- [ ] F2. Qualify persistent cluster upgrades, replacement, and supported recovery paths
  Files: new `test/lifecycle/cluster_test.go`, `test/lifecycle/versions.json` (created by F4), `scripts/lifecycle-tests.sh` (created by F4), new `helm/caesium/ci/test-values-lifecycle.yaml`.
  Depends on: F1, F4, B3.
  Verify: Extend F4's known release fixtures to three persistent members. Upgrade with active/queued work, reconcile history and raw effects, and test mixed versions only where Q4/F1 permits them. Exercise supported rollback or backup restore on isolated volumes, membership replacement, and snapshot catch-up. Use actual release digests and the chart, retain real HTTP/CLI observations, and fail or block unavailable prerequisites explicitly. Single-node migration qualification is already delivered by F4 and does not wait for this item.

- [ ] F3. Add seeded sustained faults and multi-host qualification.
  Files: new `test/robustness/exploratory_test.go`, new `test/robustness/workloads/`, new `test/chaos/`, new `scripts/soak-tests.sh`.
  Depends on: B3, C3, E4, F2.
  Verify: after Q2 provisioning, run bounded seeded fault/workload sequences on separate hosts and repeatable single-host exploratory jobs. Chaos Mesh or the A1-selected equivalent injects Kubernetes partitions/latency/bandwidth plus explicitly supported storage, clock, and resource faults. Combine slow consumers, queue overload, retention, repeated failover, and node replacement. Check safety throughout and progress after healing; verify resource use stabilizes after drain and owned containers/FDs do not leak. Retain actual fault schedules as well as seeds because OS scheduling is not fully reproducible. No process-kill test is labeled power-loss qualification. A short version of every soak scenario must be locally reproducible before scheduled execution.

- [x] F4. Ship single-node previous-release upgrade qualification independently
  Files: new `test/lifecycle/standalone_test.go`, new `test/lifecycle/versions.json`, new `scripts/lifecycle-tests.sh`.
  Depends on: F1.
  Verify: With Q4's supported version pair, start the previous release on an owned persistent volume, create jobs/history/queued work through its public surface, stop it, and start the candidate on that same volume. Assert migrated/publicly readable state and resumed eligible work; also test a failed/unsupported transition according to F1's policy. Use integration-tagged tests and the existing container toolchain. Reuse versioned release artifacts and existing amd64/arm64 CLI checksum smoke evidence; do not require a three-node cluster, B3, or new paid infrastructure. Emit same-SHA qualification evidence and a direct containerized command; G6 later wires this standalone runner into the declared CI/release matrix.
  Note (W4-γ): shipped `scripts/lifecycle-tests.sh` (host controller), `test/lifecycle/standalone_test.go` (integration-tagged runner, compiled explicitly with `-tags=integration` in the builder because the precompiled `./test` binary does not contain subpackage tests) and `test/lifecycle/versions.json` (the version matrix: pinned image plus index/per-arch digests, the published CLI asset checksums, protocol version, shard count, and the expected delta as explicit `{table, column}` pairs). B1's split, one command, leaving the candidate image unbuilt: `CAESIUM_LIFECYCLE_ID=<id> CAESIUM_LIFECYCLE_ARTIFACTS=<dir> CAESIUM_LIFECYCLE_PREV_IMAGE=caesiumcloud/caesium:v0.1.0 CAESIUM_LIFECYCLE_CANDIDATE_IMAGE=caesiumcloud/caesium:$CANDIDATE_SHA bash scripts/lifecycle-tests.sh`. The harness builds the candidate itself from this checkout (`just tag=$CANDIDATE_SHA build-release`) when that image is absent, which is what binds `candidate_sha` to the image actually qualified (the `candidate-image-provenance` case, added below); pre-building it yourself makes the image "supplied", not "built-by-this-run", and blocks the run unless `CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1` is also set. Measured on an arm64 macOS Docker host, one command, end to end in ~1–2 minutes once the images exist: **qualification pass**, 17 pass + 3 recorded-outcome cases, machine-readable record at `$CAESIUM_LIFECYCLE_ARTIFACTS/qualification.json` (exact per-run numbers, image IDs, run UUIDs and timings are in PR #554's evidence section, which is bound to this commit's tree). `caesiumcloud/caesium:v0.1.0` is pulled and its repo digest verified against the pinned index digest `sha256:2e6996f9…73917` before anything runs; `:latest` is refused outright. The serving container's image ID is compared to the built candidate's and `/v1/system/features` must carry `data_assertions_enabled` (assertion 7). Schema: 40 → 42 tables, no v0.1.0 table or column dropped, both pinned table additions (`dataset_holds`, `dataset_metrics`) present and all 12 pinned column additions present on exactly the table named, **zero unpinned additions observed**. Events replay as a SET above an explicit cursor — every pre-upgrade `(sequence, type, task_id, payload)` tuple above it is present, duplicates are legal, and there is no gap-free or high-water-mark check anywhere in the file. Queued work: the `run_queue` row is recorded pending and unclaimed under v0.1.0, survives `docker stop -t 60`, and is dequeued by the candidate into a run carrying that row's own params whose `started_at` is strictly after the previous release's container finished, with a task row, reaching `succeeded`, after which the row is gone from the queue.
  **#536 re-grounding.** F1's "required unsupported-transition case" (restart the candidate at a different `CAESIUM_NODE_ADDRESS`, expect exit 1) is stale: PR #536 (`35bced63`, closes #493) merged after the F1 record and makes the candidate reconcile `info.yaml` and the discovery cache and recover a genuine sole member's raft configuration before going ready. F4 therefore asserts #536's contract on the candidate — it started healthy on the retained volume at `127.0.0.2:9001`, `info.yaml` then recorded node 3297041220608546238 at that address, and every pre-upgrade identity stayed readable — and keeps the **pinned v0.1.0** image at a changed address, on a COPY of the volume, as the deterministic failing transition: observed exit status 1 with `address "127.0.0.1:9001" in info.yaml does not match "127.0.0.2:9001"` and no healthy `/health`. That is why "upgrade first, re-address second" is the only supported order. The F1 record itself is untouched here.
  **Deviations from F1, deliberate and recorded.** (1) The schema assertion is *superset + pinned additions*, not "plus exactly `dataset_metrics` and `dataset_holds`": additions beyond the pinned list are recorded in the evidence rather than failed, because F4 is not in CI until G6 and an "exactly these" check would rot red on the next model addition. The delta was re-measured at this base; F1's column list was three short (`task_runs.log_generation`, `task_runs.log_scrubbed`, `job_runs.timeout_started_at`), all now pinned on their own tables alongside F1's `job_runs.skip_reason` / `task_runs.data_violations` split. (2) The env block is identical on both sides **except** that the run-queue dequeuer is off while v0.1.0 seeds (`CAESIUM_RUN_QUEUE_ENABLED` and `CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED` are ORed at `cmd/start/start.go:427`, so both must be false to stop it). Admission to the queue never consults either flag, so the fixture is created exactly as F1 describes — a trigger admitted to the queue while its predecessor is running — and only the draining moves to the candidate. The alternative, holding the slot open with a live run, would leave the queue blocked forever (see the recorded in-flight case). (3) `caesium job export` does not exist at v0.1.0, so assertion 6 (export → re-lint → `job diff`, all exit 0 with parseable JSON on stdout) is **candidate-side only**; the seed records the applied manifests instead and asserts the subcommand's absence rather than skipping silently.
  **Recorded-outcome cases**, each on its own COPY of the volume so ordering cannot contaminate the main case, reported with no pre-judged expectation and able to fail the qualification only by being unrunnable or unobservable: *rollback* — v0.1.0 started on the candidate-migrated volume, exit 0, healthy, 42 tables, both recorded runs read back with matching status and task-run counts; *shard-count change* — the candidate started on a copy with `CAESIUM_DATABASE_SHARDS=2` against a directory seeded at 1, exit 0, healthy, both recorded runs still readable, i.e. no stranding was observed at this size, which is not evidence that the transition is safe and does not unfreeze the shard count; *retained in-flight run* — a run still executing when v0.1.0 stopped is still `running` on the candidate, which permanently holds a concurrency slot and permanently refuses `job apply` for that job (filed as [#553](https://github.com/caesium-cloud/caesium/issues/553) with a reproducer).
  **What makes the record trustworthy** (round-1 review of #554). The run is a claim about a *complete* expected case set: the controller carries the manifest of all 20 cases, a case with no record is synthesized as `blocked`, every phase's return code is folded into the record's `phases` map and any nonzero one fails it, `cases/`/`observations/` are purged at startup and every surviving case record must carry this invocation's `lifecycle_id`, and the run writes a `result: incomplete` placeholder first so an aborted run can never leave a passing file behind. `candidate_sha` is bound externally (there is still no product version surface, F1 prerequisite 3): the script records whether *it* built the image from a clean checkout at that SHA, refuses to build from a dirty tree, and marks a supplied or pre-existing image `supplied/unverified` — which blocks the qualification unless `CAESIUM_LIFECYCLE_ALLOW_UNVERIFIED_IMAGE=1` is set and recorded. Recorded-outcome cases are blocked when the observation itself failed (`status: unknown`, a sentinel exit code, an uncaptured log), per F1 ruling 5 that “cannot run or cannot observe” is inconclusive, never a pass; a runner self-check phase exercises that guard on every run. Round-4: a RUNNING container whose HTTP probe recorded only transport/decode failures is blocked (an HTTP 404 is a product answer; an EXITED container remains a recorded outcome); the isolation probe requires a positive control and treats docker 125/127 as blocked, not as isolation.
  **Limits.** One node, one shard, arm64, local execution mode. This pair is purely additive, so it does **not** exercise `MigrateTaskRunUniquePartitionIndex`; nothing here qualifies a cluster upgrade, a mixed-version window, restore-from-snapshot or rollback as supported. The published amd64/arm64 CLI asset checksums are carried in `versions.json` and reproduced in the record, but the bare-host smoke (`scripts/ci-cli-smoke.sh`) is not re-run by this lane. The server ran as `--user 10001:10001 --group-add 0` (the docker socket's in-container gid) identically on both sides, on a named volume and never a bind mount, with no fixed host port and a unique `LIFECYCLE_ID` on every container, volume and network. Fixture task containers are matched by a per-invocation ownership token (lifecycle id plus a nonce, delimiter-anchored, never a substring of the id) and swept only after this invocation has actually claimed its names, so a run refused on an active id cannot kill the live containers of the run that owns it. Two instances were proven to coexist (both healthy at once on separate networks and volumes, neither reachable from the other). The runner is deliberately NOT wired into CI or `ci-ok` — G6 owns that, and `test/contracts/scenarios.json` is untouched.

### Stream G — Diagnostics, coverage, and enforceable CI

- [x] G1. Preserve first-attempt failures and discover all Python validator tests
  Files: `ui/playwright.config.ts`, `.github/workflows/ci.yml`, `scripts/test_ci.py`.
  Depends on: none.
  Verify: Make browser retries retain diagnostic evidence without erasing initial failure; a temporary controlled fail-once scenario must fail and upload its trace. Upload reports/screenshots/video/server logs including setup failure, and retain structured outcomes. In the same PR widen ci-config discovery from `test_ci.py` to `test_*.py` and assert that command in `scripts/test_ci.py`; a temporary extra matching test that fails must fail ci-config. This ensures A2's `test_test_evidence.py`, E3's `test_compare_performance.py`, and G2's `test_coverage.py` run immediately when introduced. Keep upload errors visible without masking the original failure; no automatic quarantine, reduced floors, or erased first attempts.

- [x] G2. Collect actual CLI/server integration coverage with provenance.
  Files: new `build/Dockerfile.coverage`, new `scripts/integration-coverage.sh`, new `scripts/check-coverage.py`, new `scripts/test_coverage.py`.
  Depends on: A2, G1.
  Verify: use Go coverage-instrumented binaries built in containers, explicit package selection and `GOCOVERDIR`, and merge compatible profiles from CLI, server, and live browser journeys. Collect on graceful shutdown and provide explicit flushing where required; killed-process or missing profiles are incomplete evidence, not zero coverage or success. Label profile provenance, separate unit/integration/browser contributions, and report uncovered changed paths and critical contract gaps. Set package/diff ratchets after measuring a baseline instead of requiring a vanity global percentage. Demonstrate coverage of a real request-to-write-to-read path. Keep coverage/fault instrumentation out of performance artifacts and audit the separate `reagents/go.mod` scope when relevant.
  Note (W6 follow-up): a fresh coverage image built from reviewed #558 head `7dcef7f07d85cf460a26239e11bb30889ac9221a` carried that revision label. `bash scripts/integration-coverage.sh` exited 0 with `verdict: pass` and complete CLI, server and merged integration profiles; the instrumented apply→export request/write/read path was covered (7.6% integration coverage). The local report is `/tmp/caesium-w6-g2.ax3fCH/report.json`. No browser profile was supplied, so that contribution remains explicitly incomplete and no browser-coverage claim is made. The collector can merge a labelled browser profile when supplied; this run proves CLI/server coverage and provenance only.

- [x] G3. Wire the first persistent three-node lane and its exact scenario selectors
  Files: `.github/workflows/ci.yml`, `.github/actions/run-integration/action.yml`, `build/ci.docker-bake.hcl`, `build/ci.docker-bake-cache.hcl`, `justfile`, `scripts/collect-evidence.py` (new), `scripts/test_collect_evidence.py` (new), `scripts/test_ci.py`, `scripts/test_test_evidence.py`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B1, E5, G1.
  Merged: [#477](https://github.com/caesium-cloud/caesium/pull/477), merge `2ef933e13d96f3e26b8f188fa19481d90257692f` (W3-α).
  Verify: Add the B1 runner/image/recipe and artifact consumers using the existing kind and Helm infrastructure. Register the owner-crash scenario and E5's existing-suite budget with exact topology, candidate digest, and scenario identity. A new `test/robustness` binary must be explicitly built with integration tags; the existing `./test` precompiled binary cannot contain its subpackage tests. Verify selectors for code, fixtures, chart, dependency, image/build, and workflow changes, and a missing artifact/scenario failure. Preserve complete existing engine/architecture suites. Ship an executable lane and selector tests; promotion/settings/merge policy are separate G5/G7 work, so G3 does not wait for full B3, E4, F2, or G2.
  Note (W3-alpha): `just early-evidence` runs E5's `TestIntegrationTestSuite/TestStatementBudgetFixedWorkload` against the existing `integration-up` server and B1's `TestOwnerCrash` on an owned kind cluster (1 control-plane + 3 workers, three persistent Helm StatefulSet replicas), then `scripts/collect-evidence.py` reduces the retained artifacts to one report that `scripts/check-test-evidence.py --require early --strict` validates. Every reported field is read back from a real artifact (pod environments, kind config, bound PVC claims, recorder records, `docker inspect`, a retained `/metrics` scrape); a missing record or an unreadable `ctr` listing is reported inconclusive, never pass. The new `early-evidence` job uses the existing kind/Helm/`run-integration` infrastructure and is deliberately **not** in `ci-ok` or any required check — promotion is G5. The three rows are `proven` with `gates: ["early"]`; every other row stays `absent` with no gate. The Docker lane runs the `-test` image variant, so `e5-sql-work-budget` records `observed_server_image_id` and sets `require_candidate_digest: false` rather than claiming the release digest.

- [ ] G4. Schedule broader qualification and enforce release evidence
  Files: `.github/workflows/ci.yml`, new `.github/workflows/testing-qualification.yml`, `scripts/test_ci.py`.
  Depends on: F3, G6.
  Verify: Schedule longer seed/fuzz campaigns, browsers, multi-host faults, and soaks within Q1/Q2 limits, with cleanup, retained evidence, and triage ownership. Retain current native CLI checksum/smoke gates. Require supported release qualification in the actual publish chain or verified same-SHA evidence; demonstrate a failed qualification blocks publication. Run scheduled/manual campaigns and record one-command reproduction and explicit limits for N-1's runbook sync. Nightly results are broader evidence, not retroactive proof for every PR.

- [x] G5. Promote the minimum credible gate into the fail-closed aggregate
  Files: `.github/workflows/ci.yml`, `scripts/ci-ok.py`, `scripts/test_ci.py`.
  Depends on: G3.
  Merged: [#481](https://github.com/caesium-cloud/caesium/pull/481), merge `5d4bf73e0d194301090e147a17daf5c883f7e0bc` (W3-ζ).
  Verify: On the existing hosted runner, require B1's demonstrated three-member owner-crash lane alongside E5's SQL-work budget and G1's honest browser outcomes. Record measured added duration and repeated stability without waiting for controlled-runner timing calibration or the full exploratory suite. Audit current required checks/rulesets under Q6 and verify that ci-ok is actually enforced; change settings only within execution authorization. Test failed/cancelled/skipped/missing new dependencies and require exact scenario/fault evidence. Existing optional mode/engine lanes retain their honest status until explicitly promoted. This milestone provides narrow documented confidence, not full partition/upgrade/performance equivalence.
  Note (W3-ζ): `ci-ok` now requires `early-evidence` (B1's three-member owner-crash lane plus E5's SQL-work budget) alongside G1's `ui-e2e`/`ui-e2e-auth`, and `helm-lint`, the lane's unconditional producer, is promoted with it so a chart failure reports as itself rather than as a skipped consumer. A green job result is not accepted as evidence: `ci-ok` downloads the report the lane uploaded and `scripts/ci-ok.py` requires it to exist, to bind to this run's `github.sha`, to report every `early`-gated manifest row `pass` with its required fault-activation observations, and to pass `check-test-evidence.py --require early --strict`; absent, foreign or hollow evidence fails closed, as do failed/cancelled/unexpectedly-skipped/missing dependencies. Measured on hosted runners: `early-evidence` took 6m19s, 5m57s, 6m18s, 7m15s and 7m32s across five green hosted runs ([34861385345](https://github.com/caesium-cloud/caesium/actions/runs/34861385345), [34863643792](https://github.com/caesium-cloud/caesium/actions/runs/34863643792), both attempts of [34865488758](https://github.com/caesium-cloud/caesium/actions/runs/34865488758) and [34868499191](https://github.com/caesium-cloud/caesium/actions/runs/34868499191) attempt 2), with no lane flake. It moved `ci-ok` behind the lane — on W3-ζ attempt 1 `ci-ok` finished 16:09:20 instead of ~16:07:13, about +2m — while total workflow wall clock grew 0s on attempt 1 (13m12s; `helm-integration-test` was still the critical path) and +25s on attempt 2, where `ci-ok` became the critical path. The promotion also fail-closed for real on 34868499191 attempt 1, where an unrelated transient `images`/`bake-images` failure skipped the lane: `ci-ok failed: images=failure, … early-evidence=skipped, early-evidence evidence: .tmp/evidence/evidence.json is absent; the lane produced no evidence`. **Q6 audit finding: `ci-ok` is not enforced at merge.** Master's protection lists eight required contexts (lint, unit-test, unit-test-arm64, ui-test, ui-e2e, ui-e2e-auth, build-and-integration-test, build-and-integration-test-agent-auth), `strict: false`, `enforce_admins: false`, one CODEOWNER review, and there are no rulesets; `ci-ok` is absent. It is enforced only as a dependency of `publish` (so this promotion does block `v*` tag publication), and `scripts/ci-ok.py` is reached at merge only through the two required wrapper contexts over `changes images integration`. Making this promotion merge-blocking needs exactly one repository-settings change — add `ci-ok` to `required_status_checks.contexts` on master — which is outside execution authorization and was NOT made; it is surfaced to the CODEOWNER. Existing optional lanes (helm-integration-test, podman-integration-test, integration-extra, integration-arm64) keep their unpromoted status, asserted by a test. This is narrow documented confidence: one owner-crash fault and one SQL-work budget, not partition, upgrade or performance equivalence.

- [ ] G6. Wire and qualify the remaining complete system suites
  Files: `.github/workflows/ci.yml`, `.github/actions/run-integration/action.yml`, `build/ci.docker-bake.hcl`, `build/Dockerfile.integration`, `justfile`, `scripts/ci-ok.py`, `scripts/test_ci.py`, `scripts/integration-test.sh`, `test/shard_test.go`, `test/contracts/scenarios.json` (created by A2).
  Depends on: B3, C3, D3, E4, F2, G2, G7.
  Verify: Extend G3/G5's functioning lane pattern to generated tests, full faults/journeys, standalone and cluster lifecycle runners, real coverage, and calibrated performance. Explicitly compile/run every new integration subpackage and validate fixture/config/path selection. Under Q1/Q2/Q5, measure capacity and audit promotion of the existing distributed/owner-memory/Podman/Helm/arm64 integration lanes; retain all real-surface coverage and document unpromoted limits. Fail closed on missing artifacts, unexpected skips, unexecuted scenarios, or inconclusive performance. Do not bundle new merge-policy machinery here; use G7's verified candidate identity.

- [x] G7. Verify the merge-candidate policy independently of suite expansion
  Files: `.github/workflows/ci.yml`, `scripts/ci-ok.py`, `scripts/test_ci.py`.
  Depends on: G5.
  Verify: Resolve Q6's up-to-date-base versus merge-queue choice from current repository settings and test the actual prospective merge commit. If activating a queue, wire merge_group and prove producers, selectors, and required context names on that event; otherwise verify the chosen base-freshness enforcement. Bind evidence to the tested candidate SHA and test stale/missing candidate refusal. Record permissions/settings evidence and measured implications for Q1; no paid performance or multi-host infrastructure is a prerequisite.
  Note (W4-ε): **Settings evidence, read twice (2026-09-16 execution-base and reconfirmed 2026-09-17), unchanged between reads and NOT modified by this PR.** `gh api repos/caesium-cloud/caesium/branches/master/protection`: required contexts `lint, unit-test, unit-test-arm64, ui-test, ui-e2e, ui-e2e-auth, build-and-integration-test, build-and-integration-test-agent-auth`; `strict: false`; `enforce_admins: false`; 1 approving review, `require_code_owner_reviews: true`. `gh api repos/caesium-cloud/caesium/rulesets` → `[]` (no merge queue exists). Repo: `allow_auto_merge: false`, `allow_update_branch: false`, squash/merge/rebase all allowed, `delete_branch_on_merge: false`. `ci-ok` remains absent from required contexts (G5's finding, unresolved). No repository setting was changed by this PR; only read-only `gh api` GETs were issued.

  Q6 was answerable only by making the workflow correct under **both** candidate policies at once, since the choice itself belongs to the CODEOWNER: (1) audited that every one of the 26 `actions/checkout@v6` steps across every job omits `ref:`, so each evidence-producing/required-context job tests exactly the commit GitHub hands it — on `pull_request` that is the prospective merge commit (`refs/pull/N/merge`); (2) wired `merge_group` (`types: [checks_requested]`) so a queue is activatable by a settings change alone, with no further workflow edit; (3) extended the evidence binding G5 introduced into a full candidate-identity check; (4) added a pure base-freshness check invoked only where it can mean something.

  **What is wired.** `scripts/ci-ok.py` gained `verify_candidate_identity` and `verify_base_freshness` (pure, unit-tested directly) plus `--event-name`/`--base-sha`/`--head-sha`/`--current-base-sha`; the `ci-ok` job now passes this run's real `github.event_name`, the base/head SHAs from `github.event.pull_request.*` or `github.event.merge_group.*`, and a `--current-base-sha` read fresh via a new `git ls-remote origin refs/heads/master` step (`continue-on-error: true`, so a transient failure lands as an empty string rather than hard-failing the job — `ci-ok.py`'s own logic decides what an empty result means). These checks run only when `--candidate-sha` is present, so the two legacy `build-and-integration-test*` wrapper jobs (which never pass it) are untouched. `.github/workflows/ci.yml`'s `changes` job now filters real diffs on `merge_group` too (`base: github.event.merge_group.base_sha`, `fetch-depth: 0`, versus `pull_request`'s unchanged API-based auto-detect with `base: ''`), and its `set` step was rewritten so a missing or non-boolean filter output **fails the job** instead of defaulting to `false` (which would have silently read as "everything skipped" for a required producer) — `push` keeps its unconditional everything-true fallback, now explicitly gated to exclude `merge_group`. The `concurrency` group now keys `pull_request` and `merge_group` runs into disjoint namespaces (`pr-<number>` vs `mergegroup-<head_sha>`) so a queued run can never be cancelled by, or share a slot with, a PR run; `cancel-in-progress` was already `pull_request`-only and needed no change. `IMAGE_TAG` (keys off `ref_type`) and `publish` (`startsWith(github.ref, 'refs/tags/v')`) were already event-agnostic/tag-only and are asserted unchanged.

  **What each refusal test proves.** Pure-function tests (`CandidateIdentityUnitTests`) cover `verify_candidate_identity`/`verify_base_freshness` directly: missing event-name, missing candidate-sha, an unrecognized event, missing base/head on `pull_request`/`merge_group` (push needs neither), and every freshness combination (fresh/stale/missing-base/missing-current) on both `pull_request` (never fails, always informational) and `merge_group` (fails closed on all three). `CandidateIdentityGateTests` re-proves the same behavior through the actual CLI subprocess, plus that a caller passing no `--candidate-sha` (the legacy wrapper jobs) skips identity entirely. `MergeGroupWorkflowWiringTests` is the static proof for the workflow itself: the trigger and concurrency wiring; that every job-level `if:` and all but three step-level `if:`s are free of `event_name` (`test_event_name_appears_only_in_audited_locations`, enumerating the exact three); the `ref:`-override audit across all 26 checkouts; and a subprocess-level test of the rewritten `set` step proving `push` still forces everything true, `pull_request`/`merge_group` both compute real selectors from valid filter output, and both fail closed (nonzero exit, "not true/false" on stderr) when a filter output is unset or malformed rather than silently reading as skip.

  **Review-round hardening (same PR, squashed into commit `133915a9`).** An adversarial review of the first version of this PR (four inline comments, cross-checked against the actual code before accepting each finding, replied to in-thread), plus a real `ci-config` failure on that PR's own hosted run, together found five real defects, all fixed with new tests rather than argued away: (1) on `merge_group` the gate did not check that the tested `--candidate-sha` equals `--head-sha`, even though GitHub's contract is that they are identical for that event -- added to `verify_candidate_identity`, with a pure test and a subprocess test proving the mismatch refuses and the match passes. (2) `if args.candidate_sha:` gated identity/freshness on truthiness, so an explicitly empty `--candidate-sha ''` (e.g. a broken template expression, as opposed to the flag being omitted, which is the only thing that legitimately exempts the two legacy wrapper jobs) would have silently skipped validation instead of refusing it -- fixed by checking `is not None` across all four identity flags, so any one of them being explicitly passed (even empty) turns validation on; proven with a subprocess regression that supplies `--candidate-sha ''` with `early-evidence` deselected, isolating that the refusal comes from identity, not evidence. (3) On `pull_request`, `github.event.pull_request.base.sha` was trusted at face value as "the tested base," but the tested commit is actually GitHub's synthetic `refs/pull/N/merge`, whose real base is its first git parent and can differ from the payload field if the merge ref was regenerated after the payload was recorded. Fixed with a new pure `verify_pull_request_candidate_parents` (unit-tested for the 2-parent-match, wrong-count, second-parent-mismatch, and first-parent-disagreement-is-reported-not-refused cases) fed by a new `git cat-file -p "${{ github.sha }}"` workflow step -- confirmed empirically that this reads both parent SHAs correctly even at the job's existing default fetch-depth 1 (a shallow fetch omits the parent *commits*, never the parent SHA lines recorded in the fetched commit's own object header; verified directly against this PR's own `refs/pull/550/merge`, whose two parents are exactly `88d7edcf` (master's tip at the time) and this branch's own head). (4) `fetch-depth: ${{ github.event_name == 'merge_group' && 0 || 1 }}` always evaluated to `1`: GitHub Actions expressions use JS-like truthiness where the *number* `0` is falsy, so a true `cond && 0` immediately falls through the `|| 1` -- the merge_group branch was unreachable. Fixed by quoting the operands (`'0'`/`'1'`, non-empty strings are truthy); the regression test evaluates the branch semantics with a small truthiness-mimicking helper rather than re-matching the same buggy text. Separately, the PR's own `ci-config` run caught a real bug in the *test harness* (not the workflow): `test_set_step_fails_closed_instead_of_treating_a_bad_filter_as_skip`'s simulated "missing filter output" case built its subprocess environment by inheriting `os.environ`, which on a GitHub-hosted runner already carries `CI=true` ambiently for every job -- so the "unset CI output" case silently observed `CI=true` leaking in from the runner itself rather than from the (correctly empty) `outputs` dict, and the fail-closed assertion never actually fired. Passed locally (no ambient `CI` var on this host) and failed on the runner for exactly that reason. Fixed by having every call build all five filter-output env vars explicitly (defaulting to `""`, matching what `${{ steps.filter.outputs.X }}` actually resolves to for a nonexistent output) instead of relying on omission from a dict layered over an inherited environment; also switched the test's invocation from `bash -c "<script>"` to a script *file* run with `bash --noprofile --norc -eo pipefail <file>`, matching `ci.yml`'s actual `defaults.run.shell: bash` invocation rather than merely its text. Reproduced the original failure and confirmed the fix under real `bash 5` (`docker run bash:5`) with ambient `CI=true` injected, and re-ran the entire `scripts/` suite inside a `python:3.12-slim` Linux container with `CI=true`/`GITHUB_ACTIONS=true` set (`test_ci.py`: 89/89 pass on both host and Linux; the only other module's failures, 23 in `test_caesium_cli_wrapper.py`, are that minimal container lacking `docker`/`kubectl` binaries entirely -- a property of the ad hoc verification container, not of this change, and that file is untouched by this diff).

  **Live-queue limitation, stated plainly.** No ruleset exists on this repository (`rulesets` → `[]`, confirmed above), so `merge_group` has never fired and cannot be exercised live in this PR. Every `merge_group`-specific behavior — the real diff range, the disjoint concurrency group, identity/freshness on that event — is proven only statically, by `actionlint .github/workflows/ci.yml` (clean) and the structural/subprocess tests above; there is no substitute for a live queued run once a ruleset exists. The `pull_request` path is the only one with live evidence: this PR's own CI run exercises the shared `set`-step rewrite and the `base: ''`/`fetch-depth: 1` branch of the new ternaries (i.e. proves passing `base: ''` reproduces the pre-G7 unset-input behavior, since `changes` still correctly selected `ci` on this PR).

  **Measured Q1 implications.** Last 10 green `pull_request` CI runs (`gh run list --workflow CI --event pull_request --json databaseId,createdAt,updatedAt,conclusion`, 2026-09-17): wall-clock 0.60–35.57 min, mean 15.9 min (wide spread reflects runner queueing and PR scope, not workflow cost). Per-job timing for one representative run ([35157375234](https://github.com/caesium-cloud/caesium/actions/runs/35157375234), `gh run view 35157375234 --json jobs`) shows the required-to-merge critical path (`changes` start to `ci-ok` completion) at ~13 min, full matrix including optional lanes (`helm-integration-test` is the tail) at ~17.3 min — consistent with G5's ~13m12s figure. **`strict: true`** costs one extra full ~13-minute required-set rerun per PR *each time* master advances during that PR's review. Once that requirement is on, GitHub's own up-to-date policy permits the update flow (permissions and conflicts still apply). `allow_update_branch: true` is an optional convenience for *non-strict* protection: it lets a behind head be updated even when being current is not required. Today's `allow_update_branch: false` therefore does not block flipping `strict: true`. **A merge queue** costs exactly one extra ~13-minute run per merged PR (or per batch, if batching), incurred once at merge time regardless of how many times master moved during review, and needs no `allow_update_branch` change. This repository's actual merge pattern is not "one PR at a time": this very wave has 5 concurrent W4 streams open against `master` right now (per the orchestrator's ownership table), landing by sequential admin-merge squashes — exactly the shape where `strict: true`'s per-staleness-event cost multiplies across open siblings while a queue's per-merge cost stays bounded and predictable.

  **Recommendation.** Given the observed concurrent-PR pattern, a merge queue is the better fit for this repository's merge pattern *in the steady state*, despite being the heavier settings lift (a ruleset, not a flag). Concretely, for the CODEOWNER: both options first need G5's already-surfaced prerequisite — add `ci-ok` to `required_status_checks.contexts` (command in `docs/ci.md` §1). Then either (a) flip `strict: true` on the existing `required_status_checks` PATCH (cheapest, one field; `allow_update_branch: true` is not a prerequisite), or (b) create a branch ruleset with a `merge_queue` rule targeting `master` and `required_status_checks` naming `ci-ok` (plus, during transition, the eight legacy contexts) as the queue's own merge condition — the heavier lift, but the one whose cost does not grow with how many PRs are open at once. If the team wants an incremental step, `strict: true` alone is defensible short-term since sequential admin-merges mean any one PR is usually only invalidated once; but the wave-orchestration pattern in this repo's own memory notes (`feedback_wave_merge_gate.md`) suggests the queue is where this ends up regardless, and this PR's `merge_group` wiring means adopting it later needs no further workflow change — only the ruleset.

  **Preserved from G5.** Fail-closed behavior on failed/cancelled/unexpectedly-skipped/missing dependencies is untouched (`GateTests`, `EarlyEvidencePromotionTests` all still pass unmodified); the evidence-report requirements (candidate-SHA binding, per-scenario pass + fault-activation) are untouched. `helm-pod-replacement-test` (added by PR #536, after G5) was already wired with the same unpromoted shape as its siblings (not in `ci-ok`'s `needs`, not in `SELECTORS`) — `test_existing_optional_lanes_keep_their_current_status` now covers it explicitly so that status stays asserted rather than merely true by omission.

  **Verification (final, post-review-round).** `python3 -m unittest discover -s scripts -p 'test_*.py' -v`: 194 tests OK on the host (89 in `test_ci.py`, 45 net-new vs `master`). Re-ran `test_ci.py` (89/89) and the full `scripts/` discovery inside a `python:3.12-slim` Linux container with `CI=true`/`GITHUB_ACTIONS=true` set, matching the hosted runner's ambient environment exactly — the only failures there are 23 in the untouched `test_caesium_cli_wrapper.py`, caused by that minimal container lacking `docker`/`kubectl` binaries, not by this change. `actionlint .github/workflows/ci.yml`: clean. `git diff --check` against `master`: clean. `just lint` + `just unit-test` ran together in one host lane-lock hold (`.tmp/lane.sh`, disk-pressure addendum: `docker run --rm alpine:3.23 df -h /` showed 23.8G free beforehand, well above the ~5GB floor): `just lint` 0 issues, `just unit-test` exit 0. No server was started — this item touches only workflow/Python files, so `just integration-test` was not run (and would prove nothing about `merge_group`, which no local run can trigger). This PR's own `pull_request` CI runs are the live evidence for the `pull_request` path: the first hosted run ([35180469342](https://github.com/caesium-cloud/caesium/actions/runs/35180469342)) caught the real `ci-config` regression described above; the second, on the squashed commit `133915a9` ([35221628892](https://github.com/caesium-cloud/caesium/actions/runs/35221628892)), is this fix's own confirmation run: `ci-config` and `ci-ok` both `success` (the `ci-ok` log shows `ci-ok candidate identity: event='pull_request' sha='cfdee007...' base='88d7edcf...' head='133915a9...'` followed by `base freshness: tested base 88d7edcf... matches current master tip` and `ci-ok passed` — the candidate-parents step read a first parent that agreed with the payload base, so no disagreement line was needed). The run's overall GitHub conclusion reads `failure` only because `helm-integration-test (3)` — one of the four lanes §2 documents as historically flaky and deliberately not required-to-merge, untouched by this diff, failing on an unrelated `digest-mismatch` in the Kubernetes cache-identity check — went red; every required-to-merge job, including `ci-ok`, is green.

## Sequencing & Dependencies

### First wave, early milestone, and ordering

**First dependency-ready candidates: A1, E1, G1.** Their Files lists are
pairwise disjoint. A1 resolves the first conformance scenario, E1 fixes existing
measurement behavior, and G1 makes browser results and Python discovery honest.
None requires controlled performance runners or a fully specified failure model.
Do not launch implementation merely because this plan has been drafted.

**Minimum credible gate target: the end of wave two.** After the foundation
wave, B1 ships a three-replica persistent kind/Helm pod-kill regression, E5 ships
a repeatable SQL-work budget in the existing integration suite, and A2 supplies
scenario/evidence validation. G3 then wires the new runner; G5 promotes the
validated early lane into the enforced aggregate. G3 and G5 are sequential
merge steps within that wave, not extra standalone waves or work that waits
for B2/B3/C3/E4/F2/G2. N-1 closes both waves. Missing correctness or setting
proof blocks this milestone; the target is not a promise to fit unmeasured CI
time. This gate proves a narrow owner-crash/SQL-work/browser-outcome envelope,
not all partitions, side effects, latency bounds, or upgrades.

C1 can follow A1 independently of B1; C2 follows C1. B2 needs B1/C1 and the
explicit hook handoff; B3 needs B2 plus G3 to serialize scenario-manifest edits.
D1 follows A1; D2 follows A1/G1; D3 follows B3/D2. F4 follows F1 without B3,
while F2 adds the cluster cases after F4/B3. E2/E3/E4 form the later timing
comparison path; E5 does not wait for it. G7 follows G5 and resolves candidate
identity. G6 joins the complete fault/model/journey/lifecycle/coverage and
calibrated performance capabilities after G7. F3/G4 add broader qualification.

Item dependency fields are authoritative. Existing IDs retain their identity:
G3 now owns initial lane wiring, G5 owns early promotion, G7 owns candidate
policy, and G6 owns the later matrix expansion carved out of the original G3.
A prerequisite is ready only after its deliverable is merged and its applicable
decisions are resolved. Stacked work needs explicit execution scope.

### One writer per overlapping surface

| Surface | Owner and serialized order |
| --- | --- |
| Plan decision records/dashboard | A1 then F1 own their assigned Strategic Decisions text; the native orchestrator owns Progress and reconciles item evidence. |
| Root `go.mod`/`go.sum` | C1 adds only its used model libraries. B1 reuses chart/CLI infrastructure. A later proxy/container dependency requires an explicit owner and toolchain-generated sums. |
| Runtime fault seam | B2 owns only A1's enumerated event-dispatch boundary after EX-HOOKS. No blanket ownership of run store, owner completion, dispatch handlers, or worker runtime. Any clock refactor requires a separately scoped plan amendment and sibling handoff. |
| Scenario manifest | A2 -> G3 -> B3 -> D3 -> G6. Other streams supply completed scenario entries to the next assigned writer. |
| `justfile` | E1 owns only the load recipe; G3 owns first-cluster recipes; G6 owns later lane recipes. All scripts execute useful work before recipes are added. |
| Workflow and `scripts/test_ci.py` | G1 -> G3 -> G5 -> G7 -> G6 -> G4. A2/E3/G2 add separate Python test files that G1's wildcard discovers; they do not edit the workflow. |
| `scripts/ci-ok.py` | G5 -> G7 -> G6; each preserves fail-closed behavior on the current required set. |
| `docs/ci.md`, documentation index/roadmap links | Reserved exclusively to N-1 at each wave close. No implementation stream lists the runbook in its Files. |
| UI config/dependencies/shared fixtures | G1 config -> D2 -> D3. E3 uses separate browser performance tests after D2. |
| Load implementation/results | E1 -> E2 -> E3 -> E4. E5 uses separate integration fixtures and measured counters after E1. |
| Cluster harness/recorder | B1 -> B2 -> B3; F2/F3 and D3 consume the completed harness and own separate scenario files. |
| Lifecycle runner/version matrix | F4 creates -> F2 extends -> F3 consumes. |

### N-1 — Wave-close runbook synchronization (coordination checkpoint)

N-1 is a step of each wave's native orchestrator, not a fictitious agent role,
an independently dispatchable placeholder, or an additional implementation
stream. Its sole file scope is `docs/ci.md`, relevant `docs/README.md` and
`docs/roadmap.md` links, and this plan's Progress. Once the wave's implementation
PRs are merged, the orchestrator authors one docs-only sync PR from that merged
base, recording the actual commands, outcomes, guarantees, limits, and evidence.
Land it after those PRs and before closing the wave or dispatching the next wave.
Record its PR and merge SHA as `W<n>/N-1` in Progress.

Until then, the current runbook continues to describe the prior completed wave;
implementation PRs provide their executable commands and evidence in their own
bodies/assigned notes. Do not claim docs/Progress agree with newly merged work
before N-1 completes. Required product behavior docs for an actual bug fix stay
with that product PR; N-1 consolidates shared testing/CI operations only.

### Sibling ownership and handoff conditions

**EX-HOOKS (B2 dispatch prerequisite):** A1 records the precise event-publication
hook and the owner of `internal/event/bus_dispatch.go` at the execution base.
Before allocating B2's instrumented case, verify the merged SHA/PR of any
intersecting [closed-loop arc](closed-loop-arc.md) work and record exclusive
ownership for the B2 wave, or record that no sibling has an active claim after
checking its current plan/PR. The prior draft's speculative run-store/worker
edits are removed. An unmerged sibling checkbox or invented future wave number
is not a valid handoff. A1 must amend the plan before any additional product
hook or clock-seam file is allowed.

- [Resource right-sizing](resource-right-sizing.md) owns production per-task
  resource capture, OOM semantics, resource fields, engine adapters, and its
  recommendation/UI work. E2 uses existing metrics and external observations;
  it does not create those fields or a second telemetry pipeline. Adding its
  new product scenarios requires a merged sibling SHA and verified public
  schema/engine behavior. This is an optional extension, not a prerequisite
  for E2's baseline workloads.
- [Backtesting](backtesting.md) owns replay-over-history and proposal verification
  as product features. This plan's base/candidate system benchmark does not
  implement that feature or execute production data and credentials.
- [Window scheduling](window-scheduling.md) owns window/deadline policy and
  predictor semantics. Test existing queue/priority behavior without promising
  starvation freedom or new deadline behavior. New window scenarios await that
  plan's merged contracts.
- [Data circuit breaker](data-circuit-breaker.md) owns its operator UI and
  incident/freshness integration. Refresh PR #442 and subsequent wave status
  at the execution base; reuse merged scenarios rather than recreating them.
  B2's event hook and any sibling event/metrics, UI shared-file, or CI writer
  must have a recorded serialized merge order. A local checked box is not sufficient handoff evidence.

### Documentation consolidation

This initiative has **one execution plan: this file**, and uses the existing
**`docs/ci.md` runbook** for shipped testing commands, guarantees, troubleshooting,
and enforcement. A1/F1 keep design decisions here. No per-stream testing plans,
design memos, Console/developer guides, or performance/coverage/robustness
Markdown documents are introduced. N-1 alone synchronizes the runbook after
each wave; this deliberately replaces the earlier contradictory promise that
every parallel implementation PR would also edit the same runbook.

Versioned scenarios, budgets, release matrices, and the machine-readable fixed
baseline live with their tests. Raw reports/profiles/logs are CI artifacts,
not dated Markdown files. `docs/load-testing-history.md` remains an explicitly
historical record, not a second current testing guide. Distinct product plans
retain their unshipped contracts instead of duplicating them here. N-1 folds
actual duplicated testing instructions into the runbook, updates inbound links,
and removes superseded copies while preserving unique historical evidence.

### Deferred or optional scope

Full deterministic simulation of CGO/dqlite, a bespoke Docker cluster before
reuse of the existing Helm path, a standalone Jepsen port, formal
TLA+/TLC specifications, testscript migration, unsupported native platforms,
k6 (the Go harness supplies open-loop traffic), a production clock-seam refactor
unless A1 justifies and scopes it, and new production telemetry/backup
capabilities are deferred. A1 can recommend a separately scoped follow-up with
a concrete design deliverable. Do not add
empty runnable items or count these as completed coverage. F3/G4 are included
later scope with external prerequisites, not substitutes for the early G5 gate.

## Verification (Run For Every PR)

For this plan-only PR: validate template sections, unique item IDs, all
dependencies and acyclicity, Files paths (existing or explicitly new), ownership
ordering, relative Markdown targets/anchors, absence of template tokens, and
`git diff --check`. Review the new plan against the actual workflow and active
sibling ownership. No application suite result is implied by documentation-only
CI skips.

For implementation PRs changing runtime Go, API/CLI, DB, dependencies, build,
or test wiring, use the repository baseline from the candidate checkout:

```sh
just lint
just unit-test
just integration-test
```

Root tests do not replace compiling/running integration-tagged packages or the
separate reagent module. Every new Go file that launches or requires Docker,
Kubernetes, a live server, or fault infrastructure under `test/robustness/`,
`test/lifecycle/`, `test/chaos/`, or `test/performance/` must carry
`//go:build integration`, including live helpers and recorder drivers. Pure
reference models under `test/model/` and pure report/math tests remain untagged
and must not initialize external infrastructure. Compile the new system runners
explicitly with `-tags=integration` inside the builder; the current precompiled
`./test` runner does not discover subpackages. Verify ordinary `go test ./...`
from the unit container requires no cluster, and the selected tagged runners
actually execute their named system tests. Add the relevant existing checks:

| Changed behavior | Additional existing checks |
| --- | --- |
| Owner/worker/distributed | `just integration-test-distributed`, `just integration-test-owner-memory`, plus B's actual multi-node scenarios |
| CLI journeys | Binary-driven integration scenarios, separate stdout/stderr, static CLI smoke on each supported release architecture |
| UI | `just ui-lint`, `just ui-test`, `just ui-e2e`; auth changes also `just ui-e2e-auth` |
| Auth/permissions/agent surface | `just integration-test-agent` and relevant live browser authorization cases |
| Podman | `just integration-test-podman` and affected scenarios; do not reduce full existing suite coverage |
| Helm/Kubernetes | `just helm-lint`, `just helm-template`, and the actual kind/Helm integration procedure in CI with the scenario's required replica/persistence settings |
| Reagents | `just reagents-lint`, `just reagents-test`, `just integration-test-infra` as affected |
| Workflow/filter/gate | `actionlint .github/workflows/ci.yml` and `python3 -m unittest discover -s scripts -p 'test_*.py' -v` after G1 (currently `test_ci.py`); lint new workflows, run all new validators immediately, and prove real lane selection |

New commands in G3/G6 are **proposed**, not available today. Before their CI
wiring, each stream tests its containerized entry script and records it in its
PR, including exact image inputs and exit conditions. Run focused new tests plus the relevant
baseline; a unit pass alone cannot prove a cluster/browser scenario.

Build through `just` and the repository Dockerfiles. Use current candidate
artifacts, never stale tags or an unverified precompiled runner. Enable feature
gates on both the server and test runner wherever required. Honor the job YAML
reference. No new CLI command or REST query is planned; any separately approved
addition requires binary/HTTP coverage in `test/` under AGENTS.md.

Shared Docker image tags, fixed container names, sockets, and ports remain
serialized across existing integration/UI/Helm lanes. Coordinate with existing
test owners and record the owner and full command lifetime in Progress; this
repository currently provides no shared lane-lock implementation. Do not assume
a lock file or invent a prerequisite to execute these recipes. New harness
isolation must be proven before relaxing serialization. Operate only on owned
test resources; no production
fault injection. Keep fault controls and coverage instrumentation out of normal
release/performance builds, and verify that exclusion. Record SHA, worktree,
commands, exit statuses, topology/config, and artifact paths without secrets.

Treat missing infrastructure, incomplete histories, timeouts in a checker, and
insufficient samples as blocked/inconclusive evidence. Do not retry indefinitely,
delete assertions, lower floors, or reinterpret a setup failure as a pass.
Revalidate after conflict resolution or changes that invalidate the tested SHA.

## Acceptance Criteria

1. **Contract coverage:** A1/A2 provide reviewed, separately identified safety,
   liveness, compatibility, and performance contracts; every claimed required
   guarantee has discovered and executed real-surface scenarios. Open Q1–Q6
   decisions remain visible until resolved with evidence.
2. **Distributed correctness:** B1 proves real pod-kill recovery; B3 runs on three
   actual members with persistent volumes, demonstrates each named fault, and
   passes independent history/effect
   checks for owner loss, stale owners, partition/heal, ambiguous responses,
   event replay, peer benching/cooldown, cross-node negative auth, and concurrent
   lifecycle actions. Quorum-loss rejection is asserted only for A1-resolved
   endpoint contracts; unresolved writes remain uncertain. Recovery bounds state
   their preconditions.
3. **Generated coverage:** C1/C2 exercise generated state sequences and sustained
   fuzzing, preserve minimized regressions, and report reproducibility limits.
   C3 demonstrably rejects selected known-bad histories/implementations while
   accepting contract-permitted duplicate and uncertain outcomes.
4. **Developer experience:** D1 proves the shipped CLI workflow, machine-output
   contract, invalid-input behavior, watch/interrupt handling, and cleanup with
   actual binaries on the agreed platform matrix.
5. **Console correctness:** D2/D3 prove live operator/auth workflows, keyboard
   accessibility and reviewed visuals, large-data usability, and convergence
   after a real backend fault. Browser retry cannot erase first-attempt failure.
6. **Performance:** E5 first enforces the repeatable instrumented SQL-work
   budget without claiming timing equivalence. E1–E4 reconcile
   offered/admitted/completed work, collect the full workload interval, compare
   identified base/candidate release artifacts,
   and enforce calibrated absolute/relative budgets with explicit uncertainty.
   Known regressions and insufficient evidence cannot yield a passing strict
   gate. Fixed-baseline trends expose cumulative degradation.
7. **Durability and lifecycle:** F4 qualifies single-node upgrades independently
   of B3; F1/F2 qualify every selected supported upgrade,
   persistent restart/replacement, and rollback/restore path against actual
   release artifacts and public reads. Unsupported capabilities are recorded
   prerequisites rather than invented or silently skipped.
8. **Broader qualification:** F3/G4 demonstrate bounded exploratory and sustained
   runs on the provisioned topology, including separate hosts, retained failure
   schedules, post-drain resource checks, and a reproducible short scenario.
9. **Evidence integrity:** G1/G2 retain first-attempt browser diagnostics and
   real CLI/server coverage with candidate provenance. Manifest validation fails
   on wrong topology, unexpected skips, missing instrumentation/evidence, or
   absent faults. Coverage reporting does not contaminate performance results.
10. **Actual enforcement:** G3/G5 establish the minimum credible gate; G7
    establishes candidate identity; G6/G4 prove the selected required checks on
    the real merge candidate and block publication on missing/failed same-SHA release
    qualification. Workflow, branch settings, scenario inventory, runbook, and
    Progress agree after each N-1 checkpoint. Preserve existing full
    engine/architecture coverage and label unpromoted lanes honestly.
    Nightly results are not represented as
    evidence from an individual PR.

## How To Pick Up Work

1. Read this plan, its contracts, and the applicable `AGENTS.md`.
2. Select unchecked, included items whose dependencies are verified ready.
   Start with A1, E1, and G1; record Q1–Q6 resolution as it becomes available.
3. Use an assigned worktree and branch from the verified base. Keep changes
   within the stream's file ownership and include its tests and behavior docs.
4. Run the required verification on the actual candidate changes. Record what
   passed, failed, or could not run, including actual scenario execution.
5. Update only assigned item checkboxes and notes. The wave orchestrator owns
   Progress and records completion from verified merged PRs. Complete the
   W<n>/N-1 docs sync before closing a wave; never dispatch concurrent runbook edits.
6. Follow the requested publication endpoint. Implementation PR titles use
   `<Imperative subject> (distributed-testing W<n>-<Greek stream suffix>)`.

Use `$exec-plan-wave` with this plan's path to orchestrate a wave in Codex.
Use `$draft-exec-plan` to revise the plan without starting implementation.

## Cross-References

- [CI runbook](../../ci.md), [roadmap](../../roadmap.md), and
  [execution operations](../../parallel-execution-operations.md).
- [Historical load measurements](../../load-testing-history.md): useful workload
  history, not a current performance baseline.
- [Getting started](../../getting-started.md) and
  [job-definition reference](../../caesium-job-llm-reference.md): developer-journey
  and fixture contracts.
- [Trust the Substrate](../completed/trust-the-substrate.md),
  [closed-loop arc](closed-loop-arc.md), [data circuit breaker](data-circuit-breaker.md),
  [resource right-sizing](resource-right-sizing.md), [backtesting](backtesting.md),
  and [window scheduling](window-scheduling.md): prior work and sibling ownership.
- [Native drafting skill](../../../.codex/skills/draft-exec-plan/SKILL.md) and
  [native execution skill](../../../.codex/skills/exec-plan-wave/SKILL.md).

Open source references researched for this proposal (2026-09-09); pin and verify
versions when adding dependencies, and keep them test-only where possible:

| Reference | Intended use and limit |
| --- | --- |
| [Jepsen](https://jepsen.io/services/analysis) and [etcd robustness](https://github.com/etcd-io/etcd/blob/main/tests/robustness/README.md) | Contract-based fault testing, histories, and regression validation; full Jepsen adoption is a later option. |
| [Testcontainers Go](https://golang.testcontainers.org/) and [Toxiproxy](https://github.com/Shopify/toxiproxy) | Isolated container lifecycle and controlled TCP faults; verify that discovered peer routes actually traverse the injector. |
| [Porcupine](https://github.com/anishathalye/porcupine) | Check histories against small sequential models; not a blanket checker for asynchronous DAG liveness. |
| [Rapid](https://pkg.go.dev/pgregory.net/rapid) and [Go fuzzing](https://go.dev/doc/security/fuzz/) | Generated state sequences, properties, and retained failing inputs. |
| [Go synctest](https://go.dev/blog/testing-time) and [gofail](https://github.com/etcd-io/gofail) | Isolated timer tests and a fault-hook design reference; real networking/CGO is not automatically deterministic. |
| [testscript](https://pkg.go.dev/github.com/rogpeppe/go-internal/testscript) | Optional isolated CLI fixture framework; existing binary helpers remain valid. |
| [Playwright accessibility](https://playwright.dev/docs/accessibility-testing), [visual assertions](https://playwright.dev/docs/api/class-pageassertions), and [flaky-test CLI behavior](https://playwright.dev/docs/test-cli) | Extend the existing browser stack with axe, selected reviewed screenshots, and honest retry outcomes. |
| [Open/closed workload models](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/) and [benchstat](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) | Workload-design reference for the Go driver (no k6 install) and repeated benchmark comparisons; insignificance alone is not equivalence. |
| [Go integration coverage](https://go.dev/doc/build-cover) | Instrument actual CLI/server execution and merge compatible profiles; graceful collection and killed-process incompleteness need handling. |
| [Chaos Mesh network faults](https://chaos-mesh.org/docs/simulate-network-chaos-on-kubernetes/) | Kubernetes partition/latency/loss/bandwidth scenarios on a provisioned qualification cluster. |

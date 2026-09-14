# Distributed Testing and Performance Confidence

Last updated: 2026-09-09

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

## Progress (as of 2026-09-14)

**W1 is closed and W2 implementation is merged; W2/N-1 is this PR.**
A1, A2, B1, E1, E5 and G1 have merged acceptance evidence and are checked. The
other 21 implementation items remain undispatched. This documentation checkpoint
is based on merged master `f3f5ce3a`, which includes all three W2 items. No
repository settings have changed, and no new lane has been added to `ci-ok`:
the owner-crash robustness runner is still invoked by hand, and G3 owns wiring
it. W2 delivers the first real multi-node owner-crash regression, an SQL-work
budget inside the existing required integration lane, and a fail-closed
scenario/evidence validator whose manifest rows are all still `absent`.

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
| W2/N-1 | this PR | Docs-only sync of `docs/ci.md`, this Progress dashboard, and the README/roadmap status lines to the three merged W2 items. Its merge SHA is unavailable until merge; resolve it on the next invocation and record it here. |

The overall 27-item plan remains active. W2 does not promote anything into the
enforced aggregate: the minimum credible gate still needs G3's lane wiring and
G5's promotion, and calibrated performance budgets, partitions, upgrades and
Console fault journeys have not shipped.

### Resume and tracking rules

The committed plan is the shared progress record; PR bodies hold detailed
candidate evidence. The machine-local `.codex/runs/distributed-testing/w<n>/state.md`
is a recovery aid, not a replacement for this dashboard. The orchestrator keeps
Progress current when PRs are published, revised, verified or merged, including
at a review-only endpoint. Interim Progress corrections do not wait for N-1;
N-1 consolidates the shared runbook after implementation merges.

On every `exec-plan-wave` invocation, fetch the current base and reconcile these
rows against live PR state, head/merge SHAs, reviews and current-head checks.
Resume the W2/N-1 PR while it is unfinished. Checkboxes mean merged acceptance
evidence; rows distinguish implementation and verification from merge. Once
W2/N-1 is verified merged, record its merge SHA and select **W3** in that
invocation; choose dependency-ready items while preserving unresolved Q1–Q6 and
shared-file ownership. Dependency readiness alone does not authorize dispatch
before the current wave's checkpoint.

Dependency-ready after W2: **G3 first, then G5** — they are sequential writers of
`.github/workflows/ci.yml` and `scripts/test_ci.py` and together complete the
minimum credible gate — plus **C1, D1, D2, F1, E2 and G2**, whose Files lists do
not intersect the workflow chain. **B2 stays blocked** on C1 and on the EX-HOOKS
handoff; do not dispatch it on B1's merge alone. G5 must not be run in parallel
with G3, and G2/A2's Python test files are discovered by G1's wildcard rather
than edited into the workflow.

### Stream Status

| Stream | Scope | Priority | Status |
| --- | --- | --- | --- |
| A | Contracts and scenario evidence (2 items) | P0 | Complete: A1 merged #465, A2 merged #470. Every manifest row is still `absent`; G3 then B3/D3/G6 own later rows |
| B | Real multi-node robustness (3 items) | P0 | B1 merged #472 and manually reproducible; B2 blocked on C1 and EX-HOOKS; B3 needs A2 + B2 + G3 |
| C | Reference models, generated tests, and checker validation (3 items) | P0 | A1 prerequisite merged; C1 dependency-ready for W3; C2 follows C1 |
| D | Developer and Console journeys (3 items) | P0 | A1/G1 prerequisites merged; D1 and D2 dependency-ready for W3; D3 needs B3/D2; expanded support needs Q4 |
| E | Correct load reporting and performance comparison (5 items) | P0 | E1 merged #466 and E5 merged #471; E2 dependency-ready for W3; E3 needs D2/C2; E4 needs Q2/Q5 |
| F | Upgrades, durability, and sustained faults (4 items) | P1 | Undispatched; F1 dependency-ready for W3; F4 follows F1 without B3; F2 adds cluster qualification |
| G | Diagnostics, coverage, and CI enforcement (7 items) | P0 | G1 merged #464; G2 and G3 dependency-ready for W3 after A2/B1/E5; G5 follows G3 in the same workflow chain |

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

- [ ] B2. Add targeted faults and correlate public event and effect histories
  Files: new `test/robustness/faults/`, new `test/robustness/history/`, new `test/robustness/faults_test.go`, `test/robustness/recorder/` (created by B1), new `internal/testfault/`, `internal/event/bus_dispatch.go` (only A1-approved dispatch hook; EX-HOOKS required), `build/Dockerfile.robustness` (created by B1).
  Depends on: B1, C1; external EX-HOOKS for the instrumented event case.
  Verify: Add external pause/resume, asymmetric network partition, and delayed/dropped-response control with independent activation/heal evidence. Verify discovered peer/worker addresses traverse the injector. Extend B1's test-runner recorder with an SSE subscriber using the existing `/v1/events` surface; retain duplicates and compare reconnection results with persisted public event reads using the documented sequence scope, not an assumed gap-free global counter. SSE is implementation-produced evidence and cannot reveal an external effect whose completion event was lost; preserve the separate raw task-effect ledger. Keep possibly committed timeouts and controller-observed ordering; recorder loss is inconclusive. Limit product instrumentation to an A1-justified durable-event-before-publication boundary in `internal/event/bus_dispatch.go`, with test-only controls absent/inert in release builds. Do not edit `internal/run/store.go`, owner completion, dispatch handlers, or worker runtime speculatively. Before dispatching the hook work, EX-HOOKS must identify the actual merged sibling base and exclusive file owner; otherwise that case is blocked, never skipped as passing. Run the ordinary release image without faults as a baseline.

- [ ] B3. Extend the core failure suite with fenced recovery, dispatch, and authorization cases
  Files: new `test/robustness/core_test.go`, new `test/robustness/testdata/`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B2, G3.
  Verify: Extend B1's owner-crash regression with owner pause past lease/stale completion, 2–1 split/heal, commit-before-response loss, durable-event-before-delivery crash, and cancel/retry/completion races, including fan-out/fan-in and frozen retry recipes. Make a worker unreachable, observe network-error rejection and peer benching through actual dispatch progress and available `caesium_dispatch_rejected_total`/`caesium_dispatch_stalled_total` metrics, then prove it receives new work after the cooldown. Send wrong-token `/internal/dispatch` and `/internal/complete` requests between real processes; cover valid-token stale-generation refusal separately and invalid peer credentials where mTLS is supported. Assert denied operations have no task/state/effect mutation. During quorum loss, enforce A1's bounded error contract only where resolved; retain uncertain writes and do not equate client timeout with rejection. Check accepted-state durability, generation authority, legal terminal/dependency behavior, raw external attempts, and recovery after healing. Persist histories/fault timelines/topology/digests; missing fault activation or stalling cannot pass.

### Stream C — Generated state transitions and checker strength

- [x] C1. Implement independent pure models and generated lifecycle tests
  Files: new `test/model/`, new `internal/run/model_properties_test.go`, new `internal/run/recovery_properties_test.go`, `go.mod`, `go.sum`.
  Depends on: A1.
  Verify: Generate bounded DAGs and sequences of admission, completion, cancellation, retry, lease expiry, checkpoint, and recovery using Rapid; minimize and retain failures. The independent `test/model` package is deliberately untagged, pure Go, and has no cluster/client startup, Docker socket, real network, or product decision-function dependency, so it belongs in `just unit-test`. Any test that launches external infrastructure must instead be integration-tagged and run by the system runner. Use Porcupine only for valid sequential contracts, with separate liveness/event models. Check checkpoint/replay equivalence and partition accounting with hermetic fixtures; compare real execution modes in B3 rather than claiming the pure model proves wiring. C1 owns its used model dependencies; adding a future proxy/container library needs a separately assigned owner.
  Note: shipped with `pgregory.net/rapid v1.3.0` and `github.com/anishathalye/porcupine v1.3.0` (toolchain-generated sums; `go mod tidy` also promoted the already-direct `prometheus/common` out of the indirect block). `test/model` is untagged pure Go and `independence_test.go` enforces its import allowlist mechanically, so the no-product-dependency and hermetic claims are a test rather than a convention. Porcupine checks only the run-status register; delivery, liveness and lease safety have separate models, each with a negative control proving the oracle rejects a planted defect. Retained minimized failures are committed as deterministic tests in `test/model/regression_test.go`, not as Rapid `.fail` artifacts: a fixed defect's bitstream replays as "no longer valid" and documents nothing, so the workflow in `doc.go` keeps `.fail` files only while a defect is open. The four counterexamples found were all in the new model; the product's `RunState`/recovery paths agreed with it under 4000+ generated cases per property. No cluster evidence is claimed — B1/B3 remain the only source for that.

- [ ] C2. Expand native fuzzing and deterministic concurrency regressions.
  Files: `pkg/jobdef/schemacompat/fuzz_test.go`, `internal/jobdef/diff/fuzz_test.go`, `internal/trigger/cron/fuzz_test.go`, new `internal/run/descriptor_fuzz_test.go`, new `internal/run/recovery_fuzz_test.go`, new `internal/worker/renewal_synctest_test.go`, new `scripts/fuzz-tests.sh`.
  Depends on: C1.
  Verify: the containerized script discovers/selects each intended fuzz target, performs a bounded exploration rather than seed-only execution, and preserves corpus artifacts. Persist minimized failures as normal regressions. Exercise isolated timer/cancellation/renewal logic with `testing/synctest` where supported; real sockets and CGO/dqlite remain outside its deterministic claim. Repeat selected concurrency tests with race detection and varied scheduling. No sleep-only success oracle or arbitrary valid-input rejection substitutes for a property.

- [ ] C3. Prove the checkers still detect known classes of defects.
  Files: new `test/model/oracle_regression_test.go`, new `test/model/testdata/`, new `scripts/validate-test-oracles.sh`.
  Depends on: A2, B3, C2.
  Verify: lost acknowledged state, accepted stale generations, invalid fan-in, missing replay, and unaccounted external effects fail their respective checkers. Include legal duplicate delivery and ambiguous timeout histories that must not be falsely rejected. Reproduce selected historical defects or temporary intentional mutations in an isolated checkout with a recorded known-bad SHA/patch; the tests catch them and the fixed candidate passes. Never ship mutations or change the user's working tree to run this validation. Missing evidence and checker resource exhaustion cannot become green.

### Stream D — Developer and Console journeys

- [x] D1. Extend binary-driven developer workflows and cleanup assertions.
  Files: `test/local_dev_test.go`, new `test/developer_journey_test.go`, new `test/developer_testdata/`.
  Depends on: A1.
  Verify: use the container-built release CLI in an empty temporary workspace for lint, preview, dev-once, watch/edit, interrupt, apply, and inspect. Check malformed input, paths with spaces, unavailable engines, cancellation/timeouts, and owned-resource cleanup. Parse JSON exclusively from stdout captured separately from stderr and assert exit status. Preserve existing helpers; testscript is an optional future harness substitution, not a reason to rewrite working tests. Run the currently shipped Linux architectures; add native platforms only after Q4 confirms support. Inspect the job-definition reference before writing any YAML fixtures.

- [ ] D2. Expand functional, visual, accessibility, and scale browser coverage.
  Files: new `ui/e2e/accessibility.spec.ts`, new `ui/e2e/visual.spec.ts`, new `ui/e2e/scale.spec.ts`, new `ui/e2e/network-recovery.spec.ts`, new `ui/e2e/visual.spec.ts-snapshots/`, `ui/e2e/helpers/fixtures.ts`, `ui/package.json`, `ui/package-lock.json`, `ui/playwright.config.ts`.
  Depends on: A1, G1.
  Verify: against the live backend, check create/apply or existing authoring workflows, trigger, logs, failure diagnosis, retry/cancel, reload, permission denial, credential expiry, reconnect, and stale-request races. Require keyboard/focus behavior and scoped axe checks. Review deterministic screenshots with fixed fonts, viewport, timestamps, and dataset. Load large DAGs, many partitions, paginated history, and long logs; assert all expected data remains reachable through virtualization/pagination. Fail on unexpected console/page errors. Chromium remains required; Q4 selects additional supported browser projects. Synthetic response manipulation tests are explicitly labeled and do not replace live persistence tests.

- [ ] D3. Exercise the operator journey across actual cluster failure.
  Files: new `ui/e2e/cluster-recovery.spec.ts`, new `ui/e2e/helpers/cluster.ts`, `ui/playwright.config.ts`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B3, D2.
  Verify: trigger from the Console, observe a run, fault its owner, reconnect through the supported entry point, and confirm the UI converges on the independently checked durable outcome and retained logs. Exercise both authenticated permissions and event-stream recovery. Observe the fault while the browser is connected; an API-only scenario with a final screenshot is insufficient. Reject duplicate/stale rows and false terminal success. Require available data to remain inspectable after reload.

### Stream E — Performance with correctness

- [x] E1. Make the existing load harness report and exit honestly.
  Files: `test/load/harness.go`, new `test/load/harness_test.go`, `justfile` (load-test recipe only).
  Depends on: none.
  Verify: reject invalid/zero configuration, use an overall deadline, sample metrics concurrently with submission, and return failure when expected runs fail, time out, cannot be triggered, or required samples are missing. Emit versioned machine-readable results with exact expected/observed counts and a usable failure classification. A live successful workload exits zero; deliberate bad-image and unavailable-server workloads exit nonzero. A controlled slow workload proves samples cover early and middle execution, including concurrency=1. Validate the reporter with fixtures as well as the live runs. Preserve useful existing human reports and execute the recipe inside the repository's containerized toolchain.

- [ ] E2. Extend the Go load driver with open-loop workloads and lifecycle measurements
  Files: `test/load/harness.go`, `test/load/harness_test.go` (created by E1), new `test/performance/workloads.json`, new `test/performance/load_test.go`.
  Depends on: A1, E1.
  Verify: Extend the containerized Go harness E1 already owns instead of introducing k6 or an unowned toolchain dependency. Support arrival-rate scheduling independent of completion, with offered/dropped/admitted/rejected/completed/backlog counts and bounded overload handling. Cover tiny/realistic tasks, wide/deep DAGs, fan-out, queues, cache hit/miss, API reads, subscribers, and drain. Tag the live driver `//go:build integration`; keep pure scheduling/report tests hermetic. Measure actual lifecycle intervals from observed events, marking unavailable values explicitly. Collect existing metrics and external resource observations, separating statements from rows and timed background work. Reconcile every admitted run; faster admission with backlog growth is not improvement. Production per-task resource telemetry remains owned by right-sizing.

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

- [ ] F4. Ship single-node previous-release upgrade qualification independently
  Files: new `test/lifecycle/standalone_test.go`, new `test/lifecycle/versions.json`, new `scripts/lifecycle-tests.sh`.
  Depends on: F1.
  Verify: With Q4's supported version pair, start the previous release on an owned persistent volume, create jobs/history/queued work through its public surface, stop it, and start the candidate on that same volume. Assert migrated/publicly readable state and resumed eligible work; also test a failed/unsupported transition according to F1's policy. Use integration-tagged tests and the existing container toolchain. Reuse versioned release artifacts and existing amd64/arm64 CLI checksum smoke evidence; do not require a three-node cluster, B3, or new paid infrastructure. Emit same-SHA qualification evidence and a direct containerized command; G6 later wires this standalone runner into the declared CI/release matrix.

### Stream G — Diagnostics, coverage, and enforceable CI

- [x] G1. Preserve first-attempt failures and discover all Python validator tests
  Files: `ui/playwright.config.ts`, `.github/workflows/ci.yml`, `scripts/test_ci.py`.
  Depends on: none.
  Verify: Make browser retries retain diagnostic evidence without erasing initial failure; a temporary controlled fail-once scenario must fail and upload its trace. Upload reports/screenshots/video/server logs including setup failure, and retain structured outcomes. In the same PR widen ci-config discovery from `test_ci.py` to `test_*.py` and assert that command in `scripts/test_ci.py`; a temporary extra matching test that fails must fail ci-config. This ensures A2's `test_test_evidence.py`, E3's `test_compare_performance.py`, and G2's `test_coverage.py` run immediately when introduced. Keep upload errors visible without masking the original failure; no automatic quarantine, reduced floors, or erased first attempts.

- [ ] G2. Collect actual CLI/server integration coverage with provenance.
  Files: new `build/Dockerfile.coverage`, new `scripts/integration-coverage.sh`, new `scripts/check-coverage.py`, new `scripts/test_coverage.py`.
  Depends on: A2, G1.
  Verify: use Go coverage-instrumented binaries built in containers, explicit package selection and `GOCOVERDIR`, and merge compatible profiles from CLI, server, and live browser journeys. Collect on graceful shutdown and provide explicit flushing where required; killed-process or missing profiles are incomplete evidence, not zero coverage or success. Label profile provenance, separate unit/integration/browser contributions, and report uncovered changed paths and critical contract gaps. Set package/diff ratchets after measuring a baseline instead of requiring a vanity global percentage. Demonstrate coverage of a real request-to-write-to-read path. Keep coverage/fault instrumentation out of performance artifacts and audit the separate `reagents/go.mod` scope when relevant.

- [x] G3. Wire the first persistent three-node lane and its exact scenario selectors
  Files: `.github/workflows/ci.yml`, `.github/actions/run-integration/action.yml`, `build/ci.docker-bake.hcl`, `build/ci.docker-bake-cache.hcl`, `justfile`, `scripts/collect-evidence.py` (new), `scripts/test_collect_evidence.py` (new), `scripts/test_ci.py`, `scripts/test_test_evidence.py`, `test/contracts/scenarios.json` (created by A2).
  Depends on: A2, B1, E5, G1.
  Verify: Add the B1 runner/image/recipe and artifact consumers using the existing kind and Helm infrastructure. Register the owner-crash scenario and E5's existing-suite budget with exact topology, candidate digest, and scenario identity. A new `test/robustness` binary must be explicitly built with integration tags; the existing `./test` precompiled binary cannot contain its subpackage tests. Verify selectors for code, fixtures, chart, dependency, image/build, and workflow changes, and a missing artifact/scenario failure. Preserve complete existing engine/architecture suites. Ship an executable lane and selector tests; promotion/settings/merge policy are separate G5/G7 work, so G3 does not wait for full B3, E4, F2, or G2.
  Note (W3-alpha): `just early-evidence` runs E5's `TestIntegrationTestSuite/TestStatementBudgetFixedWorkload` against the existing `integration-up` server and B1's `TestOwnerCrash` on an owned kind cluster (1 control-plane + 3 workers, three persistent Helm StatefulSet replicas), then `scripts/collect-evidence.py` reduces the retained artifacts to one report that `scripts/check-test-evidence.py --require early --strict` validates. Every reported field is read back from a real artifact (pod environments, kind config, bound PVC claims, recorder records, `docker inspect`, a retained `/metrics` scrape); a missing record or an unreadable `ctr` listing is reported inconclusive, never pass. The new `early-evidence` job uses the existing kind/Helm/`run-integration` infrastructure and is deliberately **not** in `ci-ok` or any required check — promotion is G5. The three rows are `proven` with `gates: ["early"]`; every other row stays `absent` with no gate. The Docker lane runs the `-test` image variant, so `e5-sql-work-budget` records `observed_server_image_id` and sets `require_candidate_digest: false` rather than claiming the release digest.

- [ ] G4. Schedule broader qualification and enforce release evidence
  Files: `.github/workflows/ci.yml`, new `.github/workflows/testing-qualification.yml`, `scripts/test_ci.py`.
  Depends on: F3, G6.
  Verify: Schedule longer seed/fuzz campaigns, browsers, multi-host faults, and soaks within Q1/Q2 limits, with cleanup, retained evidence, and triage ownership. Retain current native CLI checksum/smoke gates. Require supported release qualification in the actual publish chain or verified same-SHA evidence; demonstrate a failed qualification blocks publication. Run scheduled/manual campaigns and record one-command reproduction and explicit limits for N-1's runbook sync. Nightly results are broader evidence, not retroactive proof for every PR.

- [ ] G5. Promote the minimum credible gate into the fail-closed aggregate
  Files: `.github/workflows/ci.yml`, `scripts/ci-ok.py`, `scripts/test_ci.py`.
  Depends on: G3.
  Verify: On the existing hosted runner, require B1's demonstrated three-member owner-crash lane alongside E5's SQL-work budget and G1's honest browser outcomes. Record measured added duration and repeated stability without waiting for controlled-runner timing calibration or the full exploratory suite. Audit current required checks/rulesets under Q6 and verify that ci-ok is actually enforced; change settings only within execution authorization. Test failed/cancelled/skipped/missing new dependencies and require exact scenario/fault evidence. Existing optional mode/engine lanes retain their honest status until explicitly promoted. This milestone provides narrow documented confidence, not full partition/upgrade/performance equivalence.

- [ ] G6. Wire and qualify the remaining complete system suites
  Files: `.github/workflows/ci.yml`, `.github/actions/run-integration/action.yml`, `build/ci.docker-bake.hcl`, `build/Dockerfile.integration`, `justfile`, `scripts/ci-ok.py`, `scripts/test_ci.py`, `scripts/integration-test.sh`, `test/shard_test.go`, `test/contracts/scenarios.json` (created by A2).
  Depends on: B3, C3, D3, E4, F2, G2, G7.
  Verify: Extend G3/G5's functioning lane pattern to generated tests, full faults/journeys, standalone and cluster lifecycle runners, real coverage, and calibrated performance. Explicitly compile/run every new integration subpackage and validate fixture/config/path selection. Under Q1/Q2/Q5, measure capacity and audit promotion of the existing distributed/owner-memory/Podman/Helm/arm64 integration lanes; retain all real-surface coverage and document unpromoted limits. Fail closed on missing artifacts, unexpected skips, unexecuted scenarios, or inconclusive performance. Do not bundle new merge-policy machinery here; use G7's verified candidate identity.

- [ ] G7. Verify the merge-candidate policy independently of suite expansion
  Files: `.github/workflows/ci.yml`, `scripts/ci-ok.py`, `scripts/test_ci.py`.
  Depends on: G5.
  Verify: Resolve Q6's up-to-date-base versus merge-queue choice from current repository settings and test the actual prospective merge commit. If activating a queue, wire merge_group and prove producers, selectors, and required context names on that event; otherwise verify the chosen base-freshness enforcement. Bind evidence to the tested candidate SHA and test stale/missing candidate refusal. Record permissions/settings evidence and measured implications for Q1; no paid performance or multi-host infrastructure is a prerequisite.

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

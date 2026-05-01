# Parallel Execution Operations Guide

This guide covers runtime configuration, rollout, and troubleshooting for parallel job execution in Caesium.

## Execution Modes

- `local`: leader node builds the run DAG and executes tasks locally.
- `distributed`: leader node builds/enqueues the run DAG; worker loops on all nodes claim and execute tasks.

`CAESIUM_EXECUTION_MODE` defaults to `local`. Use `distributed` only after all nodes are on a build that supports distributed claims.

## Configuration Reference

| Variable | Default | Purpose |
| --- | --- | --- |
| `CAESIUM_MAX_PARALLEL_TASKS` | `runtime.NumCPU()` | Max local task concurrency within a job run (`local` mode). |
| `CAESIUM_TASK_FAILURE_POLICY` | `halt` | Task failure behavior: `halt` or `continue`. |
| `CAESIUM_TASK_TIMEOUT` | `0` | Per-task timeout (`0` disables timeout). |
| `CAESIUM_EXECUTION_MODE` | `local` | `local` or `distributed` execution model. |
| `CAESIUM_WORKER_ENABLED` | `true` | Enables distributed worker loop on this node. |
| `CAESIUM_WORKER_POOL_SIZE` | `4` | Max concurrent claimed tasks per node. |
| `CAESIUM_WORKER_POLL_INTERVAL` | `15s` | Fallback poll cadence for new claimable tasks. Distributed wakeups should handle normal claim latency. |
| `CAESIUM_WORKER_RECLAIM_INTERVAL` | `30s` | Minimum interval between expired-lease reclaim attempts. |
| `CAESIUM_WORKER_LEASE_TTL` | `5m` | Lease duration for claimed tasks before reclaim. |
| `CAESIUM_DATABASE_MAX_OPEN_CONNS` | `4` | Max SQL connections per node for dqlite/PostgreSQL. |
| `CAESIUM_DATABASE_MAX_IDLE_CONNS` | `2` | Max idle SQL connections per node for dqlite/PostgreSQL. |
| `CAESIUM_DATABASE_VOTERS` | `3` | Target dqlite voter count. Must be odd and at least 3. |
| `CAESIUM_DATABASE_STANDBYS` | `3` | Target dqlite standby count for failover headroom. Extra nodes settle as spares. |
| `CAESIUM_INTERNAL_WAKEUP_TOKEN` | `""` | Shared bearer token required for cross-node wakeups via `POST /internal/wakeup`. |
| `CAESIUM_WAKEUP_FANOUT_MODE` | `full` | Wakeup fanout strategy: `full` for every peer, or `gossip` for large clusters. |
| `CAESIUM_NODE_ADDRESS` | `127.0.0.1:9001` | Logical node identity written to `task_runs.claimed_by`. |
| `CAESIUM_NODE_LABELS` | `""` | Optional node labels (`k=v,k2=v2`) for task `nodeSelector` affinity. |

## Dqlite Topology

Use three stable control-plane nodes as voters. Add up to three standby nodes when you want fast failover without increasing quorum size. All remaining worker nodes can join the same dqlite cluster as spares; spares do not replicate the Raft log or vote, but they still open the Caesium database and claim work through the dqlite leader.

Set the same `CAESIUM_DATABASE_VOTERS` and `CAESIUM_DATABASE_STANDBYS` values on every node. For a 10-node deployment, the recommended shape is 3 voters, 3 standbys, and 4 spares.

Distributed wakeups use the dqlite cluster membership list, not `CAESIUM_DATABASE_NODES`, so spare workers receive wakeup hints after they join. Set the same `CAESIUM_INTERNAL_WAKEUP_TOKEN` on every node. The sender uses `Authorization: Bearer <token>` and receivers reject missing or incorrect tokens.

## Rollout Procedure (Distributed Mode)

1. Deploy the target version to all nodes with `CAESIUM_EXECUTION_MODE=local`.
2. Verify all nodes are healthy and on the same binary revision.
3. Enable distributed mode on all nodes (`CAESIUM_EXECUTION_MODE=distributed`) and restart/redeploy them.
4. Validate claims and execution ownership using the checks below.

Avoid mixed-mode operation (some old local-only schedulers + some distributed claimers), which can lead to duplicate execution windows.

## Runtime Verification

Use startup logs:

- `execution configuration` should include `execution_mode`, worker settings, and `node_labels`.
- In distributed mode, each node should log `launching distributed worker`.

Use worker status API:

- `GET /v1/nodes/:address/workers`
- Confirms claimed-task counts, running claims, attempt totals, lease expiry visibility, and last activity.

Use the embedded web UI:

- The Job and Run DAG views show worker attribution when `claimed_by` is present.
- Task detail panels surface `Claimed By`, claim attempts, and other task metadata for the selected node.

Use metrics:

- `caesium_worker_claims_total{node_id}`
- `caesium_worker_claim_contention_total{node_id}`
- `caesium_worker_lease_expirations_total{node_id}`
- `caesium_db_busy_retries_total`
- `caesium_reclaim_duration_seconds`
- `caesium_task_register_batch_size`

Dqlite warnings that contain `unknown data type: 0` include a `recent_db_statements` field with the last few rendered GORM statements observed by the process. Use that context to identify the nearby code path before escalating to an upstream go-dqlite issue.

## Tuning Guidance

- Increase `CAESIUM_WORKER_POOL_SIZE` to raise per-node throughput.
- Increase `CAESIUM_MAX_PARALLEL_TASKS` for better local mode utilization.
- Increase `CAESIUM_WORKER_LEASE_TTL` for long-running tasks to reduce reclaim churn.
- Increase `CAESIUM_WORKER_RECLAIM_INTERVAL` to reduce reclaim write pressure.
- Keep `CAESIUM_DATABASE_MAX_IDLE_CONNS` less than or equal to `CAESIUM_DATABASE_MAX_OPEN_CONNS`. Raise open connections cautiously and watch `caesium_db_busy_retries_total`; a higher pool can improve read/write overlap but can also add leader-side contention.
- Keep `CAESIUM_WORKER_POLL_INTERVAL` high enough to act as a fallback, not the primary coordination path. Lower it only when distributed wakeups are disabled or unhealthy.
- Use `CAESIUM_NODE_LABELS` + task `nodeSelector` to place specialized workloads.
- Prefer larger `RegisterTasks` batches. `caesium_task_register_batch_size` should normally show one sample per job run near that run's DAG width; a distribution pinned at `1` means callers are bypassing the batched path.

## Troubleshooting

### Tasks remain pending

- Confirm `CAESIUM_EXECUTION_MODE=distributed` and `CAESIUM_WORKER_ENABLED=true`.
- Confirm workers are running on nodes (`launching distributed worker` log line).
- Check `GET /v1/nodes/:address/workers` for active claims.
- Verify pending tasks have `outstanding_predecessors=0`.

### High claim contention

- Watch `caesium_worker_claim_contention_total`.
- Watch `caesium_db_busy_retries_total` for lock retries that are being absorbed before surfacing to workers.
- Increase `CAESIUM_WORKER_POOL_SIZE` judiciously and consider a higher poll interval.
- Ensure task affinity rules are not over-constraining claims.

### Frequent lease expirations or reclaimed tasks

- Increase `CAESIUM_WORKER_LEASE_TTL`.
- Confirm node stability and container runtime responsiveness.
- Watch `caesium_worker_lease_expirations_total`.

### Node affinity not being honored

- Validate `CAESIUM_NODE_LABELS` format (`k=v,k2=v2`) and exact value matching.
- Confirm job definitions use `nodeSelector` on steps.

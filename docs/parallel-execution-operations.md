# Parallel Execution Operations Guide

This guide covers runtime configuration, rollout, and troubleshooting for parallel job execution in Caesium.

## Execution Modes

- `local`: leader node builds the run DAG and executes tasks locally.
- `distributed`: leader node builds/enqueues the run DAG; worker loops on all nodes claim and execute tasks.

`CAESIUM_EXECUTION_MODE` defaults to `local`. Use `distributed` only after all nodes are on a build that supports distributed claims.

## Configuration Reference

| Variable | Default | Purpose |
| --- | --- | --- |
| `CAESIUM_MAX_PARALLEL_TASKS` | `1` | Max local task concurrency within a job run (`local` mode). |
| `CAESIUM_TASK_FAILURE_POLICY` | `halt` | Task failure behavior: `halt` or `continue`. |
| `CAESIUM_TASK_TIMEOUT` | `0` | Per-task timeout (`0` disables timeout). |
| `CAESIUM_EXECUTION_MODE` | `local` | `local` or `distributed` execution model. |
| `CAESIUM_WORKER_ENABLED` | `true` | Enables distributed worker loop on this node. |
| `CAESIUM_WORKER_POOL_SIZE` | `4` | Max concurrent claimed tasks per node. |
| `CAESIUM_WORKER_POLL_INTERVAL` | `2s` | Poll cadence for new claimable tasks. |
| `CAESIUM_WORKER_LEASE_TTL` | `5m` | Lease duration for claimed tasks before reclaim. |
| `CAESIUM_NODE_ADDRESS` | `127.0.0.1:9001` | Logical node identity written to `task_runs.claimed_by`. |
| `CAESIUM_NODE_LABELS` | `""` | Optional node labels (`k=v,k2=v2`) for task `nodeSelector` affinity. |

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

Use console TUI:

- Jobs detail DAG now shows `node: <address>` on task nodes when `claimed_by` is present.
- Node detail modal shows `Claimed By` for the focused task.

Use metrics:

- `caesium_worker_claims_total{node_id}`
- `caesium_worker_claim_contention_total{node_id}`
- `caesium_worker_lease_expirations_total{node_id}`

## Tuning Guidance

- Increase `CAESIUM_WORKER_POOL_SIZE` to raise per-node throughput.
- Increase `CAESIUM_MAX_PARALLEL_TASKS` for better local mode utilization.
- Increase `CAESIUM_WORKER_LEASE_TTL` for long-running tasks to reduce reclaim churn.
- Decrease `CAESIUM_WORKER_POLL_INTERVAL` for lower claim latency (at higher DB pressure).
- Use `CAESIUM_NODE_LABELS` + task `nodeSelector` to place specialized workloads.

## Troubleshooting

### Tasks remain pending

- Confirm `CAESIUM_EXECUTION_MODE=distributed` and `CAESIUM_WORKER_ENABLED=true`.
- Confirm workers are running on nodes (`launching distributed worker` log line).
- Check `GET /v1/nodes/:address/workers` for active claims.
- Verify pending tasks have `outstanding_predecessors=0`.

### High claim contention

- Watch `caesium_worker_claim_contention_total`.
- Increase `CAESIUM_WORKER_POOL_SIZE` judiciously and consider a higher poll interval.
- Ensure task affinity rules are not over-constraining claims.

### Frequent lease expirations or reclaimed tasks

- Increase `CAESIUM_WORKER_LEASE_TTL`.
- Confirm node stability and container runtime responsiveness.
- Watch `caesium_worker_lease_expirations_total`.

### Node affinity not being honored

- Validate `CAESIUM_NODE_LABELS` format (`k=v,k2=v2`) and exact value matching.
- Confirm job definitions use `nodeSelector` on steps.

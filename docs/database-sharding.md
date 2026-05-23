# Database Sharding

Caesium keeps dqlite as the built-in storage layer. Horizontal write scaling is
implemented by opening multiple databases on the same dqlite cluster and routing
run-scoped writes by job run ID.

`CAESIUM_DATABASE_SHARDS` defaults to `1`. At that value, the hot and cold
routes alias the catalog database and runtime behavior is unchanged. Values
greater than `1` are the Phase 4 sharding path and require
`CAESIUM_DATABASE_TYPE=internal`.

## Database Layout

| Database | Tables | Routing |
| --- | --- | --- |
| `caesium` | `atoms`, `triggers`, `jobs`, `tasks`, `task_edges`, `callbacks`, `backfills`, `task_cache`, `api_keys`, `audit_logs`, `notification_channels`, `notification_policies` | Catalog tables stay in the catalog database. |
| `caesium_hot_00` ... `caesium_hot_NN` | `job_runs`, `task_runs`, `callback_runs`, `execution_events` | Hot lifecycle tables route by `hash(job_run_id) % CAESIUM_DATABASE_SHARDS`. |
| `caesium_history` | Terminal `job_runs`, child `task_runs`, `callback_runs`, and `execution_events` after archival | Cold-history route. The archiver moves terminal runs here once implemented. |

All rows for a single job run must live on one hot shard. That keeps task
registration, claim, completion, retry, callback, and event writes
transactionally local without dqlite `ATTACH`, which is intentionally not
available.

## Router Contract

The data-layer router in `pkg/db` exposes:

- `Catalog()` for write-light definitions, auth, notifications, and operator
  tables.
- `HotShardForRun(runID)` for run lifecycle rows.
- `Cold()` for archived terminal run history.
- `RouteTable(table, runID)` for table-aware dispatch.

The shard hash is stable for a fixed shard count. Increasing
`CAESIUM_DATABASE_SHARDS` does not rebalance existing hot rows; run-scoped
lookups must use the shard count that was active when the run was created until
a rebalancer is designed.

## Constraints

Hot and cold shard migrations disable GORM foreign-key constraint creation for
cross-database relationships. Catalog records such as jobs and tasks remain the
source of truth, while hot shard rows reference them by ID and application code
performs the consistency checks that cannot be enforced across dqlite
databases.

Event sequence values are shard-local once `execution_events` moves to hot
shards. APIs that expose global event streams must merge by timestamp or add a
global cursor before sharded event storage is enabled for production traffic.

# Backfills

This guide covers how Caesium backfills work at runtime and how to operate them through the REST API, CLI, and embedded UI.

## What a Backfill Does

A backfill replays a cron-triggered job over the fire times in a half-open interval:

- `start` is inclusive.
- `end` is exclusive.
- Each logical date becomes a normal job run with `logical_date` attached as a run parameter.
- Regular cron fires also carry `logical_date`, so scheduled runs and backfill runs follow the same cache-keying model.

Backfills are only supported for jobs whose trigger type is `cron`.

## REST API

Create a backfill:

```http
POST /v1/jobs/{id}/backfill
```

Body:

```json
{
  "start": "2026-03-01T00:00:00Z",
  "end": "2026-03-10T00:00:00Z",
  "max_concurrent": 2,
  "reprocess": "failed"
}
```

Supported `reprocess` values:

- `none`: skip dates that already have any run.
- `failed`: rerun dates whose latest run did not succeed.
- `all`: queue every logical date in the interval.

Inspect backfills:

- `GET /v1/jobs/{id}/backfills`
- `GET /v1/jobs/{id}/backfills/{backfill_id}`

Cancel a backfill:

- `PUT /v1/jobs/{id}/backfills/{backfill_id}/cancel`

## CLI

Create a backfill:

```bash
caesium backfill create \
  --job-id <job-id> \
  --start 2026-03-01T00:00:00Z \
  --end 2026-03-10T00:00:00Z \
  --max-concurrent 2 \
  --reprocess failed \
  --server http://localhost:8080
```

List backfills for a job:

```bash
caesium backfill list --job-id <job-id> --server http://localhost:8080
```

Cancel a running backfill:

```bash
caesium backfill cancel \
  --job-id <job-id> \
  --backfill-id <backfill-id> \
  --server http://localhost:8080
```

## Cancellation Semantics

Backfill cancellation is durable and replica-safe:

- A cancel request is written to the backfill record, so any replica can observe it.
- The scheduler stops launching new logical-date runs after the request is visible.
- Already running work is allowed to drain normally.
- The backfill becomes terminal only after scheduling stops and in-flight work has finished.

In practice, this means you can create a backfill on one replica and cancel it from another replica without relying on process-local memory.

## Concurrency

`max_concurrent` limits how many logical dates a backfill may run at once.

- `1` is the safest default.
- Higher values increase throughput but also increase container pressure and database traffic.
- Cancellation does not interrupt already running tasks; it only prevents additional logical dates from starting.

## UI Behavior

The embedded UI surfaces backfills from the Jobs and Job Detail flows:

- Operators can start a backfill from Job Detail when the job uses a cron trigger.
- Active backfills show progress counters and status.
- Running backfills expose a cancel action.
- Cancellation is reflected through the shared database, so UI actions remain safe in multi-replica deployments.

## Operational Guidance

- Use `reprocess=none` for one-time catch-up windows you expect to be empty.
- Use `reprocess=failed` when replaying only historical failures.
- Use `reprocess=all` only when you intentionally want a full replay.
- In multi-replica deployments, any replica may accept create or cancel requests because the shared database is the source of truth.
- Large backfills may take time to settle into a terminal cancelled state because in-flight runs are allowed to finish.

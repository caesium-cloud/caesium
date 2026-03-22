# Backfills

This guide covers how Caesium backfills work at runtime and how to operate them safely in single-node and multi-replica deployments.

## What a Backfill Does

A backfill replays a job over the cron fire times in a closed interval:

- `start` is inclusive.
- `end` is exclusive.
- Each logical date becomes a normal job run with `logical_date` attached as a run parameter.

Backfills are only supported for jobs with a cron trigger.

## API

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

List and inspect backfills:

- `GET /v1/jobs/{id}/backfills`
- `GET /v1/jobs/{id}/backfills/{backfill_id}`

Cancel a backfill:

- `PUT /v1/jobs/{id}/backfills/{backfill_id}/cancel`

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
- Higher values increase throughput but also increase container pressure and DB traffic.
- Cancellation does not interrupt already running tasks; it only prevents additional logical dates from starting.

## Operational Guidance

- Use `reprocess=none` for a one-time catch-up over a window you know has no prior runs.
- Use `reprocess=failed` when you want to retry only failures in a historical window.
- Use `reprocess=all` only when you intentionally want to replay every date, even if prior runs succeeded.
- In multi-replica deployments, either replica can accept the cancel request because the request is persisted in the shared database.
- If you cancel a large backfill, expect the current in-flight runs to finish before the backfill reaches a terminal cancelled state.

## UI Behavior

The Jobs view shows active backfills, their progress counters, and a cancel action while the backfill is still running.

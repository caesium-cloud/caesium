# Airflow Parity

This page tracks Caesium features that intentionally mirror common Airflow authoring and operational workflows.

## Phase 1

Phase 1 focused on task execution semantics and operator controls needed to express the same DAG behavior in Caesium manifests and APIs.

### Implemented

| Capability | Status | Notes |
| --- | --- | --- |
| Task retries | Done | Steps support `retries`, `retryDelay`, and `retryBackoff`. Retry state is persisted per task run. |
| Trigger rules | Done | Steps support `all_success`, `all_done`, `all_failed`, `one_success`, and `always`. |
| Run parameters | Done | Triggers support `defaultParams`, and manual run requests may supply `params`. |
| Pause / unpause jobs | Done | REST API supports `PUT /v1/jobs/:id/pause` and `PUT /v1/jobs/:id/unpause`. |
| Console visibility | Done | The console shows paused jobs, run params, and task retry/trigger metadata in detail views. |

### Notes

- Paused jobs remain listed in the console and REST API, but trigger execution skips starting new runs until the job is unpaused.
- Trigger defaults and manually supplied run parameters are persisted onto the resulting run record for inspection.
- `one_success` joins are evaluated after all predecessor outcomes are known, so a failed sibling no longer causes an early skip when another predecessor succeeded.

## Related Docs

- [Job definitions](job-definitions.md)
- [Job schema reference](job-schema-reference.md)
- [Console guide](console.md)
- [Job definition plan](job-definition-plan.md)

# Design: Concurrency Strategies & Priority Queues

> Status: Proposed. This document covers concurrency strategies, rate limiting, and priority-based scheduling.

## Problem Statement

Caesium's current concurrency model is a single numeric knob: `maxParallelTasks` (defaults to CPU count in local mode, `CAESIUM_WORKER_POOL_SIZE` in distributed mode). This is insufficient for real-world scheduling scenarios:

- **Overlapping runs**: When a cron job takes longer than its interval, the next run starts while the previous is still running. There is no policy for handling this — both runs execute simultaneously, potentially contending for shared resources.
- **No run-level concurrency control**: `maxParallelTasks` limits parallelism within a single run, but there is no limit on concurrent runs of the same job.
- **No rate limiting**: Tasks that call external APIs cannot express "max 100 requests/minute." Users must build their own rate limiting inside containers.
- **No priority**: In distributed mode, workers claim tasks in arbitrary order. Critical pipelines get no preferential treatment when the cluster is saturated.
- **No fairness**: In multi-team clusters, one team's large pipeline can starve another team's small critical job.

---

## Current Architecture

### Local Mode

`internal/job/job.go` uses a semaphore (buffered channel) sized to `maxParallelTasks`:

```go
sem := make(chan struct{}, job.MaxParallelTasks)
// For each task ready to execute:
sem <- struct{}{}
go func() {
    defer func() { <-sem }()
    runTask(...)
}()
```

### Distributed Mode

`internal/worker/claimer.go` polls for claimable tasks and claims up to `CAESIUM_WORKER_POOL_SIZE` concurrently. Claims use database-level optimistic locking (UPDATE WHERE claimed_by IS NULL).

Neither mode tracks concurrent runs per job or implements any queuing discipline beyond FIFO.

---

## Design

### 1. Run Concurrency Strategies

Add a `concurrency` field to job metadata:

```yaml
metadata:
  alias: my-pipeline
  concurrency:
    maxRuns: 1              # max concurrent runs of this job (default: unlimited)
    strategy: queue         # what to do when a new run arrives and maxRuns is reached
```

**Strategies**:

| Strategy | Behavior | Use Case |
|----------|----------|----------|
| `queue` (default) | New run waits in a pending queue until a slot opens | Most pipelines — preserve all work |
| `replace` | Cancel the oldest running run, start the new one | "Latest wins" — redeployments, live data refresh |
| `skip` | Drop the new run silently | Idempotent cron jobs where overlap means the previous run is still valid |
| `fail` | Reject the new run with an error | Strict environments where overlap indicates a problem |

**Implementation**:

Add a pre-run check in the job execution path (`internal/job/job.go`):

```go
func (j *Job) Run(ctx context.Context) error {
    cfg := j.ConcurrencyConfig()

    if cfg.MaxRuns > 0 {
        activeRuns, err := j.runStore.CountActive(j.ID)
        if err != nil {
            return err
        }

        if activeRuns >= cfg.MaxRuns {
            switch cfg.Strategy {
            case "queue":
                return j.enqueueRun(ctx)
            case "replace":
                return j.replaceOldestRun(ctx)
            case "skip":
                log.Info("skipping run, max concurrent reached", "job", j.Alias)
                return nil
            case "fail":
                return ErrMaxConcurrentRunsReached
            }
        }
    }

    return j.executeRun(ctx)
}
```

**Run queue**: New `run_queue` table for the `queue` strategy:

```sql
CREATE TABLE run_queue (
    id          TEXT PRIMARY KEY,
    job_id      TEXT NOT NULL,
    params      TEXT,          -- JSON
    priority    INTEGER NOT NULL DEFAULT 2,
    created_at  TIMESTAMP NOT NULL
);

CREATE INDEX idx_run_queue_job ON run_queue(job_id, priority DESC, created_at ASC);
```

A background goroutine in the executor dequeues runs when active run count drops below `maxRuns`.

### 2. Rate Limiting

Rate limiting controls how fast tasks consume shared resources. Unlike concurrency (which limits how many things run simultaneously), rate limiting controls the throughput rate.

**Configuration** (job-level):

```yaml
metadata:
  alias: api-consumer
  rateLimits:
    - resource: "external-api"
      limit: 100
      window: 1m
    - resource: "database-pool"
      limit: 20
      window: 1s
```

**Configuration** (step-level):

```yaml
steps:
  - name: call-api
    image: client:latest
    rateLimit:
      resource: "external-api"
      units: 1               # how many units this task consumes (default: 1)
```

**Implementation**:

Rate limiting uses a sliding-window counter backed by the database:

```sql
CREATE TABLE rate_limit_tokens (
    resource    TEXT NOT NULL,
    window_key  TEXT NOT NULL,       -- e.g., "external-api:2026-04-04T12:05"
    consumed    INTEGER NOT NULL DEFAULT 0,
    limit_val   INTEGER NOT NULL,
    expires_at  TIMESTAMP NOT NULL,
    PRIMARY KEY (resource, window_key)
);
```

```go
// internal/ratelimit/limiter.go
type Limiter struct {
    db *gorm.DB
}

func (l *Limiter) Acquire(resource string, units int, limit int, window time.Duration) (bool, error) {
    windowKey := computeWindowKey(resource, window)
    expiry := time.Now().Truncate(window).Add(window)

    // Atomic increment-and-check
    result := l.db.Exec(`
        INSERT INTO rate_limit_tokens (resource, window_key, consumed, limit_val, expires_at)
        VALUES (?, ?, ?, ?, ?)
        ON CONFLICT (resource, window_key) DO UPDATE
        SET consumed = consumed + ?
        WHERE consumed + ? <= limit_val
    `, resource, windowKey, units, limit, expiry, units, units)

    return result.RowsAffected > 0, result.Error
}
```

When a task cannot acquire tokens, it is re-queued with a short delay rather than blocked. This keeps the worker slot free.

### 3. Priority Queues

Priority determines the order in which tasks are claimed in distributed mode and dequeued in the run queue.

**Configuration** (job-level):

```yaml
metadata:
  alias: critical-etl
  priority: high         # high (3), normal (2, default), low (1)
```

**Configuration** (per-run override):

```bash
caesium run --job-id <id> --priority high
# or via API
POST /v1/jobs/:id/run
{"params": {...}, "priority": "high"}
```

**Implementation**:

Add a `priority` column to the `task_runs` table (and `run_queue`):

```sql
ALTER TABLE task_runs ADD COLUMN priority INTEGER NOT NULL DEFAULT 2;
```

Update the distributed claimer to order by priority:

```go
// internal/worker/claimer.go
func (c *Claimer) claimNext(ctx context.Context) (*models.TaskRun, error) {
    var taskRun models.TaskRun
    err := c.db.WithContext(ctx).
        Where("status = ? AND claimed_by IS NULL", models.TaskStatusPending).
        Order("priority DESC, created_at ASC").  // highest priority first, then FIFO
        First(&taskRun).Error
    // ... claim with optimistic lock
}
```

**Priority values**:

| Name | Value | Behavior |
|------|-------|----------|
| `low` | 1 | Claimed only when no normal/high tasks are pending |
| `normal` | 2 | Default. Standard FIFO ordering among peers. |
| `high` | 3 | Claimed before normal and low tasks |

Priority is strictly ordering — it does not preempt running tasks. A high-priority task waits for a worker slot to become available; it does not kill a running low-priority task.

### 4. Fairness (Future Extension)

For multi-tenant clusters (after namespace isolation, Phase 3.1 of the roadmap), add a fairness layer:

- **Round-robin across namespaces**: When multiple namespaces have pending tasks, the claimer alternates between them rather than draining one namespace's queue first.
- **Namespace quotas**: Maximum concurrent tasks per namespace.

This is deferred until namespace isolation is implemented. Documenting it here for architectural awareness — the priority and rate limiting implementations should be designed to accommodate per-namespace scoping later.

---

## YAML Schema Changes

### Job Definition (`pkg/jobdef/definition.go`)

```go
type Metadata struct {
    Alias            string          `yaml:"alias" json:"alias"`
    Labels           map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
    // ... existing fields ...
    Priority         string          `yaml:"priority,omitempty" json:"priority,omitempty"`         // high, normal, low
    Concurrency      *Concurrency    `yaml:"concurrency,omitempty" json:"concurrency,omitempty"`
    RateLimits       []RateLimit     `yaml:"rateLimits,omitempty" json:"rateLimits,omitempty"`
}

type Concurrency struct {
    MaxRuns  int    `yaml:"maxRuns" json:"maxRuns"`
    Strategy string `yaml:"strategy" json:"strategy"` // queue, replace, skip, fail
}

type RateLimit struct {
    Resource string        `yaml:"resource" json:"resource"`
    Limit    int           `yaml:"limit" json:"limit"`
    Window   string        `yaml:"window" json:"window"`   // duration string: 1s, 1m, 1h
}
```

### Step Definition

```go
type Step struct {
    // ... existing fields ...
    RateLimit *StepRateLimit `yaml:"rateLimit,omitempty" json:"rateLimit,omitempty"`
}

type StepRateLimit struct {
    Resource string `yaml:"resource" json:"resource"`
    Units    int    `yaml:"units,omitempty" json:"units,omitempty"` // default: 1
}
```

---

## Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `caesium_run_queue_depth` | Gauge | `job_alias`, `priority` | Pending runs in the queue |
| `caesium_run_queue_wait_seconds` | Histogram | `job_alias` | Time runs spend in the queue |
| `caesium_run_skipped_total` | Counter | `job_alias`, `reason` | Runs skipped (skip strategy or rate limit) |
| `caesium_run_replaced_total` | Counter | `job_alias` | Runs cancelled by replace strategy |
| `caesium_rate_limit_acquired_total` | Counter | `resource` | Successful rate limit acquisitions |
| `caesium_rate_limit_rejected_total` | Counter | `resource` | Rate limit rejections (task re-queued) |
| `caesium_task_priority_claim_total` | Counter | `priority` | Tasks claimed by priority level |

---

## Implementation Plan

### Phase 1: Run Concurrency Strategies (P1)

1. Add `Concurrency` struct to `pkg/jobdef/definition.go`
2. Add `concurrency` field parsing and validation in the linter
3. Implement `CountActive(jobID)` on the run store
4. Implement concurrency check in job execution path (`internal/job/job.go`)
5. Implement `skip` and `fail` strategies (simplest — no queuing needed)
6. Add `run_queue` table and model
7. Implement `queue` strategy with background dequeuer
8. Implement `replace` strategy (cancel oldest active run, then execute)
9. Tests: unit tests per strategy, integration test for queue behavior

### Phase 2: Priority Queues (P1)

10. Add `priority` column to `task_runs` and `run_queue` tables
11. Add `Priority` field to metadata schema, parse in job definition
12. Update distributed claimer to order by `priority DESC, created_at ASC`
13. Update run queue dequeuer to respect priority ordering
14. Add `--priority` flag to `caesium run` CLI command
15. Add `priority` field to `POST /v1/jobs/:id/run` request body
16. Tests: claim ordering test with mixed priorities

### Phase 3: Rate Limiting (P2)

17. Add `rate_limit_tokens` table and model
18. Implement `Limiter.Acquire()` with sliding-window counter
19. Add `RateLimit` configuration to job metadata and step schema
20. Integrate rate limit check into task dispatch path
21. Re-queue behavior: when rate limited, mark task as `pending` with a retry-after delay
22. Rate limit token pruner (background goroutine, clean expired windows)
23. Tests: rate limit enforcement, window rollover, multi-task contention

### Phase 4: Observability (P2)

24. Prometheus metrics for all counters listed above
25. UI: run queue visualization on job detail page
26. UI: rate limit status indicator on tasks
27. CLI: `caesium job queue <alias>` to inspect the run queue

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Run queue grows unbounded | Disk/memory | Max queue depth per job (configurable, default 100). Oldest entries evicted when exceeded. |
| Replace strategy cancels long-running work | Lost compute | Log a warning. Document that `replace` is for idempotent/restartable jobs only. |
| Rate limit clock skew in distributed mode | Over/under-limiting | Window keys are coarse (1-minute granularity minimum). Clock skew within a minute is acceptable. For sub-second windows, NTP sync is a prerequisite. |
| Priority starvation (low-priority tasks never run) | Stuck pipelines | Document that priority is advisory, not preemptive. Monitor `caesium_run_queue_wait_seconds` for anomalies. Future: aging (bump priority after N minutes in queue). |
| Database contention on rate limit table | Slow claims | Use INSERT ON CONFLICT for atomic operations. Window keys naturally partition by time. Prune aggressively. |

---

## Examples

### Cron Job That Should Never Overlap

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: hourly-sync
  concurrency:
    maxRuns: 1
    strategy: skip
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: sync
    image: sync:latest
    command: ["sync.sh"]
```

### High-Priority Pipeline with Rate-Limited API Calls

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: critical-ingest
  priority: high
  concurrency:
    maxRuns: 2
    strategy: queue
  rateLimits:
    - resource: "vendor-api"
      limit: 60
      window: 1m
trigger:
  type: cron
  configuration:
    cron: "*/15 * * * *"
steps:
  - name: fetch
    image: fetcher:latest
    command: ["fetch.sh"]
    rateLimit:
      resource: "vendor-api"
      units: 1
  - name: transform
    image: etl:latest
    command: ["transform.sh"]
    dependsOn: [fetch]
```

### Deploy Pipeline with Replace Strategy

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: deploy-staging
  concurrency:
    maxRuns: 1
    strategy: replace
trigger:
  type: http
  configuration:
    path: "deploy/staging"
    secret: "secret://env/DEPLOY_SECRET"
steps:
  - name: deploy
    image: deploy:latest
    command: ["deploy.sh"]
```

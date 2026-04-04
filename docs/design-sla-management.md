# Design: SLA Management & Predictive ETAs

> Status: Proposed. This document covers SLA deadline tracking, predictive completion estimates, and escalation chains.

## Problem Statement

Caesium users have no way to express "this pipeline must finish by 06:00 UTC" or to receive proactive alerts when a pipeline is at risk of missing its deadline. The current options are:

- **Manual monitoring**: Watch the UI or check Prometheus dashboards.
- **Custom alerting**: Build Prometheus alerting rules on `caesium_job_run_duration_seconds` with static thresholds. This requires per-job configuration and doesn't account for natural variance in run durations.
- **Post-hoc detection**: Discover a missed deadline after the fact, often when downstream consumers complain.

No mainstream pipeline orchestrator does SLA management well. Airflow has a basic `sla` parameter that fires a callback when a task exceeds a duration threshold, but it doesn't support absolute deadlines, prediction, or multi-stage escalation. This is an opportunity for Caesium to offer something genuinely better.

---

## Design

### SLA Definition

Jobs declare SLA deadlines in their metadata:

```yaml
metadata:
  alias: critical-etl
  sla:
    deadline: "06:00"           # absolute time of day (24h format)
    timezone: "UTC"             # default: UTC
    escalation:
      - stage: at_risk
        channels:
          - type: slack
            target: "#data-alerts"
          - type: webhook
            target: "https://pagerduty.com/events"
      - stage: breached
        channels:
          - type: slack
            target: "#data-alerts"
          - type: webhook
            target: "https://pagerduty.com/events"
            headers:
              X-Severity: "high"
```

**Fields**:

| Field | Type | Description |
|-------|------|-------------|
| `deadline` | string | Time of day by which the run must complete (HH:MM, 24h) |
| `timezone` | string | IANA timezone for the deadline (default: UTC) |
| `escalation` | array | Ordered list of escalation stages |
| `escalation[].stage` | string | `at_risk` or `breached` |
| `escalation[].channels` | array | Notification channels for this stage |

**Stages**:

| Stage | Meaning | When Fired |
|-------|---------|------------|
| `at_risk` | Predicted to miss the deadline based on historical data | When the predicted completion time exceeds the deadline |
| `breached` | The deadline has passed and the run is still executing | When wall clock passes the deadline and the run is active |

### Predictive ETA Model

The prediction engine uses historical run durations to estimate when the current run will complete.

**Data source**: The `job_runs` table already stores `started_at` and `completed_at` for every run. The duration is `completed_at - started_at`.

**Algorithm**: Exponentially weighted moving average (EWMA) of the last N completed run durations, with outlier filtering.

```go
// internal/sla/predictor.go
type Predictor struct {
    db         *gorm.DB
    windowSize int       // number of recent runs to consider (default: 20)
    alpha      float64   // EWMA smoothing factor (default: 0.3)
}

type Prediction struct {
    EstimatedDuration time.Duration
    Confidence        float64       // 0.0-1.0 based on variance
    SampleCount       int
}

func (p *Predictor) Predict(jobID uuid.UUID) (*Prediction, error) {
    // 1. Load last N completed run durations
    var durations []float64
    rows, err := p.db.Raw(`
        SELECT EXTRACT(EPOCH FROM (completed_at - started_at))
        FROM job_runs
        WHERE job_id = ? AND status = 'succeeded'
        ORDER BY completed_at DESC
        LIMIT ?
    `, jobID, p.windowSize).Rows()

    // 2. Filter outliers (> 3 standard deviations from mean)
    filtered := removeOutliers(durations, 3.0)

    // 3. Compute EWMA
    ewma := computeEWMA(filtered, p.alpha)

    // 4. Compute confidence based on coefficient of variation
    cv := stddev(filtered) / mean(filtered)
    confidence := math.Max(0, 1.0 - cv)

    return &Prediction{
        EstimatedDuration: time.Duration(ewma * float64(time.Second)),
        Confidence:        confidence,
        SampleCount:       len(filtered),
    }
}
```

**Prediction for running jobs**: For a currently running job, the ETA is:

```
estimated_completion = started_at + predicted_duration
```

If the job has already been running longer than the predicted duration, the prediction degrades gracefully — the confidence drops and the SLA checker treats the job as at-risk.

**Minimum sample count**: Predictions require at least 3 completed runs. With fewer samples, the SLA checker skips the `at_risk` stage and only fires `breached` when the deadline actually passes.

### SLA Checker

A background goroutine that periodically evaluates SLA status for all running jobs with SLA definitions:

```go
// internal/sla/checker.go
type Checker struct {
    db        *gorm.DB
    predictor *Predictor
    notifier  *Notifier
    interval  time.Duration  // check interval (default: 1m)
}

func (c *Checker) Start(ctx context.Context) {
    ticker := time.NewTicker(c.interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            c.checkAll(ctx)
        }
    }
}

func (c *Checker) checkAll(ctx context.Context) {
    // 1. Load all running runs for jobs with SLA definitions
    runs := c.loadRunningRunsWithSLA()

    for _, run := range runs {
        sla := run.Job.SLAConfig()
        deadline := c.computeDeadline(sla, run.StartedAt)

        // 2. Check if deadline has passed (breached)
        if time.Now().After(deadline) && !run.SLABreachedNotified {
            c.notifier.Notify(run, "breached", sla.Escalation)
            c.markNotified(run.ID, "breached")
            continue
        }

        // 3. Predict completion and check if at risk
        prediction, err := c.predictor.Predict(run.JobID)
        if err != nil || prediction.SampleCount < 3 {
            continue // insufficient data, skip prediction
        }

        estimatedCompletion := run.StartedAt.Add(prediction.EstimatedDuration)
        if estimatedCompletion.After(deadline) && !run.SLAAtRiskNotified {
            c.notifier.Notify(run, "at_risk", sla.Escalation)
            c.markNotified(run.ID, "at_risk")
        }
    }
}
```

**Deadline computation**: The deadline is resolved relative to the run's trigger date:

- For cron jobs: the deadline applies to the same day as the `logical_date`
- For HTTP/event-triggered jobs: the deadline applies to the day the run was started
- If the deadline time is before the typical start time (e.g., deadline 02:00 for a job that starts at 22:00), it rolls to the next day

```go
func (c *Checker) computeDeadline(sla *SLAConfig, startedAt time.Time) time.Time {
    loc, _ := time.LoadLocation(sla.Timezone)
    startDay := startedAt.In(loc)

    deadline := time.Date(
        startDay.Year(), startDay.Month(), startDay.Day(),
        sla.DeadlineHour, sla.DeadlineMinute, 0, 0, loc,
    )

    // If deadline is before start time, it means "by this time tomorrow"
    if deadline.Before(startedAt) {
        deadline = deadline.Add(24 * time.Hour)
    }

    return deadline
}
```

### Notification System

SLA notifications reuse and extend the existing callback infrastructure (`internal/callback/`):

```go
// internal/sla/notifier.go
type Notifier struct {
    httpClient *http.Client
}

type Channel struct {
    Type    string            `yaml:"type" json:"type"`       // slack, webhook
    Target  string            `yaml:"target" json:"target"`   // URL or channel name
    Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

func (n *Notifier) Notify(run *RunWithSLA, stage string, escalation []EscalationStage) error {
    for _, esc := range escalation {
        if esc.Stage != stage {
            continue
        }
        for _, ch := range esc.Channels {
            payload := n.buildPayload(run, stage, ch)
            switch ch.Type {
            case "slack":
                n.sendSlack(ch.Target, payload)
            case "webhook":
                n.sendWebhook(ch.Target, ch.Headers, payload)
            }
        }
    }
    return nil
}
```

**Slack payload**:

```json
{
    "text": ":warning: SLA at risk for *critical-etl*",
    "blocks": [
        {
            "type": "section",
            "text": {
                "type": "mrkdwn",
                "text": "*Job*: critical-etl\n*Run*: abc-123\n*Started*: 04:15 UTC\n*Predicted completion*: 06:23 UTC\n*Deadline*: 06:00 UTC\n*Status*: At Risk (+23 min predicted overrun)"
            }
        }
    ]
}
```

**Webhook payload**:

```json
{
    "event": "sla_at_risk",
    "job_alias": "critical-etl",
    "run_id": "abc-123",
    "started_at": "2026-04-04T04:15:00Z",
    "deadline": "2026-04-04T06:00:00Z",
    "predicted_completion": "2026-04-04T06:23:00Z",
    "predicted_overrun_seconds": 1380,
    "confidence": 0.82,
    "sample_count": 18
}
```

### SLA Notification Deduplication

Each run tracks which SLA stages have been notified:

```sql
ALTER TABLE job_runs ADD COLUMN sla_at_risk_notified BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE job_runs ADD COLUMN sla_breached_notified BOOLEAN NOT NULL DEFAULT FALSE;
```

The checker only fires each stage once per run.

---

## API Changes

### New Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/v1/jobs/:id/sla` | SLA configuration and current status |
| `GET` | `/v1/jobs/:id/sla/history` | SLA compliance history (met/breached per run) |
| `GET` | `/v1/sla/at-risk` | All currently at-risk runs across all jobs |
| `GET` | `/v1/sla/breached` | All currently breached runs across all jobs |

### SLA Status Response

```json
{
    "job_alias": "critical-etl",
    "sla": {
        "deadline": "06:00",
        "timezone": "UTC"
    },
    "current_run": {
        "run_id": "abc-123",
        "started_at": "2026-04-04T04:15:00Z",
        "status": "running",
        "sla_status": "at_risk",
        "predicted_completion": "2026-04-04T06:23:00Z",
        "deadline": "2026-04-04T06:00:00Z"
    },
    "compliance": {
        "last_30_days": {
            "total_runs": 30,
            "met": 28,
            "breached": 2,
            "compliance_rate": 0.933
        }
    },
    "prediction": {
        "estimated_duration_seconds": 7680,
        "confidence": 0.82,
        "sample_count": 18
    }
}
```

---

## Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `caesium_sla_status` | Gauge | `job_alias`, `status` | Current SLA status (0=ok, 1=at_risk, 2=breached) |
| `caesium_sla_met_total` | Counter | `job_alias` | Runs that completed before the SLA deadline |
| `caesium_sla_breached_total` | Counter | `job_alias` | Runs that missed the SLA deadline |
| `caesium_sla_at_risk_total` | Counter | `job_alias` | Runs that were predicted to miss (may or may not have actually missed) |
| `caesium_sla_predicted_overrun_seconds` | Gauge | `job_alias` | Predicted overrun for currently at-risk runs |
| `caesium_sla_compliance_ratio` | Gauge | `job_alias` | Rolling 30-day compliance rate |

---

## CLI Changes

```bash
# Show SLA status for a job
caesium sla status <alias>
# Output:
#   Job:        critical-etl
#   Deadline:   06:00 UTC
#   Current:    Running (started 04:15 UTC)
#   Predicted:  06:23 UTC (at risk, +23 min)
#   Compliance: 93.3% (28/30 last 30 days)

# List all at-risk and breached SLAs
caesium sla list
# Output:
#   JOB              STATUS     DEADLINE   PREDICTED    OVERRUN
#   critical-etl     at_risk    06:00 UTC  06:23 UTC    +23m
#   daily-report     breached   08:00 UTC  --           +47m (running)

# Show SLA history for a job
caesium sla history <alias> --days 30
```

---

## Implementation Plan

### Phase 1: SLA Definition & Breach Detection (P2)

1. **SLA schema**: Add `SLA` struct to `pkg/jobdef/definition.go`
2. **SLA parsing**: Validate deadline format, timezone, escalation channels during lint
3. **SLA model**: Add `sla_at_risk_notified` and `sla_breached_notified` columns to `job_runs`
4. **SLA checker**: `internal/sla/checker.go` — background goroutine that checks breached deadlines
5. **Notifier**: `internal/sla/notifier.go` — Slack and webhook notification delivery
6. **Tests**: Deadline computation (timezone edge cases, day rollover), breach detection

### Phase 2: Predictive ETAs (P2)

7. **Predictor**: `internal/sla/predictor.go` — EWMA model over historical run durations
8. **At-risk detection**: Integrate predictor into the SLA checker loop
9. **Confidence scoring**: Coefficient of variation based confidence metric
10. **Minimum sample guard**: Skip prediction with < 3 completed runs
11. **Tests**: EWMA computation, outlier filtering, confidence scoring, edge cases (first run, all same duration, high variance)

### Phase 3: API & Observability (P2)

12. **API endpoints**: SLA status, history, at-risk list, breached list
13. **Prometheus metrics**: All metrics listed above
14. **SLA compliance recording**: On run completion, record whether SLA was met or breached
15. **CLI commands**: `caesium sla status`, `caesium sla list`, `caesium sla history`

### Phase 4: UI (P3)

16. **Job detail**: SLA badge (met/at_risk/breached), compliance rate, predicted ETA
17. **Dashboard**: SLA overview panel showing all at-risk and breached jobs
18. **Run detail**: SLA timeline showing deadline, predicted completion, actual completion
19. **Historical chart**: SLA compliance over time (last 30/60/90 days)

---

## Risks & Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Prediction accuracy with high-variance jobs | False at-risk alerts | Confidence scoring. Low-confidence predictions are shown but don't trigger escalation. Users can set a minimum confidence threshold. |
| Timezone edge cases (DST transitions) | Wrong deadline computation | Use IANA timezone database via `time.LoadLocation`. Test DST transition scenarios. |
| Notification spam (at-risk flapping) | Alert fatigue | Deduplication (one notification per stage per run). Cooldown period between at-risk notifications for the same job. |
| Historical data insufficient for new jobs | No prediction available | Graceful degradation: skip `at_risk`, only fire `breached` on actual deadline miss. Surface "insufficient data" in API/UI. |
| SLA checker adds CPU overhead | Performance | Check interval is configurable (default 1m). Checker only loads runs for jobs with SLA definitions. Prediction queries use indexed columns. |

---

## Examples

### Critical ETL with Two-Stage Escalation

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: critical-etl
  sla:
    deadline: "06:00"
    timezone: "UTC"
    escalation:
      - stage: at_risk
        channels:
          - type: slack
            target: "#data-alerts"
      - stage: breached
        channels:
          - type: slack
            target: "#data-alerts"
          - type: webhook
            target: "https://events.pagerduty.com/v2/enqueue"
            headers:
              Content-Type: "application/json"
trigger:
  type: cron
  configuration:
    cron: "0 4 * * *"
    timezone: "UTC"
steps:
  - name: extract
    image: etl:latest
    command: ["extract.sh"]
    cache: true
  - name: transform
    image: etl:latest
    command: ["transform.sh"]
    dependsOn: [extract]
  - name: load
    image: etl:latest
    command: ["load.sh"]
    dependsOn: [transform]
```

### Report with Soft SLA (Slack Only)

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: daily-report
  sla:
    deadline: "08:00"
    timezone: "America/New_York"
    escalation:
      - stage: breached
        channels:
          - type: slack
            target: "#analytics"
trigger:
  type: cron
  configuration:
    cron: "0 6 * * 1-5"
    timezone: "America/New_York"
steps:
  - name: generate
    image: reports:latest
    command: ["generate.sh"]
```

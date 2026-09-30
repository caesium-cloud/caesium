# Driving Caesium from Temporal

> Status: Operator guide. Uses the shipped `POST /v1/jobs/:id/run` idempotency
> contract ([job-definitions.md](job-definitions.md#starting-runs-from-other-systems-outcomes-and-idempotency))
> and event triggers ([design-event-triggers.md](design-event-triggers.md)).
> Caesium does not depend on Temporal. Nothing here is a plugin; it is how the
> two fit together over Caesium's REST API.

[Temporal](https://temporal.io) and Caesium sit at different layers, and they
work best together, not as substitutes:

| | Temporal | Caesium |
|---|---|---|
| Owns | The business process: long-lived, stateful, code-defined workflows | The data work: DAGs of containers |
| Good at | Durable waits (hours to months), signals, human approval, compensation, huge numbers of small per-entity workflows | Any image as a task (no SDK), content-addressed caching, typed step contracts, lineage, freshness, backfill, `why`/`blame`/`reproduce`/receipts |
| Knows about data | Nothing; payloads are opaque and small | What ran, on which inputs, what it produced, and why |
| Runs as | A server plus a database, or Temporal Cloud, plus your worker processes | One binary with an embedded database |

The split: **Temporal decides *when* and *whether*; Caesium does the data work
and remembers it.** A month-end close, a model promotion with human sign-off, or
a customer onboarding flow is a Temporal workflow. The extract, load, validation
and training steps inside it are Caesium jobs. Each Caesium run ID lands in the
workflow's history as the link to lineage, receipts and `caesium why`.

Keep pipeline logic out of Temporal activities. Once ETL lives inside activity
code, it loses per-step caching, contracts, lineage and the local
`caesium dev` loop, and every language needs its own worker fleet.

## Pattern 1: a Temporal activity runs a Caesium job

The workflow calls one activity per Caesium job. The activity starts the run
with an `Idempotency-Key` and waits for it, heartbeating as it goes.

**Why the key matters.** Temporal activities run *at least once*. An activity
that times out, or whose worker dies after `POST /v1/jobs/:id/run` succeeded,
is retried. Without a key, the retry would start a second run. With
`Idempotency-Key: <workflow run ID>/<activity ID>`, every attempt of one
activity resolves to the same Caesium run:

- The key is stable across retries of an activity and unique per activity
  invocation. A workflow reset gets a new run ID and therefore a new Caesium run,
  which is what a reset means.
- A retry after the run finished gets the run's terminal status back, not a new
  run.
- Under `metadata.concurrency.strategy: queue`, the start answers `queued`;
  re-posting the same key answers with the run once the queue promotes it.
- `skipped` (concurrency `skip`, or a held upstream dataset) and `dropped` are
  final for that key, so the activity fails non-retryably and the workflow
  decides what to do.
- `409` (the job is paused, or `strategy: fail` with the slot busy) is
  retryable: Temporal's backoff retries with the same key.
- Re-running data work after a failed run is a *new* activity invocation, and so
  a new key, or Caesium's own `POST /v1/jobs/:id/runs/:run_id/retry`. Retrying
  the same activity would only re-attach to the failed run, which is why the
  example fails a finished-but-unsuccessful run non-retryably.

The activity (Go SDK; the same shape works in any Temporal SDK):

```go
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// RunJobInput names the Caesium job (by ID) and the run params.
type RunJobInput struct {
	JobID  string
	Params map[string]string
}

// RunJobResult is what the workflow records: the run ID is the link from
// Temporal's history to Caesium's lineage, receipts, and `caesium why`.
type RunJobResult struct {
	RunID  string
	Status string
}

type Activities struct {
	Server string // e.g. http://caesium:8080
	APIKey string
	HTTP   *http.Client
}

type startResponse struct {
	Outcome string `json:"outcome"`
	ID      string `json:"id"`
	Reason  string `json:"reason"`
	QueueID string `json:"queue_id"`
}

// RunJob starts a Caesium run and waits for it to finish. Every attempt of
// one activity sends the same Idempotency-Key, so a retry after a timeout or a
// worker crash re-attaches to the run the first attempt started.
func (a *Activities) RunJob(ctx context.Context, in RunJobInput) (RunJobResult, error) {
	info := activity.GetInfo(ctx)
	key := info.WorkflowExecution.RunID + "/" + info.ActivityID

	runID := ""
	for runID == "" {
		start, err := a.start(ctx, in, key)
		if err != nil {
			return RunJobResult{}, err
		}
		switch start.Outcome {
		case "created":
			runID = start.ID
		case "queued":
			// Re-posting the same key answers with the run once the
			// concurrency queue promotes it.
			activity.RecordHeartbeat(ctx, "queued "+start.QueueID)
			if err := sleep(ctx, 10*time.Second); err != nil {
				return RunJobResult{}, err
			}
		case "skipped", "dropped":
			return RunJobResult{}, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("caesium did not run job %s: %s (%s)", in.JobID, start.Outcome, start.Reason),
				"CaesiumRunNotStarted", nil)
		default:
			return RunJobResult{}, fmt.Errorf("caesium: unexpected start outcome %q", start.Outcome)
		}
	}

	for {
		status, err := a.status(ctx, in.JobID, runID)
		if err != nil {
			return RunJobResult{}, err // retried; the retry re-attaches via the key
		}
		switch status {
		case "succeeded":
			return RunJobResult{RunID: runID, Status: status}, nil
		case "failed", "cancelled", "skipped":
			// Retrying this activity would only re-attach to the same finished
			// run. Re-running the data work is a new activity (a new key).
			return RunJobResult{}, temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("caesium run %s %s", runID, status), "CaesiumRunFailed", nil,
				RunJobResult{RunID: runID, Status: status})
		}
		activity.RecordHeartbeat(ctx, runID)
		if err := sleep(ctx, 15*time.Second); err != nil {
			return RunJobResult{}, err
		}
	}
}

func (a *Activities) start(ctx context.Context, in RunJobInput, key string) (startResponse, error) {
	body, err := json.Marshal(map[string]any{"params": in.Params})
	if err != nil {
		return startResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.Server+"/v1/jobs/"+url.PathEscape(in.JobID)+"/run", bytes.NewReader(body))
	if err != nil {
		return startResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	raw, code, err := a.do(req)
	if err != nil {
		return startResponse{}, err
	}
	switch {
	case code == http.StatusAccepted:
		var out startResponse
		return out, json.Unmarshal(raw, &out)
	case code == http.StatusConflict:
		// Job paused, or `strategy: fail` with the slot busy. Temporal's retry
		// policy backs off and tries again with the same key.
		return startResponse{}, fmt.Errorf("caesium: %s", raw)
	case code >= 500:
		return startResponse{}, fmt.Errorf("caesium: %d %s", code, raw)
	default:
		// 400/404/422: the request itself is wrong; retrying cannot help.
		return startResponse{}, temporal.NewNonRetryableApplicationError(
			fmt.Sprintf("caesium rejected run start: %d %s", code, raw), "CaesiumBadRequest", nil)
	}
}

func (a *Activities) status(ctx context.Context, jobID, runID string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.Server+"/v1/jobs/"+url.PathEscape(jobID)+"/runs/"+url.PathEscape(runID), nil)
	if err != nil {
		return "", err
	}
	raw, code, err := a.do(req)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("caesium: get run: %d %s", code, raw)
	}
	var run struct {
		Status string `json:"status"`
	}
	return run.Status, json.Unmarshal(raw, &run)
}

func (a *Activities) do(req *http.Request) ([]byte, int, error) {
	if a.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.APIKey)
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, err
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
```

And a workflow that uses it, with a human approval between two data jobs:

```go
import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// MonthEndClose is the business process; Caesium does the data work inside it.
func MonthEndClose(ctx workflow.Context, month string) error {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 6 * time.Hour,
		HeartbeatTimeout:    2 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    5 * time.Minute,
		},
	})

	var a *Activities
	var ledger RunJobResult
	if err := workflow.ExecuteActivity(ctx, a.RunJob, RunJobInput{
		JobID:  "3f0c…", // the job's ID from GET /v1/jobs
		Params: map[string]string{"month": month},
	}).Get(ctx, &ledger); err != nil {
		return err
	}

	// Human sign-off can take days; the workflow waits durably for it.
	var approved bool
	workflow.GetSignalChannel(ctx, "controller-approval").Receive(ctx, &approved)
	if !approved {
		return temporal.NewApplicationError("close rejected", "Rejected")
	}

	return workflow.ExecuteActivity(ctx, a.RunJob, RunJobInput{
		JobID:  "9b21…",
		Params: map[string]string{"month": month, "ledger_run": ledger.RunID},
	}).Get(ctx, nil)
}
```

Operational notes:

- `POST /v1/jobs/:id/run` takes the job **ID**. Resolve an alias once with
  `GET /v1/jobs` and pass the ID in, or keep IDs in workflow configuration.
- Set `HeartbeatTimeout`. A dead worker is then detected in minutes, and the
  retry re-attaches through the key instead of waiting out `StartToCloseTimeout`.
- Poll `GET /v1/jobs/:id/runs/:run_id` as the example does, or subscribe to
  `GET /v1/events?run_id=<run-id>&types=run_completed,run_failed` (SSE) for push
  delivery.
- When API-key auth is on, give the worker a `runner` key scoped to the jobs it
  starts: `caesium auth key create --role runner --scope-jobs <alias>,...`.
- Cancelling the Temporal activity stops the *waiting*, not the Caesium run.
  Caesium has no REST endpoint to cancel a single run today, so a run that must
  stop needs `metadata.concurrency.strategy: replace` or an operator.

## Pattern 2: Temporal emits events, Caesium routes them

When a workflow does not need to wait for the data work, say "an order closed,
refresh whatever depends on orders", the activity posts an event and Caesium's
event triggers decide which jobs run:

```yaml
apiVersion: v1
kind: Job
metadata:
  alias: refresh-order-facts
trigger:
  type: event
  configuration:
    events:
      - type: "order.closed"
        source: "temporal"
    paramMapping:
      order_id: "$.order_id"
steps:
  - name: refresh
    image: alpine:3.23
    command: ["sh", "-c", "echo refreshing facts for order $CAESIUM_PARAM_ORDER_ID"]
```

```bash
curl -X POST "$CAESIUM/v1/events" \
  -H "Authorization: Bearer $CAESIUM_EVENT_INGEST_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"type":"order.closed","source":"temporal","data":{"order_id":"o-123"}}'
```

The workflow does not need to know which jobs exist; adding a consumer is a
YAML change on the Caesium side. `POST /v1/events` has no idempotency key, so a
retried activity can deliver an event twice. Make the downstream job safe to run
twice (Caesium's cache skips unchanged work), or use Pattern 1 when exactly one
run matters.

## Pattern 3: Caesium hands off to Temporal

The other direction, for when a pipeline's result should kick off or advance a
business process:

- **From a step.** A step whose image ships the `temporal` CLI starts or
  signals a workflow when the data is ready. Build the workflow ID from the
  injected `CAESIUM_RUN_ID`, so a retried step addresses the same workflow
  instead of creating another. Temporal's workflow-ID reuse and conflict
  policies decide whether a duplicate start is rejected or attaches to the
  existing execution.

  ```yaml
  - name: start-review
    image: <an image with the temporal CLI>
    command:
      - sh
      - -c
      - >-
        temporal workflow start --type ReviewLoad --task-queue reviews
        --workflow-id "review-load-$CAESIUM_RUN_ID"
  ```

- **From lifecycle events.** A notification policy with a `webhook` channel on
  `run_failed` or `sla_missed` can call a small bridge that uses
  signal-with-start on a Temporal workflow, for a long, human-driven follow-up.
  The notification payload carries `job_id`, `job_alias` and `run_id`, but not
  the run's params, so the bridge keys on `run_id`. For automated triage and
  approval-gated remediation, Caesium's own incident runtime
  ([design-agent-in-the-loop.md](design-agent-in-the-loop.md)) may already cover
  it. Pick one owner per concern.

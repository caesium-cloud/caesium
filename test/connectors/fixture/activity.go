//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

type runJobInput struct {
	JobID           string `json:"job_id"`
	RetryAfterStart bool   `json:"retry_after_start"`
	MetadataOnly    bool   `json:"metadata_only"`
}
type opaqueHint struct{}
type activities struct {
	keyMu       sync.RWMutex
	server, key string
	http        *http.Client
	journal     *journal
}

type startReply struct {
	Outcome string `json:"outcome"`
	ID      string `json:"id"`
	QueueID string `json:"queue_id"`
	Reason  string `json:"reason"`
}

func (a *activities) RunJob(ctx context.Context, in runJobInput) (any, error) {
	info := activity.GetInfo(ctx)
	key := info.WorkflowExecution.RunID + "/" + info.ActivityID
	a.journal.add(map[string]any{"kind": "activity_attempt", "workflow_id": info.WorkflowExecution.ID, "run_id": info.WorkflowExecution.RunID, "activity_id": info.ActivityID, "attempt": info.Attempt, "job_id": in.JobID, "idempotency_key": key})
	var hint connector.CorrelationEnvelope
	for {
		var start startReply
		if err := a.request(ctx, http.MethodPost, "/v1/jobs/"+url.PathEscape(in.JobID)+"/run", key, map[string]any{"params": map[string]string{}}, &start); err != nil {
			return nil, err
		}
		hint = connector.CorrelationEnvelope{Version: connector.CorrelationEnvelopeVersion, JobID: in.JobID, IdempotencyKey: key, Outcome: start.Outcome, QueueID: start.QueueID, RunID: start.ID}
		if err := hint.Validate(); err != nil {
			return nil, err
		}
		a.journal.add(map[string]any{"kind": "http_start", "workflow_id": info.WorkflowExecution.ID, "activity_id": info.ActivityID, "attempt": info.Attempt, "hint": hint})
		if in.MetadataOnly {
			activity.RecordHeartbeat(ctx, opaqueHint{})
		} else {
			activity.RecordHeartbeat(ctx, hint)
		}
		if in.RetryAfterStart && info.Attempt == 1 {
			return nil, fmt.Errorf("fixture fault: lost activity result after real Caesium admission")
		}
		switch start.Outcome {
		case "created":
		case "queued":
			if err := pause(ctx); err != nil {
				return nil, err
			}
			continue
		case "skipped", "dropped":
			return nil, temporal.NewNonRetryableApplicationError("Caesium admission refused", "AdmissionRefused", nil)
		default:
			return nil, fmt.Errorf("unexpected Caesium outcome %q", start.Outcome)
		}
		break
	}
	for {
		var run struct {
			Status string `json:"status"`
		}
		if err := a.request(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(in.JobID)+"/runs/"+url.PathEscape(hint.RunID), "", nil, &run); err != nil {
			return nil, err
		}
		switch run.Status {
		case "succeeded":
			if in.MetadataOnly {
				return opaqueHint{}, nil
			}
			return hint, nil
		case "failed", "cancelled", "skipped":
			return nil, temporal.NewNonRetryableApplicationError("Caesium run did not succeed", "RunFailed", nil)
		}
		if in.MetadataOnly {
			activity.RecordHeartbeat(ctx, opaqueHint{})
		} else {
			activity.RecordHeartbeat(ctx, hint)
		}
		if err := pause(ctx); err != nil {
			return nil, err
		}
	}
}
func (a *activities) Effect(ctx context.Context, actor connector.ActorEnvelope) error {
	info := activity.GetInfo(ctx)
	a.journal.add(map[string]any{"kind": "effect_attempt", "run_id": info.WorkflowExecution.RunID, "activity_id": info.ActivityID, "attempt": info.Attempt})
	// The independent journal is durable across worker restarts. Activity retry
	// cannot count another external effect under the same workflow run.
	a.journal.effect(info.WorkflowExecution.RunID, actor)
	return nil
}
func (a *activities) request(ctx context.Context, method, path, key string, body, out any) error {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.server+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Caesium-API-Key", os.Getenv("CAESIUM_MANUAL_TRIGGER_API_KEY"))
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	a.keyMu.RLock()
	runnerToken := a.key
	a.keyMu.RUnlock()
	if runnerToken != "" {
		req.Header.Set("Authorization", "Bearer "+runnerToken)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("caesium HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}
func pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(250 * time.Millisecond):
		return nil
	}
}

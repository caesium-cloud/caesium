//go:build integration

package robustness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/history"
	"github.com/google/uuid"
)

// persistedEvent is one row read back from the event store through the existing
// read-only database HTTP query surface. No new product API is introduced.
type persistedEvent struct {
	Sequence uint64
	Type     string
	RunID    string
	// TaskID is the event's own task identity, so completion evidence can be
	// correlated per task instead of by run-wide totals.
	TaskID       string
	BusPending   bool
	DispatchedAt string
}

// readPersistedEvents returns the persisted rows for one run together with the
// scope that read actually covered.
//
// DT-EVENT-01: a sequence identifies an event within the SELECTED store's
// lifetime. The scope therefore records which member was read and the inclusive
// sequence bounds the read returned, and marks itself incomplete when the row
// count reached the limit — a truncated read can support no claim at all.
func readPersistedEvents(ctx context.Context, h *cluster.HTTP, base, runID string, limit int) ([]persistedEvent, history.Scope, error) {
	id, err := uuid.Parse(runID)
	if err != nil {
		return nil, history.Scope{}, fmt.Errorf("refusing to interpolate an unvalidated run id %q: %w", runID, err)
	}
	if limit <= 0 {
		limit = 2000
	}
	sql := fmt.Sprintf(
		"SELECT sequence, type, run_id, bus_dispatch_pending, bus_dispatched_at, task_id "+
			"FROM execution_events WHERE run_id = '%s' ORDER BY sequence ASC", id.String())
	status, raw, err := h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/database/query", map[string]any{
		"sql":   sql,
		"limit": limit,
	})
	if err != nil {
		return nil, history.Scope{}, err
	}
	if status != http.StatusOK {
		return nil, history.Scope{}, fmt.Errorf("event query status %d: %s", status, truncate(raw, 512))
	}
	var resp cluster.QueryResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, history.Scope{}, fmt.Errorf("decode event query: %w", err)
	}

	scope := history.Scope{Store: base, Complete: len(resp.Rows) < limit}
	out := make([]persistedEvent, 0, len(resp.Rows))
	for _, row := range resp.Rows {
		if len(row) < 3 {
			return nil, scope, fmt.Errorf("event row has %d columns, want at least 3", len(row))
		}
		ev := persistedEvent{
			Sequence: uint64(anyInt64(row[0])),
			Type:     fmt.Sprint(row[1]),
			RunID:    fmt.Sprint(row[2]),
		}
		if len(row) > 3 {
			ev.BusPending = anyBool(row[3])
		}
		if len(row) > 4 && row[4] != nil {
			ev.DispatchedAt = strings.TrimSpace(fmt.Sprint(row[4]))
			if strings.EqualFold(ev.DispatchedAt, "<nil>") || strings.EqualFold(ev.DispatchedAt, "null") {
				ev.DispatchedAt = ""
			}
		}
		if len(row) > 5 && row[5] != nil {
			ev.TaskID = strings.TrimSpace(fmt.Sprint(row[5]))
			if strings.EqualFold(ev.TaskID, "<nil>") || strings.EqualFold(ev.TaskID, "null") {
				ev.TaskID = ""
			}
		}
		if ev.Sequence == 0 {
			return nil, scope, fmt.Errorf("persisted event row has no sequence: %v", row)
		}
		if scope.Min == 0 || ev.Sequence < scope.Min {
			scope.Min = ev.Sequence
		}
		if ev.Sequence > scope.Max {
			scope.Max = ev.Sequence
		}
		out = append(out, ev)
	}
	return out, scope, nil
}

func toHistoryPersisted(rows []persistedEvent) []history.Persisted {
	out := make([]history.Persisted, 0, len(rows))
	for _, r := range rows {
		out = append(out, history.Persisted{
			Sequence: r.Sequence,
			Type:     r.Type,
			RunID:    r.RunID,
			TaskID:   r.TaskID,
		})
	}
	return out
}

// taskStepNames maps a job's catalog task identity — the identity
// task_succeeded / task_failed events carry — to its step name, read from the
// shipped GET /v1/jobs/:id/tasks surface. It is what makes the effect
// correlation per task rather than run-wide.
func taskStepNames(ctx context.Context, h *cluster.HTTP, base, jobID string) (map[string]string, error) {
	tasks, err := h.ListJobTasks(ctx, base, jobID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tasks))
	for _, t := range tasks {
		if strings.TrimSpace(t.ID) == "" || strings.TrimSpace(t.Name) == "" {
			return nil, fmt.Errorf("catalog task %+v has no usable identity/name pair", t)
		}
		out[t.ID] = t.Name
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("job %s reported no catalog tasks", jobID)
	}
	return out, nil
}

func findPersisted(rows []persistedEvent, sequence uint64) (persistedEvent, bool) {
	for _, r := range rows {
		if r.Sequence == sequence {
			return r, true
		}
	}
	return persistedEvent{}, false
}

func anyInt64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		var n int64
		_, _ = fmt.Sscan(t, &n)
		return n
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		var n int64
		_, _ = fmt.Sscan(fmt.Sprint(t), &n)
		return n
	}
}

func anyBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "t"
	default:
		return anyInt64(v) != 0
	}
}

// latestSequence returns the store's highest sequence, used only as a resume
// cursor so a fresh subscription does not replay the whole retained store. It
// is never used as a completeness claim.
func latestSequence(ctx context.Context, h *cluster.HTTP, base string) (uint64, error) {
	status, raw, err := h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/database/query", map[string]any{
		"sql":   "SELECT MAX(sequence) FROM execution_events",
		"limit": 1,
	})
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("max sequence query status %d: %s", status, truncate(raw, 512))
	}
	var resp cluster.QueryResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, err
	}
	if len(resp.Rows) == 0 || len(resp.Rows[0]) == 0 || resp.Rows[0][0] == nil {
		return 0, nil
	}
	return uint64(anyInt64(resp.Rows[0][0])), nil
}

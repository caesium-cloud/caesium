//go:build integration

package cluster

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Query runs one read-only SQL statement through POST /v1/database/query.
func (h *HTTP) Query(ctx context.Context, base, sql string, limit int) (QueryResponse, []byte, error) {
	if limit <= 0 {
		limit = 200
	}
	status, raw, err := h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/database/query", map[string]any{
		"sql":   sql,
		"limit": limit,
	})
	if err != nil {
		return QueryResponse{}, raw, err
	}
	if status != http.StatusOK {
		return QueryResponse{}, raw, fmt.Errorf("query status %d: %s", status, truncate(raw, 1024))
	}
	var resp QueryResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return QueryResponse{}, raw, fmt.Errorf("decode query: %w", err)
	}
	return resp, raw, nil
}

// RetryRun posts POST /v1/jobs/:id/runs/:run_id/retry and returns the status
// and body so a 409 can be distinguished from a transport error.
func (h *HTTP) RetryRun(ctx context.Context, base, jobID, runID string) (status int, run Run, raw []byte, err error) {
	jid, err := uuid.Parse(jobID)
	if err != nil {
		return 0, Run{}, nil, fmt.Errorf("job id is not a uuid: %w", err)
	}
	rid, err := uuid.Parse(runID)
	if err != nil {
		return 0, Run{}, nil, fmt.Errorf("run id is not a uuid: %w", err)
	}
	status, raw, err = h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/jobs/"+jid.String()+"/runs/"+rid.String()+"/retry", map[string]any{})
	if err != nil {
		return status, Run{}, raw, err
	}
	if status == http.StatusAccepted {
		if err := json.Unmarshal(raw, &run); err != nil {
			return status, Run{}, raw, fmt.Errorf("retry 202 body is not a run: %w", err)
		}
	}
	return status, run, raw, nil
}

// RetryPartition posts the targeted partition retry route.
func (h *HTTP) RetryPartition(ctx context.Context, base, jobID, runID, taskID string, index int) (status int, raw []byte, err error) {
	jid, err := uuid.Parse(jobID)
	if err != nil {
		return 0, nil, err
	}
	rid, err := uuid.Parse(runID)
	if err != nil {
		return 0, nil, err
	}
	tid, err := uuid.Parse(taskID)
	if err != nil {
		return 0, nil, err
	}
	url := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions/%d/retry",
		strings.TrimRight(base, "/"), jid, rid, tid, index)
	return h.Do(ctx, http.MethodPost, url, map[string]any{})
}

// Metrics scrapes GET /metrics from one member.
func (h *HTTP) Metrics(ctx context.Context, base string) (string, error) {
	status, raw, err := h.Do(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/metrics", nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("metrics status %d: %s", status, truncate(raw, 512))
	}
	return string(raw), nil
}

// TriggerRunRaw posts a trigger without treating a non-202 as a hard error.
func (h *HTTP) TriggerRunRaw(ctx context.Context, base, jobID string) (status int, raw []byte, err error) {
	id, err := uuid.Parse(jobID)
	if err != nil {
		return 0, nil, fmt.Errorf("job id is not a uuid: %w", err)
	}
	return h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/jobs/"+id.String()+"/run", map[string]any{})
}

// WithTimeout returns a shallow copy using a client with the given timeout.
func (h *HTTP) WithTimeout(d time.Duration) *HTTP {
	if d <= 0 {
		d = 15 * time.Second
	}
	return &HTTP{Client: &http.Client{Timeout: d}, ManualKey: h.ManualKey}
}

// TaskRecipe is the frozen image/command on a task_runs row.
type TaskRecipe struct {
	ID        string
	TaskID    string
	Status    string
	Image     string
	Command   string
	ClaimedBy string
	Attempt   int
}

// QueryTaskRecipes reads frozen recipe fields for one run.
func (h *HTTP) QueryTaskRecipes(ctx context.Context, base, runID string) ([]TaskRecipe, error) {
	id, err := uuid.Parse(runID)
	if err != nil {
		return nil, fmt.Errorf("refusing to interpolate an unvalidated run id %q: %w", runID, err)
	}
	sql := fmt.Sprintf("SELECT id, task_id, status, image, command, claimed_by, attempt FROM task_runs WHERE job_run_id = '%s' ORDER BY id", id.String())
	resp, _, err := h.Query(ctx, base, sql, 200)
	if err != nil {
		return nil, err
	}
	out := make([]TaskRecipe, 0, len(resp.Rows))
	for _, row := range resp.Rows {
		if len(row) < 7 {
			return nil, fmt.Errorf("task_runs row has %d columns, want 7", len(row))
		}
		rowID, err := queryUUID(row[0])
		if err != nil {
			return nil, fmt.Errorf("task_runs.id: %w", err)
		}
		taskID, err := queryUUID(row[1])
		if err != nil {
			return nil, fmt.Errorf("task_runs.task_id for %s: %w", rowID, err)
		}
		out = append(out, TaskRecipe{
			ID:        rowID,
			TaskID:    taskID,
			Status:    fmt.Sprint(row[2]),
			Image:     fmt.Sprint(row[3]),
			Command:   fmt.Sprint(row[4]),
			ClaimedBy: fmt.Sprint(row[5]),
			Attempt:   int(int64From(row[6])),
		})
	}
	return out, nil
}

// The database query endpoint can return SQLite UUID bytes as a 32-character
// hex string, while the public run API encodes the same UUID with hyphens.
// Reject anything that cannot be mapped to one unambiguous UUID.
func queryUUID(value any) (string, error) {
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("unexpected UUID cell %T (%v)", value, value)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid UUID cell %q: %w", raw, err)
	}
	return id.String(), nil
}

// DecodeBlob turns a database/query cell into bytes. Binary columns that are
// not valid UTF-8 are hex-encoded by the query surface.
func DecodeBlob(v any) ([]byte, error) {
	if v == nil {
		return nil, fmt.Errorf("nil blob")
	}
	switch t := v.(type) {
	case []byte:
		return t, nil
	case string:
		return decodeMaybeHex(t)
	default:
		return decodeMaybeHex(fmt.Sprint(t))
	}
}

func decodeMaybeHex(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty blob")
	}
	if len(s)%2 == 0 && isHex(s) {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return []byte(s), nil
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// EnvValue reads a container env var off the already-fetched pod spec. The
// value is for in-memory use only and must not be written to records.
func (m Member) EnvValue(name string) string {
	for _, c := range m.Pod.Spec.Containers {
		if c.Name != CaesiumContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value
			}
		}
	}
	return ""
}

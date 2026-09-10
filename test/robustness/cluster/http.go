//go:build integration

package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

type HTTP struct {
	Client    *http.Client
	ManualKey string
}

func NewHTTP(manualKey string) *HTTP {
	return &HTTP{
		Client:    &http.Client{Timeout: 15 * time.Second},
		ManualKey: manualKey,
	}
}

type Job struct {
	ID    string `json:"id"`
	Alias string `json:"alias"`
}

type Run struct {
	ID     string `json:"id"`
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Tasks  []Task `json:"tasks"`
}

type Task struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	ClaimedBy string `json:"claimed_by,omitempty"`
	Attempt   int    `json:"attempt"`
	Image     string `json:"image"`
	Engine    string `json:"engine"`
	Error     string `json:"error,omitempty"`
}

type QueryResponse struct {
	RowCount int              `json:"row_count"`
	Columns  []map[string]any `json:"columns"`
	Rows     [][]any          `json:"rows"`
}

type Lease struct {
	RunID          string
	OwnerNode      string
	Generation     int64
	LeaseExpiresAt string
}

func (h *HTTP) Do(ctx context.Context, method, url string, body any) (status int, raw []byte, err error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(h.ManualKey) != "" {
		req.Header.Set("X-Caesium-Manual-Trigger-Key", h.ManualKey)
	}
	resp, err := h.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, err
}

func (h *HTTP) Health(ctx context.Context, base string) error {
	status, _, err := h.Do(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("health %s: status %d", base, status)
	}
	return nil
}

func (h *HTTP) Apply(ctx context.Context, base string, defs []jobdef.Definition) error {
	status, raw, err := h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/jobdefs/apply", map[string]any{
		"definitions": defs,
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("apply status %d: %s", status, truncate(raw, 1024))
	}
	return nil
}

func (h *HTTP) JobByAlias(ctx context.Context, base, alias string) (Job, error) {
	status, raw, err := h.Do(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/jobs", nil)
	if err != nil {
		return Job{}, err
	}
	if status != http.StatusOK {
		return Job{}, fmt.Errorf("list jobs status %d: %s", status, truncate(raw, 1024))
	}
	var jobs []Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return Job{}, fmt.Errorf("decode jobs: %w", err)
	}
	for _, j := range jobs {
		if j.Alias == alias {
			if _, err := uuid.Parse(j.ID); err != nil {
				return Job{}, fmt.Errorf("job %s has invalid id %q", alias, j.ID)
			}
			return j, nil
		}
	}
	return Job{}, fmt.Errorf("job alias %s not found", alias)
}

func (h *HTTP) TriggerRun(ctx context.Context, base, jobID string) (Run, []byte, error) {
	id, err := uuid.Parse(jobID)
	if err != nil {
		return Run{}, nil, fmt.Errorf("job id is not a uuid: %w", err)
	}
	status, raw, err := h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/jobs/"+id.String()+"/run", map[string]any{})
	if err != nil {
		return Run{}, raw, err
	}
	if status != http.StatusAccepted {
		return Run{}, raw, fmt.Errorf("trigger status %d: %s", status, truncate(raw, 1024))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return Run{}, raw, fmt.Errorf("DT-ADMIT-01: bare 202 without run body")
	}
	var run Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return Run{}, raw, fmt.Errorf("DT-ADMIT-01: 202 body is not a run: %w (%s)", err, truncate(raw, 1024))
	}
	if _, err := uuid.Parse(run.ID); err != nil {
		return Run{}, raw, fmt.Errorf("DT-ADMIT-01: 202 run id is not a uuid: %q", run.ID)
	}
	if run.JobID != "" && run.JobID != id.String() {
		return Run{}, raw, fmt.Errorf("run job_id %s does not match %s", run.JobID, id)
	}
	return run, raw, nil
}

func (h *HTTP) GetRun(ctx context.Context, base, jobID, runID string) (Run, error) {
	jid, err := uuid.Parse(jobID)
	if err != nil {
		return Run{}, err
	}
	rid, err := uuid.Parse(runID)
	if err != nil {
		return Run{}, err
	}
	status, raw, err := h.Do(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/jobs/"+jid.String()+"/runs/"+rid.String(), nil)
	if err != nil {
		return Run{}, err
	}
	if status != http.StatusOK {
		return Run{}, fmt.Errorf("get run status %d: %s", status, truncate(raw, 1024))
	}
	var run Run
	if err := json.Unmarshal(raw, &run); err != nil {
		return Run{}, err
	}
	return run, nil
}

func (h *HTTP) SystemNodes(ctx context.Context, base string) ([]byte, error) {
	status, raw, err := h.Do(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/system/nodes", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return raw, fmt.Errorf("system nodes status %d: %s", status, truncate(raw, 1024))
	}
	return raw, nil
}

func (h *HTTP) QueryLease(ctx context.Context, base, runID string) (Lease, error) {
	id, err := uuid.Parse(runID)
	if err != nil {
		return Lease{}, fmt.Errorf("lease query refused unvalidated run id %q: %w", runID, err)
	}
	sql := fmt.Sprintf("SELECT run_id, owner_node, generation, lease_expires_at FROM run_leases WHERE run_id = '%s'", id.String())
	status, raw, err := h.Do(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/database/query", map[string]any{
		"sql":   sql,
		"limit": 1,
	})
	if err != nil {
		return Lease{}, err
	}
	if status != http.StatusOK {
		return Lease{}, fmt.Errorf("lease query status %d: %s", status, truncate(raw, 1024))
	}
	var resp QueryResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Lease{}, err
	}
	if len(resp.Rows) == 0 {
		return Lease{}, fmt.Errorf("no run_leases row for %s", id)
	}
	row := resp.Rows[0]
	if len(row) < 4 {
		return Lease{}, fmt.Errorf("lease row has %d columns, want 4", len(row))
	}
	lease := Lease{
		RunID:          fmt.Sprint(row[0]),
		OwnerNode:      fmt.Sprint(row[1]),
		Generation:     int64From(row[2]),
		LeaseExpiresAt: fmt.Sprint(row[3]),
	}
	if lease.RunID != id.String() {
		return Lease{}, fmt.Errorf("lease run_id %s != %s", lease.RunID, id)
	}
	return lease, nil
}

func int64From(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int32:
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
	default:
		var n int64
		_, _ = fmt.Sscan(fmt.Sprint(t), &n)
		return n
	}
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func WaitLease(ctx context.Context, h *HTTP, base, runID, wantOwner string) (Lease, error) {
	var last error
	var lease Lease
	err := Poll(ctx, 500*time.Millisecond, func() (bool, error) {
		got, err := h.QueryLease(ctx, base, runID)
		if err != nil {
			last = err
			return false, nil
		}
		if wantOwner != "" && got.OwnerNode != wantOwner {
			last = fmt.Errorf("lease owner %s != %s", got.OwnerNode, wantOwner)
			return false, nil
		}
		if got.Generation < 1 {
			last = fmt.Errorf("lease generation %d", got.Generation)
			return false, nil
		}
		lease = got
		last = nil
		return true, nil
	})
	if err != nil {
		if last != nil {
			return Lease{}, fmt.Errorf("wait lease: %w (%v)", err, last)
		}
		return Lease{}, fmt.Errorf("wait lease: %w", err)
	}
	return lease, nil
}

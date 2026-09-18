//go:build integration

// Package lifecycle holds the single-node previous-release upgrade
// qualification (distributed-testing F4, procedure in F1's decision record).
//
// The harness is split the way B1's robustness harness is: scripts/lifecycle-tests.sh
// is the host controller that owns every image, volume, network and container,
// and this integration-tagged runner is the assertion half. It is compiled
// explicitly with `-tags=integration` inside the builder image — the
// precompiled ./test binary does not contain subpackage tests — and is executed
// once per phase, inside a container attached to the harness's private network,
// from the image of the side it is driving so that `docker cp`'d CLI is the
// CLI that side shipped.
//
// Phases (CAESIUM_LIFECYCLE_PHASE, one `-test.run` per phase):
//
//	selfcheck  TestLifecycleObservationValidation  prove an unobservable outcome is rejected
//	seed       TestLifecycleSeedPreviousRelease    drive v0.1.0, record the fixture
//	upgrade    TestLifecycleUpgradeToCandidate     F1 assertions 1-7 on the migrated volume
//	readdress  TestLifecycleCandidateAddressChange PR #536's supported re-address contract
//	probe      TestLifecycleProbe                  record one server's observable state
//	outcomes   TestLifecycleTransitionOutcomes     judge the recorded transitions
//
// Nothing here starts, stops or inspects a container: every container fact this
// file asserts on was written to the artifacts directory by the host
// controller, so a missing observation is reported blocked rather than passed.
package lifecycle

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const (
	healthDeadline   = 180 * time.Second
	runDeadline      = 5 * time.Minute
	pollInterval     = 500 * time.Millisecond
	httpTimeout      = 30 * time.Second
	eventIdleTimeout = 3 * time.Second
	eventHardLimit   = 45 * time.Second

	// inflightSleepSeconds keeps one run genuinely in flight when the previous
	// release is stopped, so the upgrade sees retained in-flight state.
	inflightSleepSeconds = "600"
	// predecessorSleepSeconds holds a run slot just long enough for a second
	// trigger to be admitted to the run queue, then terminates on its own so
	// nothing is left occupying the slot across the transition.
	predecessorSleepSeconds = "3"
	// queuedSleepSeconds is what the queued run executes once the CANDIDATE
	// dequeues it. Short, because by then it is the thing under test.
	queuedSleepSeconds = "2"

	statusPass     = "pass"
	statusFail     = "fail"
	statusBlocked  = "blocked"
	statusRecorded = "recorded-outcome"
)

// ---------------------------------------------------------------------------
// Fixture: everything the seed phase records and every later phase re-reads.
// ---------------------------------------------------------------------------

type tableSchema struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

type schemaSnapshot struct {
	Dialect string        `json:"dialect"`
	Version string        `json:"version"`
	Tables  []tableSchema `json:"tables"`
}

func (s schemaSnapshot) index() map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(s.Tables))
	for _, tbl := range s.Tables {
		cols := make(map[string]bool, len(tbl.Columns))
		for _, c := range tbl.Columns {
			cols[c] = true
		}
		out[tbl.Name] = cols
	}
	return out
}

type taskRunFixture struct {
	ID          string `json:"id"`
	TaskID      string `json:"task_id"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	Attempt     int    `json:"attempt"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
}

type runFixture struct {
	ID          string            `json:"id"`
	JobID       string            `json:"job_id"`
	Status      string            `json:"status"`
	Error       string            `json:"error"`
	Params      map[string]string `json:"params"`
	StartedAt   string            `json:"started_at"`
	CompletedAt string            `json:"completed_at,omitempty"`
	CreatedAt   string            `json:"created_at"`
	Tasks       []taskRunFixture  `json:"tasks"`
	// Events is the full pre-upgrade set of (sequence, type, task_id, payload)
	// tuples read from GET /v1/events?run_id=, and ResumeCursor is an explicit
	// cursor strictly below the lowest retained sequence — never a high-water
	// mark. A single run's sequences are legitimately sparse, so the post-upgrade
	// assertion is a SET containment check (see F1's assertion 4).
	Events       []eventTuple `json:"events"`
	ResumeCursor uint64       `json:"resume_cursor"`
}

type eventTuple struct {
	Sequence uint64 `json:"sequence"`
	Type     string `json:"type"`
	TaskID   string `json:"task_id"`
	Payload  string `json:"payload"`
}

func (e eventTuple) key() string {
	return fmt.Sprintf("%d|%s|%s|%s", e.Sequence, e.Type, e.TaskID, e.Payload)
}

// queueRowFixture mirrors the public queue view (api/rest/service/job.QueueItem),
// not the run_queue table: `claim_state` is what distinguishes a row genuinely
// waiting for capacity from one held by a claim whose dequeuer died.
type queueRowFixture struct {
	ID         string            `json:"id"`
	Position   int               `json:"position"`
	Priority   int               `json:"priority"`
	ClaimState string            `json:"claim_state"`
	Stale      bool              `json:"stale"`
	ClaimedBy  string            `json:"claimed_by"`
	ClaimedAt  string            `json:"claimed_at,omitempty"`
	EnqueuedAt string            `json:"enqueued_at"`
	Params     map[string]string `json:"params"`
}

type jobFixture struct {
	ID       string `json:"id"`
	Alias    string `json:"alias"`
	Manifest string `json:"manifest"`
	Exported string `json:"exported"`
}

type fixture struct {
	LifecycleID  string                `json:"lifecycle_id"`
	Suffix       string                `json:"suffix"`
	Pair         string                `json:"pair"`
	RecordedAt   string                `json:"recorded_at"`
	PreviousSide sideIdentity          `json:"previous_side"`
	Schema       schemaSnapshot        `json:"schema"`
	Jobs         map[string]jobFixture `json:"jobs"`
	SucceededRun runFixture            `json:"succeeded_run"`
	FailedRun    runFixture            `json:"failed_run"`
	InFlightRun  runFixture            `json:"in_flight_run"`
	Predecessor  runFixture            `json:"predecessor_run"`
	QueuedRow    queueRowFixture       `json:"queued_row"`
	QueueToken   string                `json:"queue_token"`
}

type sideIdentity struct {
	BaseURL  string          `json:"base_url"`
	Features map[string]any  `json:"features"`
	Nodes    json.RawMessage `json:"nodes,omitempty"`
}

// ---------------------------------------------------------------------------
// versions.json
// ---------------------------------------------------------------------------

type columnAddition struct {
	Table  string `json:"table"`
	Column string `json:"column"`
}

type standaloneMatrix struct {
	DatabaseShards          int              `json:"database_shards"`
	DataDirectory           string           `json:"data_directory"`
	NodeAddress             string           `json:"node_address"`
	AlternateNodeAddress    string           `json:"alternate_node_address"`
	ExpectedTableAdditions  []string         `json:"expected_table_additions"`
	ExpectedColumnAdditions []columnAddition `json:"expected_column_additions"`
}

type previousMatrix struct {
	Release string `json:"release"`
	Image   string `json:"image"`
	Schema  struct {
		Dialect    string `json:"dialect"`
		TableCount int    `json:"table_count"`
	} `json:"schema"`
}

type pairMatrix struct {
	ID         string           `json:"id"`
	Previous   previousMatrix   `json:"previous"`
	Standalone standaloneMatrix `json:"standalone"`
}

type versionMatrix struct {
	SchemaVersion int          `json:"schema_version"`
	Pairs         []pairMatrix `json:"pairs"`
}

func loadMatrix(t *testing.T) pairMatrix {
	t.Helper()
	path := filepath.Join(artifactsDir(t), "versions.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		blockf(t, "versions-matrix", "cannot read the version matrix at %s: %v", path, err)
	}
	var matrix versionMatrix
	require.NoErrorf(t, json.Unmarshal(raw, &matrix), "versions.json is not valid JSON")
	want := envOr("CAESIUM_LIFECYCLE_PAIR", "v0.1.0-to-candidate")
	for _, pair := range matrix.Pairs {
		if pair.ID == want {
			return pair
		}
	}
	blockf(t, "versions-matrix", "version matrix has no pair %q", want)
	return pairMatrix{}
}

// ---------------------------------------------------------------------------
// Environment, artifacts and case records.
// ---------------------------------------------------------------------------

func mustEnv(t *testing.T, name string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		t.Fatalf("%s is required; scripts/lifecycle-tests.sh sets it", name)
	}
	return v
}

func envOr(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

func artifactsDir(t *testing.T) string {
	t.Helper()
	return mustEnv(t, "CAESIUM_LIFECYCLE_ARTIFACTS")
}

type caseRecord struct {
	Name            string         `json:"name"`
	Phase           string         `json:"phase"`
	LifecycleID     string         `json:"lifecycle_id"`
	Status          string         `json:"status"`
	DurationSeconds float64        `json:"duration_seconds"`
	Detail          string         `json:"detail,omitempty"`
	Observations    map[string]any `json:"observations,omitempty"`
}

// writeCase stamps every record with this invocation's lifecycle id. The host
// controller's finalizer rejects any case record carrying a different id, so a
// record left behind by an earlier invocation can never be counted as evidence
// for this one.
func writeCase(t *testing.T, rec caseRecord) {
	t.Helper()
	rec.Phase = envOr("CAESIUM_LIFECYCLE_PHASE", "unknown")
	rec.LifecycleID = envOr("CAESIUM_LIFECYCLE_ID", "")
	dir := filepath.Join(artifactsDir(t), "cases")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create cases dir: %v", err)
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal case %s: %v", rec.Name, err)
	}
	path := filepath.Join(dir, sanitize(rec.Name)+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write case %s: %v", rec.Name, err)
	}
	t.Logf("case %s: %s (%.1fs) %s", rec.Name, rec.Status, rec.DurationSeconds, rec.Detail)
}

// blockf records a case as BLOCKED and fails. Blocked is never a pass: it means
// the harness could not observe what the case is about.
func blockf(t *testing.T, name, format string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(format, args...)
	writeCase(t, caseRecord{Name: name, Status: statusBlocked, Detail: detail})
	t.Fatalf("BLOCKED %s: %s", name, detail)
}

// runCase runs one numbered assertion as a subtest and records its outcome.
func runCase(t *testing.T, name string, fn func(t *testing.T)) bool {
	t.Helper()
	start := time.Now()
	ok := t.Run(name, fn)
	status := statusPass
	if !ok {
		status = statusFail
	}
	writeCase(t, caseRecord{
		Name:            name,
		Status:          status,
		DurationSeconds: time.Since(start).Seconds(),
	})
	return ok
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func writeJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	require.NoErrorf(t, err, "marshal %s", name)
	path := filepath.Join(artifactsDir(t), name)
	require.NoErrorf(t, os.MkdirAll(filepath.Dir(path), 0o755), "create dir for %s", name)
	require.NoErrorf(t, os.WriteFile(path, append(raw, '\n'), 0o644), "write %s", name)
}

func readJSON(t *testing.T, name string, into any) bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(artifactsDir(t), name))
	if err != nil {
		return false
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Logf("artifact %s is unreadable: %v", name, err)
		return false
	}
	return true
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	var fx fixture
	if !readJSON(t, "fixture.json", &fx) {
		blockf(t, "fixture", "the seed phase's fixture.json is missing or unreadable; nothing can be qualified")
	}
	return fx
}

// ---------------------------------------------------------------------------
// HTTP against the server under test.
// ---------------------------------------------------------------------------

type client struct {
	base      string
	manualKey string
	http      *http.Client
}

func newClient(t *testing.T) *client {
	t.Helper()
	return &client{
		base:      strings.TrimRight(mustEnv(t, "CAESIUM_LIFECYCLE_BASE_URL"), "/"),
		manualKey: os.Getenv("CAESIUM_MANUAL_TRIGGER_API_KEY"),
		http:      &http.Client{Timeout: httpTimeout},
	}
}

func (c *client) do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, c.base+path, nil)
	}
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.manualKey != "" {
		req.Header.Set("X-Caesium-Manual-Trigger-Key", c.manualKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	buf := &bytes.Buffer{}
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, buf.Bytes(), nil
}

func (c *client) getJSON(ctx context.Context, path string, into any) error {
	status, raw, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET %s: status %d: %s", path, status, truncate(raw, 512))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(raw, into)
}

// awaitHealthy is the only "wait" in this file that is allowed to be the start
// of a case: reaching /health is itself assertion 1.
func (c *client) awaitHealthy(ctx context.Context, deadline time.Duration) error {
	stop := time.Now().Add(deadline)
	var last error
	for time.Now().Before(stop) {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, _, err := c.do(reqCtx, http.MethodGet, "/health", nil)
		cancel()
		switch {
		case err != nil:
			last = err
		case status != http.StatusOK:
			last = fmt.Errorf("status %d", status)
		default:
			return nil
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("server at %s never reported healthy within %s: %w", c.base, deadline, last)
}

func (c *client) schema(ctx context.Context) (schemaSnapshot, error) {
	var resp struct {
		Dialect string `json:"dialect"`
		Version string `json:"version"`
		Tables  []struct {
			Name    string `json:"name"`
			Columns []struct {
				Name string `json:"name"`
			} `json:"columns"`
		} `json:"tables"`
	}
	if err := c.getJSON(ctx, "/v1/database/schema", &resp); err != nil {
		return schemaSnapshot{}, err
	}
	snap := schemaSnapshot{Dialect: resp.Dialect, Version: resp.Version}
	for _, tbl := range resp.Tables {
		entry := tableSchema{Name: tbl.Name}
		for _, col := range tbl.Columns {
			entry.Columns = append(entry.Columns, col.Name)
		}
		sort.Strings(entry.Columns)
		snap.Tables = append(snap.Tables, entry)
	}
	sort.Slice(snap.Tables, func(i, j int) bool { return snap.Tables[i].Name < snap.Tables[j].Name })
	return snap, nil
}

func (c *client) features(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	if err := c.getJSON(ctx, "/v1/system/features", &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *client) jobIDByAlias(ctx context.Context, alias string) (string, error) {
	var jobs []struct {
		ID    string `json:"id"`
		Alias string `json:"alias"`
	}
	if err := c.getJSON(ctx, "/v1/jobs", &jobs); err != nil {
		return "", err
	}
	for _, j := range jobs {
		if j.Alias == alias {
			if _, err := uuid.Parse(j.ID); err != nil {
				return "", fmt.Errorf("job %s has a non-uuid id %q", alias, j.ID)
			}
			return j.ID, nil
		}
	}
	return "", fmt.Errorf("no job with alias %q", alias)
}

type apiRun struct {
	ID          string            `json:"id"`
	JobID       string            `json:"job_id"`
	Status      string            `json:"status"`
	Error       string            `json:"error"`
	Params      map[string]string `json:"params"`
	StartedAt   string            `json:"started_at"`
	CompletedAt string            `json:"completed_at"`
	CreatedAt   string            `json:"created_at"`
	Tasks       []struct {
		ID          string `json:"id"`
		TaskID      string `json:"task_id"`
		Status      string `json:"status"`
		Error       string `json:"error"`
		Attempt     int    `json:"attempt"`
		ExitCode    *int   `json:"exit_code"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
	} `json:"tasks"`
}

func (r apiRun) fixture() runFixture {
	out := runFixture{
		ID:          r.ID,
		JobID:       r.JobID,
		Status:      r.Status,
		Error:       r.Error,
		Params:      r.Params,
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		CreatedAt:   r.CreatedAt,
	}
	for _, task := range r.Tasks {
		out.Tasks = append(out.Tasks, taskRunFixture{
			ID:          task.ID,
			TaskID:      task.TaskID,
			Status:      task.Status,
			Error:       task.Error,
			Attempt:     task.Attempt,
			ExitCode:    task.ExitCode,
			StartedAt:   task.StartedAt,
			CompletedAt: task.CompletedAt,
		})
	}
	sort.Slice(out.Tasks, func(i, j int) bool { return out.Tasks[i].ID < out.Tasks[j].ID })
	return out
}

// triggerRun posts a manual run. A queued run answers 202 with no body, which
// is not an error — the caller distinguishes them by the returned run id.
func (c *client) triggerRun(ctx context.Context, jobID string, params map[string]string) (apiRun, bool, error) {
	body := map[string]any{}
	if len(params) > 0 {
		body["params"] = params
	}
	status, raw, err := c.do(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/run", body)
	if err != nil {
		return apiRun{}, false, err
	}
	if status != http.StatusAccepted {
		return apiRun{}, false, fmt.Errorf("trigger run: status %d: %s", status, truncate(raw, 512))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return apiRun{}, false, nil // admitted to the queue, not started
	}
	var run apiRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return apiRun{}, false, fmt.Errorf("trigger run: 202 body is not a run: %w (%s)", err, truncate(raw, 512))
	}
	return run, true, nil
}

func (c *client) run(ctx context.Context, jobID, runID string) (apiRun, error) {
	var run apiRun
	err := c.getJSON(ctx, "/v1/jobs/"+jobID+"/runs/"+runID, &run)
	return run, err
}

func (c *client) runs(ctx context.Context, jobID string) ([]apiRun, error) {
	var runs []apiRun
	err := c.getJSON(ctx, "/v1/jobs/"+jobID+"/runs", &runs)
	return runs, err
}

func (c *client) queue(ctx context.Context, jobID string) ([]queueRowFixture, error) {
	var rows []queueRowFixture
	if err := c.getJSON(ctx, "/v1/jobs/"+jobID+"/queue", &rows); err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Params == nil {
			rows[i].Params = map[string]string{}
		}
	}
	return rows, nil
}

func isTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled", "skipped":
		return true
	}
	return false
}

func (c *client) awaitRunStatus(ctx context.Context, jobID, runID string, want func(apiRun) bool, deadline time.Duration) (apiRun, error) {
	stop := time.Now().Add(deadline)
	var last apiRun
	var lastErr error
	for time.Now().Before(stop) {
		run, err := c.run(ctx, jobID, runID)
		if err != nil {
			lastErr = err
		} else {
			last = run
			lastErr = nil
			if want(run) {
				return run, nil
			}
		}
		time.Sleep(pollInterval)
	}
	if lastErr != nil {
		return last, fmt.Errorf("run %s never satisfied the condition: %w", runID, lastErr)
	}
	return last, fmt.Errorf("run %s never satisfied the condition (last status %q, error %q)", runID, last.Status, last.Error)
}

// ---------------------------------------------------------------------------
// Event replay (SSE backlog, read as a SET).
// ---------------------------------------------------------------------------

// readEventBacklog connects to GET /v1/events?run_id=, optionally resuming from
// an explicit Last-Event-ID, and collects the persisted backlog. The stream is
// long-lived by design, so the read ends on an idle window rather than EOF.
func readEventBacklog(ctx context.Context, c *client, runID string, cursor uint64) ([]eventTuple, error) {
	streamCtx, cancel := context.WithTimeout(ctx, eventHardLimit)
	defer cancel()

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, c.base+"/v1/events?run_id="+runID, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if cursor > 0 {
		req.Header.Set("Last-Event-ID", fmt.Sprintf("%d", cursor))
	}
	if c.manualKey != "" {
		req.Header.Set("X-Caesium-Manual-Trigger-Key", c.manualKey)
	}
	// No client timeout: the stream never ends on its own.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/events?run_id=%s: status %d", runID, resp.StatusCode)
	}

	lines := make(chan string, 256)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-streamCtx.Done():
				return
			}
		}
		readErr <- scanner.Err()
		close(lines)
	}()

	var (
		tuples  []eventTuple
		curData string
		idle    = time.NewTimer(eventIdleTimeout)
	)
	defer idle.Stop()
	for {
		select {
		case <-streamCtx.Done():
			return tuples, nil
		case err := <-readErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				return tuples, err
			}
			return tuples, nil
		case line, ok := <-lines:
			if !ok {
				return tuples, nil
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(eventIdleTimeout)
			switch {
			case strings.HasPrefix(line, ":"):
				// heartbeat comment
			case strings.HasPrefix(line, "data: "):
				curData = strings.TrimPrefix(line, "data: ")
			case line == "":
				if curData != "" {
					tuple, err := parseEventTuple(curData)
					if err != nil {
						return tuples, err
					}
					tuples = append(tuples, tuple)
					curData = ""
				}
			}
		case <-idle.C:
			return tuples, nil
		}
	}
}

func parseEventTuple(data string) (eventTuple, error) {
	var evt struct {
		Sequence uint64          `json:"sequence"`
		Type     string          `json:"type"`
		TaskID   string          `json:"task_id"`
		Payload  json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal([]byte(data), &evt); err != nil {
		return eventTuple{}, fmt.Errorf("event frame is not JSON: %w (%s)", err, truncate([]byte(data), 256))
	}
	payload := ""
	if len(evt.Payload) > 0 {
		compact := &bytes.Buffer{}
		if err := json.Compact(compact, evt.Payload); err != nil {
			return eventTuple{}, err
		}
		payload = compact.String()
	}
	return eventTuple{Sequence: evt.Sequence, Type: evt.Type, TaskID: evt.TaskID, Payload: payload}, nil
}

func lowestSequence(tuples []eventTuple) uint64 {
	var low uint64
	for i, tuple := range tuples {
		if i == 0 || tuple.Sequence < low {
			low = tuple.Sequence
		}
	}
	return low
}

// ---------------------------------------------------------------------------
// CLI: each side's own binary, docker-cp'd out of that side's own image.
// ---------------------------------------------------------------------------

type cliResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runCLI invokes the extracted CLI, capturing stdout SEPARATELY from stderr so
// a machine-readable stdout can be asserted on without stream merging.
func runCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	bin := mustEnv(t, "CAESIUM_CLI_PATH")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := cliResult{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		t.Fatalf("caesium %s could not be executed: %v (stderr: %s)", strings.Join(args, " "), err, stderr.String())
	}
	t.Logf("caesium %s -> exit %d", strings.Join(args, " "), res.ExitCode)
	return res
}

func requireCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	res := runCLI(t, args...)
	require.Equalf(t, 0, res.ExitCode, "caesium %s failed\nstdout:\n%s\nstderr:\n%s",
		strings.Join(args, " "), res.Stdout, res.Stderr)
	return res
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---------------------------------------------------------------------------
// Fixture manifests.
// ---------------------------------------------------------------------------

func suffixOf(lifecycleID string) string {
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(lifecycleID))
	if len(clean) > 12 {
		clean = clean[len(clean)-12:]
	}
	if clean == "" {
		clean = "lifecycle"
	}
	return clean
}

func historyAlias(suffix string) string  { return "lifecycle-history-" + suffix }
func queueAlias(suffix string) string    { return "lifecycle-queue-" + suffix }
func inflightAlias(suffix string) string { return "lifecycle-inflight-" + suffix }

// taskMarker is the ownership token the host controller minted for THIS
// invocation (lifecycle id plus a per-invocation nonce). Every fixture task
// container echoes it, and the controller's cleanup matches it with a
// delimiter-anchored exact pattern — so it must stay free of whitespace and of
// regex or shell metacharacters, and it must never be derived from the
// lifecycle id alone (two ids can share a suffix).
func taskMarker(t *testing.T) string {
	t.Helper()
	marker := mustEnv(t, "CAESIUM_LIFECYCLE_TASK_MARKER")
	for _, r := range marker {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '=':
		default:
			t.Fatalf("CAESIUM_LIFECYCLE_TASK_MARKER %q contains unsupported character %q; "+
				"the ownership token must match [A-Za-z0-9_=-]+", marker, r)
		}
	}
	return marker
}

// historyManifest produces the succeeded and failed history. The exit code is a
// run parameter, so one definition yields both outcomes and the second step
// exercises a DAG edge that must not run after a failure.
func historyManifest(suffix, marker string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: http
  configuration:
    path: %s
steps:
  - name: emit
    image: alpine:3.23
    command: ["sh", "-c", "echo %s step=emit token=$CAESIUM_PARAM_TOKEN; exit $CAESIUM_PARAM_EXIT"]
  - name: finish
    image: alpine:3.23
    command: ["sh", "-c", "echo %s step=finish token=$CAESIUM_PARAM_TOKEN"]
`, historyAlias(suffix), historyAlias(suffix), marker, marker)
}

// queueManifest holds a run slot open so a second trigger is admitted to the
// run queue.
func queueManifest(suffix, marker string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  concurrency:
    maxRuns: 1
    strategy: queue
trigger:
  type: http
  configuration:
    path: %s
steps:
  - name: hold
    image: alpine:3.23
    command: ["sh", "-c", "echo %s token=$CAESIUM_PARAM_TOKEN; sleep $CAESIUM_PARAM_SLEEP"]
`, queueAlias(suffix), queueAlias(suffix), marker)
}

// inflightManifest supplies the run that is still executing when the previous
// release is stopped, so the upgrade sees retained in-flight state.
func inflightManifest(suffix, marker string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: http
  configuration:
    path: %s
steps:
  - name: work
    image: alpine:3.23
    command: ["sh", "-c", "echo %s token=$CAESIUM_PARAM_TOKEN; sleep $CAESIUM_PARAM_SLEEP"]
`, inflightAlias(suffix), inflightAlias(suffix), marker)
}

func writeManifests(t *testing.T, dir string, manifests map[string]string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for name, body := range manifests {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name+".job.yaml"), []byte(body), 0o644))
	}
}

// ---------------------------------------------------------------------------
// Phase: seed (previous release).
// ---------------------------------------------------------------------------

func TestLifecycleSeedPreviousRelease(t *testing.T) {
	start := time.Now()
	ctx := t.Context()
	matrix := loadMatrix(t)
	c := newClient(t)
	lifecycleID := mustEnv(t, "CAESIUM_LIFECYCLE_ID")
	suffix := suffixOf(lifecycleID)
	marker := taskMarker(t)

	if err := c.awaitHealthy(ctx, healthDeadline); err != nil {
		blockf(t, "seed-previous-release-healthy", "%v", err)
	}

	features, err := c.features(ctx)
	if err != nil {
		blockf(t, "seed-previous-release-features", "%v", err)
	}
	if _, present := features["data_assertions_enabled"]; present {
		t.Logf("NOTE: the previous release reports data_assertions_enabled; "+
			"the behavioural build discriminator no longer separates the pair (features: %v)", features)
	}

	snap, err := c.schema(ctx)
	if err != nil {
		blockf(t, "seed-previous-release-schema", "GET /v1/database/schema on the previous release failed: %v "+
			"(CAESIUM_DATABASE_CONSOLE_ENABLED must be true on BOTH sides)", err)
	}
	require.Equalf(t, matrix.Previous.Schema.Dialect, snap.Dialect,
		"previous release dialect %q != pinned %q", snap.Dialect, matrix.Previous.Schema.Dialect)
	require.Equalf(t, matrix.Previous.Schema.TableCount, len(snap.Tables),
		"the previous release reports %d tables, not the %d pinned for %s; the image under test is not %s",
		len(snap.Tables), matrix.Previous.Schema.TableCount, matrix.Previous.Release, matrix.Previous.Image)

	// --- definitions, applied through the previous release's OWN CLI ---
	defsDir := filepath.Join(artifactsDir(t), "defs", "seed")
	manifests := map[string]string{
		"history":  historyManifest(suffix, marker),
		"queue":    queueManifest(suffix, marker),
		"inflight": inflightManifest(suffix, marker),
	}
	writeManifests(t, defsDir, manifests)
	requireCLI(t, "job", "lint", "--path", defsDir)
	requireCLI(t, "job", "apply", "--path", defsDir, "--server", c.base)

	historyID, err := c.jobIDByAlias(ctx, historyAlias(suffix))
	require.NoError(t, err)
	queueID, err := c.jobIDByAlias(ctx, queueAlias(suffix))
	require.NoError(t, err)
	inflightID, err := c.jobIDByAlias(ctx, inflightAlias(suffix))
	require.NoError(t, err)

	// `caesium job export` does not exist at v0.1.0 (`caesium job --help` lists
	// apply/diff/lint/preview/queue/schema only), so the previous release can
	// only record the manifests it was given. The exported-manifest half of the
	// definitions fixture is therefore taken on the candidate, in assertion 6.
	jobs := map[string]jobFixture{
		"history":  {ID: historyID, Alias: historyAlias(suffix), Manifest: manifests["history"]},
		"queue":    {ID: queueID, Alias: queueAlias(suffix), Manifest: manifests["queue"]},
		"inflight": {ID: inflightID, Alias: inflightAlias(suffix), Manifest: manifests["inflight"]},
	}
	for key, job := range jobs {
		res := runCLI(t, "job", "export", job.Alias, "--server", c.base)
		if res.ExitCode == 0 {
			require.NotEmptyf(t, strings.TrimSpace(res.Stdout), "job export %s wrote nothing to stdout", job.Alias)
			job.Exported = res.Stdout
			jobs[key] = job
			continue
		}
		require.Containsf(t, res.Stderr, "unknown flag: --server",
			"`job export %s` failed on the previous release for a reason other than the subcommand's absence: %s",
			job.Alias, res.Stderr)
		t.Logf("the previous release has no `job export` subcommand; the exported manifest is recorded on the candidate only")
	}

	// --- succeeded history ---
	okToken := "ok-" + suffix
	okRun, started, err := c.triggerRun(ctx, historyID, map[string]string{"EXIT": "0", "TOKEN": okToken})
	require.NoError(t, err)
	require.True(t, started, "the succeeded-history run was queued instead of started")
	okFinal, err := c.awaitRunStatus(ctx, historyID, okRun.ID, func(r apiRun) bool { return isTerminal(r.Status) }, runDeadline)
	require.NoError(t, err)
	require.Equalf(t, "succeeded", okFinal.Status, "seed run %s did not succeed: %s", okRun.ID, okFinal.Error)

	// --- failed history ---
	failToken := "fail-" + suffix
	failRun, started, err := c.triggerRun(ctx, historyID, map[string]string{"EXIT": "7", "TOKEN": failToken})
	require.NoError(t, err)
	require.True(t, started, "the failed-history run was queued instead of started")
	failFinal, err := c.awaitRunStatus(ctx, historyID, failRun.ID, func(r apiRun) bool { return isTerminal(r.Status) }, runDeadline)
	require.NoError(t, err)
	require.Equalf(t, "failed", failFinal.Status, "seed run %s was expected to fail, got %q", failRun.ID, failFinal.Status)

	// --- in-flight work: still executing when the previous release is stopped ---
	inflightToken := "inflight-" + suffix
	inflightRun, started, err := c.triggerRun(ctx, inflightID, map[string]string{"SLEEP": inflightSleepSeconds, "TOKEN": inflightToken})
	require.NoError(t, err)
	require.True(t, started, "the in-flight run was queued instead of started")
	inflightLive, err := c.awaitRunStatus(ctx, inflightID, inflightRun.ID,
		func(r apiRun) bool { return r.Status == "running" && len(r.Tasks) > 0 }, 2*time.Minute)
	require.NoError(t, err, "the in-flight run never reached running with a task row")

	// --- queued work: triggered while its predecessor is running ---
	//
	// The predecessor terminates on its own well before the stop. The previous
	// release runs with both CAESIUM_RUN_QUEUE_ENABLED and
	// CAESIUM_RUN_QUEUE_DEQUEUER_ENABLED false (cmd/start/start.go ORs them), so
	// the row is admitted to the queue — admission never consults either flag —
	// but never drained on this side. Draining it is the candidate's job, and
	// that is what assertion 5 measures.
	predecessorToken := "predecessor-" + suffix
	predecessorRun, started, err := c.triggerRun(ctx, queueID, map[string]string{"SLEEP": predecessorSleepSeconds, "TOKEN": predecessorToken})
	require.NoError(t, err)
	require.True(t, started, "the queue job's predecessor was queued instead of started")
	_, err = c.awaitRunStatus(ctx, queueID, predecessorRun.ID,
		func(r apiRun) bool { return r.Status == "running" }, 2*time.Minute)
	require.NoError(t, err, "the queue job's predecessor never reached running")

	queueToken := "queued-" + suffix
	_, started, err = c.triggerRun(ctx, queueID, map[string]string{"SLEEP": queuedSleepSeconds, "TOKEN": queueToken})
	require.NoError(t, err)
	require.False(t, started, "the second trigger started a run instead of queueing it; the concurrency policy did not admit to the queue")

	predecessorFinal, err := c.awaitRunStatus(ctx, queueID, predecessorRun.ID,
		func(r apiRun) bool { return isTerminal(r.Status) }, runDeadline)
	require.NoError(t, err, "the queue job's predecessor never terminalized")
	require.Equalf(t, "succeeded", predecessorFinal.Status,
		"the queue job's predecessor did not succeed: %s", predecessorFinal.Error)

	// With the predecessor terminal and the previous release's dequeuer off, the
	// row must still be queued: nothing on this side may start it.
	rows, err := c.queue(ctx, queueID)
	require.NoError(t, err)
	require.Lenf(t, rows, 1, "expected exactly one run_queue row for %s, got %d: %+v", queueAlias(suffix), len(rows), rows)
	require.Equalf(t, queueToken, rows[0].Params["TOKEN"],
		"the queued row does not carry the token the trigger supplied: %+v", rows[0].Params)
	// claim_state/stale were added to the queue view after v0.1.0, so the
	// previous release serves neither; claimed_by is the field both versions
	// have, and it is what separates a row waiting for capacity from one held
	// by a dequeuer that died. Accept an absent claim_state, never a claimed one.
	require.Containsf(t, []string{"", "pending"}, rows[0].ClaimState,
		"the queued row is %q, not pending; it is held by a claim rather than waiting for capacity (%+v)",
		rows[0].ClaimState, rows[0])
	require.Emptyf(t, rows[0].ClaimedBy, "the queued row is claimed by %q", rows[0].ClaimedBy)
	require.Falsef(t, rows[0].Stale, "the queued row is reported stale: %+v", rows[0])
	require.NotEmptyf(t, rows[0].EnqueuedAt, "the queued row has no enqueued_at")
	queueRuns, err := c.runs(ctx, queueID)
	require.NoError(t, err)
	for _, r := range queueRuns {
		require.NotEqualf(t, queueToken, r.Params["TOKEN"],
			"the previous release started the queued row (run %s) even though its dequeuer is disabled", r.ID)
	}

	// --- events, as a set, with an explicit resume cursor below the lowest sequence ---
	fx := fixture{
		LifecycleID:  lifecycleID,
		Suffix:       suffix,
		Pair:         matrix.ID,
		RecordedAt:   time.Now().UTC().Format(time.RFC3339),
		PreviousSide: sideIdentity{BaseURL: c.base, Features: features},
		Schema:       snap,
		Jobs:         jobs,
		SucceededRun: okFinal.fixture(),
		FailedRun:    failFinal.fixture(),
		InFlightRun:  inflightLive.fixture(),
		Predecessor:  predecessorFinal.fixture(),
		QueuedRow:    rows[0],
		QueueToken:   queueToken,
	}
	for _, target := range []*runFixture{&fx.SucceededRun, &fx.FailedRun} {
		tuples, err := readEventBacklog(ctx, c, target.ID, 0)
		require.NoErrorf(t, err, "reading the pre-upgrade event backlog for run %s", target.ID)
		require.NotEmptyf(t, tuples, "run %s recorded no events before the upgrade; "+
			"there is nothing for assertion 4 to replay", target.ID)
		target.Events = tuples
		low := lowestSequence(tuples)
		require.Positivef(t, low, "run %s has a zero event sequence; the cursor cannot be placed below it", target.ID)
		target.ResumeCursor = low - 1
	}

	writeJSON(t, "fixture.json", fx)
	writeCase(t, caseRecord{
		Name:            "seed-previous-release",
		Status:          statusPass,
		DurationSeconds: time.Since(start).Seconds(),
		Detail: fmt.Sprintf("%s: %d tables, jobs %s/%s/%s, runs succeeded=%s failed=%s in-flight=%s, queued row %s",
			matrix.Previous.Release, len(snap.Tables), historyAlias(suffix), queueAlias(suffix), inflightAlias(suffix),
			okFinal.ID, failFinal.ID, inflightLive.ID, rows[0].ID),
		Observations: map[string]any{
			"previous_features":      features,
			"previous_table_ct":      len(snap.Tables),
			"queued_row_id":          rows[0].ID,
			"queued_row_claim_state": rows[0].ClaimState,
			"queued_row_claimed_by":  rows[0].ClaimedBy,
			"queued_row_claimed_at":  rows[0].ClaimedAt,
			"queued_row_enqueued_at": rows[0].EnqueuedAt,
		},
	})
}

// ---------------------------------------------------------------------------
// Phase: upgrade (candidate on the retained volume). F1 assertions 1-7.
// ---------------------------------------------------------------------------

func TestLifecycleUpgradeToCandidate(t *testing.T) {
	ctx := t.Context()
	matrix := loadMatrix(t)
	fx := loadFixture(t)
	c := newClient(t)

	// Assertion 1 --------------------------------------------------------
	runCase(t, "assert1-candidate-healthy-and-migrated", func(t *testing.T) {
		require.NoError(t, c.awaitHealthy(ctx, healthDeadline),
			"the candidate never reached /health on the retained volume")

		logPath := filepath.Join(artifactsDir(t), "logs", "candidate.log")
		logs, err := os.ReadFile(logPath)
		if err != nil {
			blockf(t, "assert1-candidate-healthy-and-migrated",
				"the host controller did not capture %s, so `migrating database` cannot be observed: %v", logPath, err)
		}
		require.Containsf(t, string(logs), "migrating database",
			"the candidate's log does not contain the migration line from cmd/start/start.go")

		var state struct {
			Status     string `json:"status"`
			ExitCode   int    `json:"exit_code"`
			Restarts   int    `json:"restart_count"`
			OOMKilled  bool   `json:"oom_killed"`
			StartedAt  string `json:"started_at"`
			FinishedAt string `json:"finished_at"`
		}
		if !readJSON(t, filepath.Join("observations", "candidate-state.json"), &state) {
			blockf(t, "assert1-candidate-healthy-and-migrated",
				"the host controller did not record the candidate container's state; a non-zero exit cannot be excluded")
		}
		require.Equalf(t, "running", state.Status, "the candidate container is %q, not running", state.Status)
		require.Zerof(t, state.ExitCode, "the candidate container reported exit code %d", state.ExitCode)
		require.Zerof(t, state.Restarts, "the candidate container restarted %d times", state.Restarts)
	})

	// Assertion 2 --------------------------------------------------------
	runCase(t, "assert2-schema-migrated-additively", func(t *testing.T) {
		after, err := c.schema(ctx)
		require.NoError(t, err, "GET /v1/database/schema on the candidate")
		writeJSON(t, "candidate-schema.json", after)

		before := fx.Schema.index()
		now := after.index()

		// Nothing the previous release had may disappear. AutoMigrate never
		// drops a column, so a missing one is a real regression, not drift.
		var missingTables, missingColumns []string
		for table, cols := range before {
			nowCols, ok := now[table]
			if !ok {
				missingTables = append(missingTables, table)
				continue
			}
			for col := range cols {
				if !nowCols[col] {
					missingColumns = append(missingColumns, table+"."+col)
				}
			}
		}
		sort.Strings(missingTables)
		sort.Strings(missingColumns)
		require.Emptyf(t, missingTables, "tables present at %s are missing after the upgrade: %v",
			matrix.Previous.Release, missingTables)
		require.Emptyf(t, missingColumns, "columns present at %s are missing after the upgrade: %v",
			matrix.Previous.Release, missingColumns)

		// Every pinned addition must be present on EXACTLY the table named.
		for _, table := range matrix.Standalone.ExpectedTableAdditions {
			require.Containsf(t, now, table, "pinned table addition %q is absent after migration", table)
		}
		for _, add := range matrix.Standalone.ExpectedColumnAdditions {
			cols, ok := now[add.Table]
			require.Truef(t, ok, "pinned column addition %s.%s: table %s does not exist", add.Table, add.Column, add.Table)
			require.Truef(t, cols[add.Column], "pinned column addition %s.%s is absent after migration", add.Table, add.Column)
		}

		// Additions beyond the pinned list are RECORDED, not failed: this
		// runner is not in CI until G6 and an "exactly these" check would rot
		// red on the next model addition.
		pinnedTables := map[string]bool{}
		for _, table := range matrix.Standalone.ExpectedTableAdditions {
			pinnedTables[table] = true
		}
		pinnedColumns := map[string]bool{}
		for _, add := range matrix.Standalone.ExpectedColumnAdditions {
			pinnedColumns[add.Table+"."+add.Column] = true
		}
		var extraTables, extraColumns []string
		for table, cols := range now {
			beforeCols, existed := before[table]
			if !existed {
				if !pinnedTables[table] {
					extraTables = append(extraTables, table)
				}
				continue
			}
			for col := range cols {
				if beforeCols[col] {
					continue
				}
				if !pinnedColumns[table+"."+col] {
					extraColumns = append(extraColumns, table+"."+col)
				}
			}
		}
		sort.Strings(extraTables)
		sort.Strings(extraColumns)
		writeJSON(t, filepath.Join("observations", "schema-delta.json"), map[string]any{
			"previous_release":          matrix.Previous.Release,
			"previous_table_count":      len(fx.Schema.Tables),
			"candidate_table_count":     len(after.Tables),
			"pinned_table_additions":    matrix.Standalone.ExpectedTableAdditions,
			"pinned_column_additions":   matrix.Standalone.ExpectedColumnAdditions,
			"unpinned_table_additions":  extraTables,
			"unpinned_column_additions": extraColumns,
			"dropped_tables":            missingTables,
			"dropped_columns":           missingColumns,
		})
		if len(extraTables) > 0 || len(extraColumns) > 0 {
			t.Logf("RECORDED (not a failure): additions beyond the pinned delta — tables %v, columns %v",
				extraTables, extraColumns)
		}
	})

	// Assertion 3 --------------------------------------------------------
	runCase(t, "assert3-recorded-identities-readable", func(t *testing.T) {
		for _, alias := range []string{fx.Jobs["history"].Alias, fx.Jobs["queue"].Alias} {
			id, err := c.jobIDByAlias(ctx, alias)
			require.NoErrorf(t, err, "job %s is not readable after the upgrade", alias)
			require.Equalf(t, jobIDFor(fx, alias), id, "job %s changed id across the upgrade", alias)
		}
		assertRunUnchanged(t, ctx, c, fx.SucceededRun, "succeeded")
		assertRunUnchanged(t, ctx, c, fx.FailedRun, "failed")
	})

	// Assertion 4 --------------------------------------------------------
	runCase(t, "assert4-event-replay-from-explicit-cursor", func(t *testing.T) {
		for _, recorded := range []runFixture{fx.SucceededRun, fx.FailedRun} {
			tuples, err := readEventBacklog(ctx, c, recorded.ID, recorded.ResumeCursor)
			require.NoErrorf(t, err, "replaying events for run %s", recorded.ID)
			got := map[string]bool{}
			for _, tuple := range tuples {
				got[tuple.key()] = true
			}
			var missing []string
			for _, want := range recorded.Events {
				if want.Sequence <= recorded.ResumeCursor {
					continue
				}
				if !got[want.key()] {
					missing = append(missing, want.key())
				}
			}
			require.Emptyf(t, missing,
				"run %s: pre-upgrade events above cursor %d were not replayed after the upgrade: %v "+
					"(duplicates and extras are legal; this is a set-containment check, never a gap-free or "+
					"high-water-mark check)",
				recorded.ID, recorded.ResumeCursor, missing)
			t.Logf("run %s: replayed %d tuples from cursor %d, all %d recorded tuples present",
				recorded.ID, len(tuples), recorded.ResumeCursor, len(recorded.Events))
		}
	})

	// Assertion 5 --------------------------------------------------------
	runCase(t, "assert5-queued-row-reaches-a-started-run", func(t *testing.T) {
		queueJob := fx.Jobs["queue"]
		stoppedAt := mustEnv(t, "CAESIUM_LIFECYCLE_PREVIOUS_STOPPED_AT")
		previousStop, err := time.Parse(time.RFC3339Nano, stoppedAt)
		require.NoErrorf(t, err, "the host controller recorded an unparseable stop time %q", stoppedAt)

		// The candidate's dequeuer is the first thing that can act on the row,
		// so the run it produces must have started AFTER the previous release
		// stopped. Existence alone would not distinguish it from a stale claim.
		// GET /v1/jobs/:id/runs is a summary projection with no `tasks` array,
		// so the task rows are read back from the single-run endpoint.
		var startedID string
		deadline := time.Now().Add(runDeadline)
		for time.Now().Before(deadline) {
			runs, err := c.runs(ctx, queueJob.ID)
			require.NoError(t, err)
			for _, r := range runs {
				if r.Params["TOKEN"] == fx.QueueToken {
					startedID = r.ID
					break
				}
			}
			if startedID != "" {
				break
			}
			time.Sleep(pollInterval)
		}
		require.NotEmptyf(t, startedID,
			"the pre-upgrade queued row (%s, token %q) never became a run on the candidate",
			fx.QueuedRow.ID, fx.QueueToken)

		started, err := c.awaitRunStatus(ctx, queueJob.ID, startedID,
			func(r apiRun) bool { return len(r.Tasks) > 0 }, runDeadline)
		require.NoErrorf(t, err,
			"the dequeued run %s never registered a task row, so it never actually started", startedID)
		require.Equalf(t, fx.QueueToken, started.Params["TOKEN"],
			"run %s does not carry the queued row's params", startedID)

		startedAt, err := time.Parse(time.RFC3339Nano, started.StartedAt)
		require.NoErrorf(t, err, "run %s has an unparseable started_at %q", started.ID, started.StartedAt)
		require.Truef(t, startedAt.After(previousStop),
			"the dequeued run %s started at %s, which is not after the previous release stopped at %s; "+
				"it was not the candidate that started it", started.ID, started.StartedAt, stoppedAt)

		final, err := c.awaitRunStatus(ctx, queueJob.ID, started.ID,
			func(r apiRun) bool { return r.Status == "running" || isTerminal(r.Status) }, runDeadline)
		require.NoError(t, err)

		after, err := c.queue(ctx, queueJob.ID)
		require.NoError(t, err)
		for _, row := range after {
			require.NotEqualf(t, fx.QueuedRow.ID, row.ID,
				"run_queue row %s is still queued after it was dequeued into run %s", row.ID, started.ID)
		}
		writeJSON(t, filepath.Join("observations", "queued-work.json"), map[string]any{
			"queued_row_id":          fx.QueuedRow.ID,
			"queued_row_enqueued_at": fx.QueuedRow.EnqueuedAt,
			"queued_row_claim_state": fx.QueuedRow.ClaimState,
			"queued_token":           fx.QueueToken,
			"previous_stopped_at":    stoppedAt,
			"started_run_id":         started.ID,
			"started_run_started_at": started.StartedAt,
			"final_status":           final.Status,
			"task_row_count":         len(final.Tasks),
		})
		t.Logf("run_queue row %s was dequeued by the candidate into run %s (started %s, status %q, %d task rows)",
			fx.QueuedRow.ID, started.ID, started.StartedAt, final.Status, len(final.Tasks))
	})

	runCase(t, "assert6-export-relints-and-diffs-clean", func(t *testing.T) {
		exportDir := filepath.Join(artifactsDir(t), "defs", "exported")
		require.NoError(t, os.MkdirAll(exportDir, 0o755))
		for key, job := range fx.Jobs {
			res := requireCLI(t, "job", "export", job.Alias, "--server", c.base)
			require.NotEmptyf(t, strings.TrimSpace(res.Stdout),
				"job export %s wrote nothing to stdout after the upgrade", job.Alias)
			path := filepath.Join(exportDir, key+".job.yaml")
			require.NoError(t, os.WriteFile(path, []byte(res.Stdout), 0o644))
		}
		requireCLI(t, "job", "lint", "--path", exportDir)
		diff := runCLI(t, "job", "diff", "--path", exportDir, "--server", c.base, "--json")
		require.Equalf(t, 0, diff.ExitCode,
			"job diff of the exported manifests reported in-scope changes\nstdout:\n%s\nstderr:\n%s",
			diff.Stdout, diff.Stderr)
		var parsed map[string]any
		require.NoErrorf(t, json.Unmarshal([]byte(diff.Stdout), &parsed),
			"job diff --json stdout is not parseable JSON; log lines are leaking into stdout:\n%s", diff.Stdout)
		require.NoError(t, os.WriteFile(filepath.Join(artifactsDir(t), "job-diff.json"), []byte(diff.Stdout), 0o644))
	})

	// Assertion 7 --------------------------------------------------------
	runCase(t, "assert7-serving-build-is-the-candidate", func(t *testing.T) {
		var identity struct {
			ExpectedImageID string `json:"expected_image_id"`
			ObservedImageID string `json:"observed_image_id"`
			ImageRef        string `json:"image_ref"`
		}
		if !readJSON(t, filepath.Join("observations", "candidate-identity.json"), &identity) {
			blockf(t, "assert7-serving-build-is-the-candidate",
				"the host controller did not record the candidate container's image identity")
		}
		require.NotEmpty(t, identity.ExpectedImageID, "the built candidate image ID was not recorded")
		require.Equalf(t, identity.ExpectedImageID, identity.ObservedImageID,
			"the serving container's image ID %s is not the built candidate %s",
			identity.ObservedImageID, identity.ExpectedImageID)

		features, err := c.features(ctx)
		require.NoError(t, err)
		_, present := features["data_assertions_enabled"]
		require.Truef(t, present,
			"GET /v1/system/features has no data_assertions_enabled key, which the candidate has and %s lacks; "+
				"the behavioural cross-check of build identity failed (features: %v)", matrix.Previous.Release, features)
	})

	// Retained in-flight work: recorded, not judged. A run that was executing
	// when the previous release stopped is not finalized by the candidate — local
	// execution mode has no restart recovery — so its post-upgrade state is
	// evidence about retained state, not a pass/fail contract.
	func() {
		start := time.Now()
		got, err := c.run(ctx, fx.InFlightRun.JobID, fx.InFlightRun.ID)
		if err != nil {
			blockf(t, "recorded-in-flight-run-after-upgrade",
				"the run that was in flight at shutdown (%s) is not readable after the upgrade: %v",
				fx.InFlightRun.ID, err)
		}
		writeCase(t, caseRecord{
			Name:            "recorded-in-flight-run-after-upgrade",
			Status:          statusRecorded,
			DurationSeconds: time.Since(start).Seconds(),
			Detail: fmt.Sprintf(
				"run %s was %q when the previous release stopped and is %q on the candidate (error %q); "+
					"nothing in local execution mode finalizes a run whose process exited",
				got.ID, fx.InFlightRun.Status, got.Status, got.Error),
			Observations: map[string]any{
				"run_id":                got.ID,
				"status_before_restart": fx.InFlightRun.Status,
				"status_after_upgrade":  got.Status,
				"task_statuses_before":  fx.InFlightRun.Tasks,
				"task_row_count_after":  len(got.Tasks),
				"no_expectation_set":    true,
			},
		})
	}()
}

func jobIDFor(fx fixture, alias string) string {
	for _, job := range fx.Jobs {
		if job.Alias == alias {
			return job.ID
		}
	}
	return ""
}

func assertRunUnchanged(t *testing.T, ctx context.Context, c *client, want runFixture, label string) {
	t.Helper()
	got, err := c.run(ctx, want.JobID, want.ID)
	require.NoErrorf(t, err, "the %s run %s is not readable after the upgrade", label, want.ID)
	require.Equalf(t, want.Status, got.Status, "the %s run %s changed status across the upgrade", label, want.ID)
	require.Equalf(t, want.Error, got.Error, "the %s run %s changed its recorded error across the upgrade", label, want.ID)
	require.Equalf(t, want.StartedAt, got.StartedAt, "the %s run %s changed started_at across the upgrade", label, want.ID)
	require.Equalf(t, want.CompletedAt, got.CompletedAt, "the %s run %s changed completed_at across the upgrade", label, want.ID)
	require.Equalf(t, want.CreatedAt, got.CreatedAt, "the %s run %s changed created_at across the upgrade", label, want.ID)
	require.Equalf(t, want.Params, got.Params, "the %s run %s changed its params across the upgrade", label, want.ID)

	gotTasks := got.fixture().Tasks
	require.Lenf(t, gotTasks, len(want.Tasks), "the %s run %s changed its task-run count across the upgrade", label, want.ID)
	for i := range want.Tasks {
		require.Equalf(t, want.Tasks[i], gotTasks[i],
			"the %s run %s task-run %s changed across the upgrade", label, want.ID, want.Tasks[i].ID)
	}
}

// ---------------------------------------------------------------------------
// Phase: readdress (PR #536's supported re-address contract).
// ---------------------------------------------------------------------------

// TestLifecycleCandidateAddressChange exercises the transition F1 recorded as
// UNSUPPORTED and PR #536 (35bced63, closes #493) made supported for the
// candidate: a retained data directory started at a different
// CAESIUM_NODE_ADDRESS. The candidate rewrites info.yaml and the discovery
// cache, and for a genuine sole member recovers the local raft configuration
// after first checking that no peer answers. F1's "expect exit 1" applies to
// the PREVIOUS release only, which TestLifecycleTransitionOutcomes covers.
func TestLifecycleCandidateAddressChange(t *testing.T) {
	ctx := t.Context()
	matrix := loadMatrix(t)
	fx := loadFixture(t)
	c := newClient(t)
	want := envOr("CAESIUM_LIFECYCLE_EXPECT_NODE_ADDRESS", matrix.Standalone.AlternateNodeAddress)

	runCase(t, "supported-candidate-readdress", func(t *testing.T) {
		require.NoErrorf(t, c.awaitHealthy(ctx, healthDeadline),
			"PR #536's contract is that the candidate starts on a retained volume at a new node address (%s); "+
				"it never reached /health. If this is a product regression, it is a defect, not a test expectation to relax",
			want)

		dataDir := mustEnv(t, "CAESIUM_LIFECYCLE_DATA_DIR")
		infoPath := filepath.Join(dataDir, "info.yaml")
		raw, err := os.ReadFile(infoPath)
		if err != nil {
			blockf(t, "supported-candidate-readdress",
				"the retained data directory is not readable at %s: %v", infoPath, err)
		}
		var info struct {
			ID      uint64 `yaml:"ID"`
			Address string `yaml:"Address"`
			Role    int    `yaml:"Role"`
		}
		require.NoErrorf(t, yaml.Unmarshal(raw, &info), "info.yaml is not parseable: %s", string(raw))
		require.Equalf(t, want, info.Address,
			"info.yaml still records %q after the candidate started at %q; PR #536's reconciliation did not land",
			info.Address, want)
		t.Logf("info.yaml now records node %d at %s", info.ID, info.Address)

		// Every recorded identity must still be readable at the new address.
		assertRunUnchanged(t, ctx, c, fx.SucceededRun, "succeeded")
		assertRunUnchanged(t, ctx, c, fx.FailedRun, "failed")
		// The predecessor already terminalized (assertion at seed time: it
		// succeeded) before the previous release ever stopped, so — like the
		// succeeded/failed history — its terminal fields must be byte-identical
		// at the new address.
		assertRunUnchanged(t, ctx, c, fx.Predecessor, "predecessor")
		for _, job := range fx.Jobs {
			id, err := c.jobIDByAlias(ctx, job.Alias)
			require.NoErrorf(t, err, "job %s is not readable after the address change", job.Alias)
			require.Equal(t, job.ID, id)
		}

		// The in-flight run is NOT asserted to have any particular status: local
		// execution mode has no restart recovery, so it can legitimately still be
		// "running" after a server stop (#553), and now it has also survived a
		// second restart at a different node address. What must hold regardless
		// of outcome is that the run and its recorded task rows are still
		// readable and identifiable — nothing about the address change may make
		// pre-upgrade state unreachable.
		inflight, err := c.run(ctx, fx.InFlightRun.JobID, fx.InFlightRun.ID)
		require.NoErrorf(t, err, "the in-flight run %s is not readable after the address change", fx.InFlightRun.ID)
		wantTaskIDs := make([]string, len(fx.InFlightRun.Tasks))
		for i, tr := range fx.InFlightRun.Tasks {
			wantTaskIDs[i] = tr.ID
		}
		gotTaskIDs := make([]string, len(inflight.Tasks))
		for i, tr := range inflight.Tasks {
			gotTaskIDs[i] = tr.ID
		}
		require.ElementsMatchf(t, wantTaskIDs, gotTaskIDs,
			"the in-flight run %s's recorded task identities changed across the address change (before: %v, after: %v)",
			fx.InFlightRun.ID, wantTaskIDs, gotTaskIDs)
		t.Logf("in-flight run %s is %q after the address change (no outcome expected; see #553)",
			inflight.ID, inflight.Status)

		writeJSON(t, filepath.Join("observations", "readdress.json"), map[string]any{
			"expected_address":       want,
			"info_yaml_id":           info.ID,
			"info_yaml_addr":         info.Address,
			"pr":                     "#536 (35bced63)",
			"predecessor_run_id":     fx.Predecessor.ID,
			"in_flight_run_id":       inflight.ID,
			"in_flight_status_after": inflight.Status,
		})
	})
}

// ---------------------------------------------------------------------------
// Phase: probe — record one server's observable state without judging it.
// ---------------------------------------------------------------------------

type probeRecord struct {
	Name              string         `json:"name"`
	BaseURL           string         `json:"base_url"`
	Healthy           bool           `json:"healthy"`
	HealthError       string         `json:"health_error,omitempty"`
	Features          map[string]any `json:"features,omitempty"`
	SchemaTableCount  int            `json:"schema_table_count"`
	SchemaError       string         `json:"schema_error,omitempty"`
	IdentitiesRead    map[string]any `json:"identities_read"`
	IdentitiesMissing []string       `json:"identities_missing"`
	// IdentitiesChanged lists a job or task UUID that resolved to something
	// OTHER than what the fixture recorded — never a run's status, which is
	// explicitly not judged here (the in-flight run in particular has no
	// expected outcome; see TestLifecycleTransitionOutcomes). An empty
	// IdentitiesMissing is not by itself proof every identity read back
	// correctly; this is the field that would catch a UUID silently changing
	// underneath an unsupported transition.
	IdentitiesChanged []string `json:"identities_changed"`
	ObservedAt        string   `json:"observed_at"`
}

// sameIDSet reports whether got and want contain the same set of IDs,
// ignoring order. Used to compare recorded task-run identities across an
// unsupported transition without asserting anything about their status.
func sameIDSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// TestLifecycleProbe records what a server started by the host controller can
// actually serve. It never judges the outcome — TestLifecycleTransitionOutcomes
// does — but it MUST be able to write its record, because a case that cannot be
// observed is blocked, not passed.
func TestLifecycleProbe(t *testing.T) {
	ctx := t.Context()
	name := mustEnv(t, "CAESIUM_LIFECYCLE_PROBE_NAME")
	c := newClient(t)
	rec := probeRecord{
		Name:              name,
		BaseURL:           c.base,
		IdentitiesRead:    map[string]any{},
		IdentitiesMissing: []string{},
		IdentitiesChanged: []string{},
		ObservedAt:        time.Now().UTC().Format(time.RFC3339),
	}

	deadline := 90 * time.Second
	if raw := envOr("CAESIUM_LIFECYCLE_PROBE_DEADLINE_SECONDS", ""); raw != "" {
		seconds, err := strconv.Atoi(raw)
		require.NoErrorf(t, err, "CAESIUM_LIFECYCLE_PROBE_DEADLINE_SECONDS=%q is not a number", raw)
		deadline = time.Duration(seconds) * time.Second
	}
	if err := c.awaitHealthy(ctx, deadline); err != nil {
		rec.HealthError = err.Error()
	} else {
		rec.Healthy = true
	}

	if rec.Healthy {
		if features, err := c.features(ctx); err == nil {
			rec.Features = features
		}
		if snap, err := c.schema(ctx); err != nil {
			rec.SchemaError = err.Error()
		} else {
			rec.SchemaTableCount = len(snap.Tables)
		}

		var fx fixture
		if readJSON(t, "fixture.json", &fx) {
			// F1 defines the rollback/shard-count outcomes as "record whether
			// it starts and whether EVERY pre-upgrade identity reads back" —
			// all four runs the fixture recorded, not just the two terminal
			// history ones. The in-flight run's STATUS is still never judged
			// (#553); only its job/task UUIDs are compared here.
			for _, label := range []string{"succeeded_run", "failed_run", "predecessor_run", "in_flight_run"} {
				var recorded runFixture
				switch label {
				case "succeeded_run":
					recorded = fx.SucceededRun
				case "failed_run":
					recorded = fx.FailedRun
				case "predecessor_run":
					recorded = fx.Predecessor
				case "in_flight_run":
					recorded = fx.InFlightRun
				}
				got, err := c.run(ctx, recorded.JobID, recorded.ID)
				if err != nil {
					rec.IdentitiesMissing = append(rec.IdentitiesMissing, label+":"+recorded.ID+" ("+err.Error()+")")
					continue
				}
				wantTaskIDs := make([]string, len(recorded.Tasks))
				for i, tr := range recorded.Tasks {
					wantTaskIDs[i] = tr.ID
				}
				gotTaskIDs := make([]string, len(got.Tasks))
				for i, tr := range got.Tasks {
					gotTaskIDs[i] = tr.ID
				}
				tasksMatch := sameIDSet(gotTaskIDs, wantTaskIDs)
				rec.IdentitiesRead[label] = map[string]any{
					"run_id":                got.ID,
					"job_id":                got.JobID,
					"job_id_expected":       recorded.JobID,
					"status":                got.Status,
					"status_expected":       recorded.Status,
					"matches":               got.Status == recorded.Status,
					"task_row_ids":          gotTaskIDs,
					"task_row_ids_expected": wantTaskIDs,
					"task_identities_match": tasksMatch,
				}
				if got.JobID != recorded.JobID {
					rec.IdentitiesChanged = append(rec.IdentitiesChanged,
						fmt.Sprintf("%s: job_id %s -> %s", label, recorded.JobID, got.JobID))
				}
				if !tasksMatch {
					rec.IdentitiesChanged = append(rec.IdentitiesChanged,
						fmt.Sprintf("%s: task identities %v -> %v", label, wantTaskIDs, gotTaskIDs))
				}
			}
			for _, job := range fx.Jobs {
				id, err := c.jobIDByAlias(ctx, job.Alias)
				if err != nil {
					rec.IdentitiesMissing = append(rec.IdentitiesMissing, "job:"+job.Alias+" ("+err.Error()+")")
					continue
				}
				if id != job.ID {
					rec.IdentitiesChanged = append(rec.IdentitiesChanged,
						fmt.Sprintf("job:%s id %s -> %s", job.Alias, job.ID, id))
				}
			}
		}
	}

	sort.Strings(rec.IdentitiesMissing)
	sort.Strings(rec.IdentitiesChanged)
	writeJSON(t, filepath.Join("observations", "probe-"+sanitize(name)+".json"), rec)
	t.Logf("probe %s: healthy=%v tables=%d missing=%v changed=%v",
		name, rec.Healthy, rec.SchemaTableCount, rec.IdentitiesMissing, rec.IdentitiesChanged)
}

// ---------------------------------------------------------------------------
// Phase: outcomes — the failing transition plus the two recorded-outcome cases.
// ---------------------------------------------------------------------------

type containerOutcome struct {
	Name       string `json:"name"`
	Image      string `json:"image"`
	Volume     string `json:"volume"`
	Env        string `json:"env"`
	ExitCode   int    `json:"exit_code"`
	Status     string `json:"status"`
	Started    bool   `json:"started"`
	LogTail    string `json:"log_tail"`
	ObservedAt string `json:"observed_at"`

	// Observation completeness, written by the host controller. An outcome the
	// controller could not read is NOT an outcome: `status: unknown` /
	// `exit_code: -1` / a swallowed `docker logs` failure describe the harness,
	// not the product, and a recorded-outcome case built on one is inconclusive.
	ObservationComplete bool     `json:"observation_complete"`
	ObservationErrors   []string `json:"observation_errors"`
	LogCaptured         bool     `json:"log_captured"`
}

// validateContainerOutcome reports why a recorded outcome cannot be believed.
// An empty result means the host controller actually observed the container.
//
// This is the guard behind F1 ruling 5: a recorded-outcome case carries no
// pre-judged expectation, but "could not run or could not be observed" is
// inconclusive — blocked, which fails the qualification — never a pass.
func validateContainerOutcome(o containerOutcome) []string {
	var problems []string
	if !o.ObservationComplete {
		detail := strings.Join(o.ObservationErrors, "; ")
		if detail == "" {
			detail = "the host controller did not mark the observation complete"
		}
		problems = append(problems, "incomplete observation: "+detail)
	}
	switch strings.TrimSpace(o.Status) {
	case "":
		problems = append(problems, "no container status was recorded")
	case "unknown":
		problems = append(problems, `container status is "unknown": docker inspect did not answer`)
	}
	if o.ExitCode < 0 {
		problems = append(problems,
			fmt.Sprintf("exit_code %d is a sentinel, not an observed exit status", o.ExitCode))
	}
	if !o.LogCaptured {
		problems = append(problems, "the container log was not captured, so no fatal can be confirmed or ruled out")
	}
	if strings.TrimSpace(o.ObservedAt) == "" {
		problems = append(problems, "the observation carries no timestamp")
	}
	return problems
}

// requireObservable blocks the named case unless the outcome was fully observed.
func requireObservable(t *testing.T, name string, o containerOutcome) {
	t.Helper()
	if problems := validateContainerOutcome(o); len(problems) > 0 {
		blockf(t, name, "the host controller could not observe %q: %s",
			o.Name, strings.Join(problems, "; "))
	}
}

// TestLifecycleObservationValidation is the harness's own self-check: it proves
// that an unobservable recorded outcome is rejected rather than reported. It
// touches no container and no server, and the host controller runs it as its
// own phase so the guard is exercised on every qualification.
func TestLifecycleObservationValidation(t *testing.T) {
	start := time.Now()
	complete := containerOutcome{
		Name: "fixture", Image: "caesiumcloud/caesium:v0.1.0", Volume: "vol",
		Status: "exited", ExitCode: 1, LogTail: "fatal: address in info.yaml does not match",
		ObservedAt: "2026-09-18T00:00:00Z", ObservationComplete: true, LogCaptured: true,
	}
	unknown := complete
	unknown.Status = "unknown"
	unknown.ExitCode = -1
	unknown.LogTail = ""
	unknown.LogCaptured = false
	unknown.ObservationComplete = false
	unknown.ObservationErrors = []string{"docker inspect failed", "docker logs failed"}

	noLogs := complete
	noLogs.LogCaptured = false
	noLogs.LogTail = ""

	sentinelExit := complete
	sentinelExit.ExitCode = -1

	legacy := complete
	legacy.ObservationComplete = false
	legacy.ObservationErrors = nil

	noTimestamp := complete
	noTimestamp.ObservedAt = ""

	cases := []struct {
		name     string
		outcome  containerOutcome
		wantAny  bool
		wantWord string
	}{
		{name: "fully observed", outcome: complete},
		{name: "docker inspect and logs both failed", outcome: unknown, wantAny: true, wantWord: "unknown"},
		{name: "log capture swallowed", outcome: noLogs, wantAny: true, wantWord: "log was not captured"},
		{name: "sentinel exit code", outcome: sentinelExit, wantAny: true, wantWord: "sentinel"},
		{name: "record predates the completeness flag", outcome: legacy, wantAny: true, wantWord: "incomplete observation"},
		{name: "no observation timestamp", outcome: noTimestamp, wantAny: true, wantWord: "no timestamp"},
	}

	ok := true
	for _, tc := range cases {
		if !t.Run(tc.name, func(t *testing.T) {
			problems := validateContainerOutcome(tc.outcome)
			if !tc.wantAny {
				require.Emptyf(t, problems, "a fully observed outcome must not be rejected: %v", problems)
				return
			}
			require.NotEmpty(t, problems, "an unobservable outcome must be rejected")
			require.Containsf(t, strings.Join(problems, " | "), tc.wantWord,
				"rejection reason does not name the defect: %v", problems)
		}) {
			ok = false
		}
	}

	status := statusPass
	if !ok {
		status = statusFail
	}
	writeCase(t, caseRecord{
		Name:            "observation-validation-self-check",
		Status:          status,
		DurationSeconds: time.Since(start).Seconds(),
		Detail: fmt.Sprintf("%d fixtures: a complete observation is accepted; unknown status, sentinel exit code, "+
			"a swallowed log capture, a missing completeness flag and a missing timestamp are each rejected, "+
			"so a recorded-outcome case built on one is blocked", len(cases)),
	})
}

func TestLifecycleTransitionOutcomes(t *testing.T) {
	// Required failing transition: the pinned previous release, on its own copy
	// of the volume, at a different node address. It characterizes the old
	// release, proves the harness can detect a failed start, and is exactly why
	// "upgrade first, re-address second" is the only supported order.
	runCase(t, "unsupported-previous-release-readdress", func(t *testing.T) {
		var outcome containerOutcome
		if !readJSON(t, filepath.Join("observations", "previous-readdress.json"), &outcome) {
			blockf(t, "unsupported-previous-release-readdress",
				"the host controller recorded no outcome for the previous release's re-address attempt")
		}
		requireObservable(t, "unsupported-previous-release-readdress", outcome)
		var probe probeRecord
		if !readJSON(t, filepath.Join("observations", "probe-previous-readdress.json"), &probe) {
			blockf(t, "unsupported-previous-release-readdress",
				"no health probe was recorded for the previous release's re-address attempt")
		}
		require.NotEqualf(t, 0, outcome.ExitCode,
			"the previous release exited 0 at a changed node address; it has no address reconciliation, "+
				"so this is not the documented behaviour (log tail: %s)", truncate([]byte(outcome.LogTail), 2048))
		require.Containsf(t, outcome.LogTail, "in info.yaml does not match",
			"the previous release did not report the go-dqlite address mismatch fatal (log tail: %s)",
			truncate([]byte(outcome.LogTail), 2048))
		require.Falsef(t, probe.Healthy,
			"the previous release answered /health at a changed node address despite exiting %d", outcome.ExitCode)
		t.Logf("previous release at a changed node address: exit %d, fatal observed, no healthy /health",
			outcome.ExitCode)
	})

	// Recorded-outcome cases. They must RUN and be OBSERVABLE; their outcome
	// never decides the qualification, because F1 promises nothing about them.
	for _, spec := range []struct {
		caseName string
		outcome  string
		probe    string
		note     string
	}{
		{
			caseName: "recorded-rollback-previous-release-on-migrated-volume",
			outcome:  "rollback.json",
			probe:    "probe-rollback.json",
			note: "F1 records restore-from-snapshot as the intended recovery and binary rollback as NOT " +
				"supported; AutoMigrate never removes what it added and no down-migration exists. " +
				"A start that succeeds here does not create a rollback guarantee.",
		},
		{
			caseName: "recorded-shard-count-change",
			outcome:  "shards.json",
			probe:    "probe-shards.json",
			note: "The shard count is frozen for the life of a data directory: a run's rows live in " +
				"fnv32a(run_id) % len(hot) and raising CAESIUM_DATABASE_SHARDS does not rebalance them. " +
				"A silently successful start that strands rows is a finding to file, not a pass.",
		},
	} {
		start := time.Now()
		name := spec.caseName
		var outcome containerOutcome
		var probe probeRecord
		if !readJSON(t, filepath.Join("observations", spec.outcome), &outcome) {
			blockf(t, name, "the host controller recorded no container outcome (%s); the case could not be run", spec.outcome)
		}
		// Ruling 5: no expectation is set for these, but an outcome that was
		// not actually observed is inconclusive, not recorded.
		requireObservable(t, name, outcome)
		if !readJSON(t, filepath.Join("observations", spec.probe), &probe) {
			blockf(t, name, "the host controller recorded no probe (%s); the case ran but could not be observed", spec.probe)
		}
		detail := fmt.Sprintf(
			"exit_code=%d status=%q healthy=%v schema_tables=%d identities_missing=%v identities_changed=%v",
			outcome.ExitCode, outcome.Status, probe.Healthy, probe.SchemaTableCount,
			probe.IdentitiesMissing, probe.IdentitiesChanged)
		writeCase(t, caseRecord{
			Name:            name,
			Status:          statusRecorded,
			DurationSeconds: time.Since(start).Seconds(),
			Detail:          detail + " | " + spec.note,
			Observations: map[string]any{
				"container":          outcome,
				"probe":              probe,
				"no_expectation_set": true,
			},
		})
		t.Logf("RECORDED OUTCOME %s: %s", name, detail)
	}
}

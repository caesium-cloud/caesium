// Package load provides a synthetic load harness for measuring Caesium's
// per-shard write throughput and per-write-category distribution.
//
// The harness generates parameterized DAG workloads (fan-out width, depth,
// task duration) against a running Caesium server, waits for completion, and
// samples prometheus metrics throughout.
//
// Usage:
//
//	go run ./test/load [flags]
//
// The harness is not an integration test — it runs against an already-started
// Caesium server reachable at CAESIUM_LOAD_SERVER (default http://127.0.0.1:8080).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	serverURL    string
	jobCount     int
	fanOut       int
	depth        int
	taskDuration time.Duration
	concurrency  int
	sampleRate   time.Duration
	outputFile   string
	apiKey       string
	engine       string // docker | kubernetes | podman; default docker
}

func defaultConfig() config {
	return config{
		serverURL:    envOrDefault("CAESIUM_LOAD_SERVER", "http://127.0.0.1:8080"),
		jobCount:     envIntOrDefault("CAESIUM_LOAD_JOBS", 10),
		fanOut:       envIntOrDefault("CAESIUM_LOAD_FAN_OUT", 4),
		depth:        envIntOrDefault("CAESIUM_LOAD_DEPTH", 3),
		taskDuration: envDurOrDefault("CAESIUM_LOAD_TASK_DURATION", 1*time.Second),
		concurrency:  envIntOrDefault("CAESIUM_LOAD_CONCURRENCY", 1),
		sampleRate:   envDurOrDefault("CAESIUM_LOAD_SAMPLE_RATE", 5*time.Second),
		outputFile:   envOrDefault("CAESIUM_LOAD_OUTPUT", ""),
		apiKey:       envOrDefault("CAESIUM_MANUAL_TRIGGER_API_KEY", ""),
		engine:       envOrDefault("CAESIUM_LOAD_ENGINE", "docker"),
	}
}

// ---------------------------------------------------------------------------
// Job definition shapes
// ---------------------------------------------------------------------------

// jobDef is the minimal YAML-equivalent payload we send to the server.
type jobDef struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   jobMeta    `json:"metadata"`
	Trigger    triggerDef `json:"trigger"`
	Steps      []stepDef  `json:"steps"`
}

type jobMeta struct {
	Alias string `json:"alias"`
}

type triggerDef struct {
	Type          string         `json:"type"`
	Configuration map[string]any `json:"configuration"`
}

type stepDef struct {
	Name      string   `json:"name"`
	Engine    string   `json:"engine,omitempty"`
	Image     string   `json:"image"`
	Command   []string `json:"command,omitempty"`
	Next      []string `json:"next,omitempty"`
	DependsOn []string `json:"dependsOn,omitempty"`
}

// buildDAGSteps builds a width×depth fan-out DAG.
// Layer 0: one root task.
// Layers 1..depth-1: fanOut tasks each.
// Final layer: one join task that depends on all layer depth-1 tasks.
// All tasks sleep for taskDuration seconds and emit one caesium::output line.
func buildDAGSteps(fanOut, depth int, taskDuration time.Duration, engine string) []stepDef {
	// Use floating-point seconds so sub-second durations are honored;
	// busybox:1.36.1 sleep accepts decimal values. Floor at 1ms so we never
	// emit "sleep 0".
	sleepSec := taskDuration.Seconds()
	if sleepSec < 0.001 {
		sleepSec = 0.001
	}

	// Single-step fast path.
	if depth <= 1 || fanOut <= 1 {
		return []stepDef{
			{
				Name:    "task-root",
				Engine:  engine,
				Image:   "busybox:1.36.1",
				Command: []string{"sh", "-c", fmt.Sprintf("sleep %g && echo '##caesium::output {\"done\":\"1\"}'", sleepSec)},
			},
		}
	}

	var steps []stepDef

	// Root task.
	rootName := "task-root"
	rootNexts := make([]string, 0, fanOut)
	for w := 0; w < fanOut; w++ {
		rootNexts = append(rootNexts, fmt.Sprintf("task-l1-w%d", w))
	}
	steps = append(steps, stepDef{
		Name:    rootName,
		Engine:  engine,
		Image:   "busybox:1.36.1",
		Command: []string{"sh", "-c", fmt.Sprintf("sleep %g && echo '##caesium::output {\"step\":\"root\"}'", sleepSec)},
		Next:    rootNexts,
	})

	// Middle layers.
	for d := 1; d < depth-1; d++ {
		for w := 0; w < fanOut; w++ {
			name := fmt.Sprintf("task-l%d-w%d", d, w)
			nextName := fmt.Sprintf("task-l%d-w%d", d+1, w)
			var dependsOn []string
			if d == 1 {
				dependsOn = []string{rootName}
			} else {
				dependsOn = []string{fmt.Sprintf("task-l%d-w%d", d-1, w)}
			}
			steps = append(steps, stepDef{
				Name:      name,
				Engine:    engine,
				Image:     "busybox:1.36.1",
				Command:   []string{"sh", "-c", fmt.Sprintf("sleep %g && echo '##caesium::output {\"step\":\"%s\"}'", sleepSec, name)},
				DependsOn: dependsOn,
				Next:      []string{nextName},
			})
		}
	}

	// Final fan-in layer: one task per width lane collapsing into join.
	joinDeps := make([]string, 0, fanOut)
	lastLayerIdx := depth - 1
	for w := 0; w < fanOut; w++ {
		name := fmt.Sprintf("task-l%d-w%d", lastLayerIdx, w)
		joinDeps = append(joinDeps, name)
		var dependsOn []string
		if lastLayerIdx == 1 {
			dependsOn = []string{rootName}
		} else {
			dependsOn = []string{fmt.Sprintf("task-l%d-w%d", lastLayerIdx-1, w)}
		}
		steps = append(steps, stepDef{
			Name:      name,
			Engine:    engine,
			Image:     "busybox:1.36.1",
			Command:   []string{"sh", "-c", fmt.Sprintf("sleep %g && echo '##caesium::output {\"step\":\"%s\"}'", sleepSec, name)},
			DependsOn: dependsOn,
			Next:      []string{"task-join"},
		})
	}

	// Join task.
	steps = append(steps, stepDef{
		Name:      "task-join",
		Engine:    engine,
		Image:     "busybox:1.36.1",
		Command:   []string{"sh", "-c", fmt.Sprintf("sleep %g && echo '##caesium::output {\"done\":\"1\"}'", sleepSec)},
		DependsOn: joinDeps,
	})

	return steps
}

// ---------------------------------------------------------------------------
// Caesium REST client
// ---------------------------------------------------------------------------

type client struct {
	base   string
	apiKey string
	http   *http.Client
}

func newClient(base, apiKey string) *client {
	return &client{
		base:   strings.TrimRight(base, "/"),
		apiKey: apiKey,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *client) do(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	return c.http.Do(req)
}

// applyDefinitions sends a batch of job definitions in a single
// POST /v1/jobdefs/apply call. The apply response carries only counts
// ({applied, pruned}); callers resolve IDs via a subsequent listJobs call.
func (c *client) applyDefinitions(ctx context.Context, defs []jobDef) error {
	body := map[string]any{"definitions": defs}
	resp, err := c.do(ctx, http.MethodPost, "/v1/jobdefs/apply", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("apply %d definitions: HTTP %d: %s", len(defs), resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// listJobs returns a flat array of {id, alias} pairs for every job on the
// server. Used to build a one-shot alias→ID lookup table after a batch apply.
func (c *client) listJobs(ctx context.Context) ([]struct{ ID, Alias string }, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/jobs", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list jobs: HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	var entries []struct {
		ID    string `json:"id"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse list-jobs response: %w", err)
	}
	out := make([]struct{ ID, Alias string }, len(entries))
	for i, e := range entries {
		out[i] = struct{ ID, Alias string }{ID: e.ID, Alias: e.Alias}
	}
	return out, nil
}

// getRunStatus returns the status of a job run. The runs endpoint is scoped
// under the parent job: /v1/jobs/:job_id/runs/:run_id.
func (c *client) getRunStatus(ctx context.Context, jobID, runID string) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("get run %s: HTTP %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("parse run status: %w", err)
	}
	return result.Status, nil
}

// fetchMetrics returns the raw Prometheus text from /metrics.
func (c *client) fetchMetrics(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/metrics", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return string(raw), err
}

// ---------------------------------------------------------------------------
// Metrics parsing
// ---------------------------------------------------------------------------

// parseCounter extracts a float64 counter value from Prometheus text output
// for metrics matching the given name and labels. Labels is a map of key→value
// pairs that must all be present in the metric line.
func parseCounter(text, metricName string, labels map[string]string) float64 {
	var total float64
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if !strings.HasPrefix(line, metricName) {
			continue
		}
		// Reject prefix-only matches (e.g., `caesium_db_writes_total_bucket`
		// would otherwise match `caesium_db_writes_total`).
		if rest := line[len(metricName):]; len(rest) > 0 && rest[0] != '{' && rest[0] != ' ' && rest[0] != '\t' {
			continue
		}

		allMatch := true
		for k, v := range labels {
			needle := fmt.Sprintf(`%s="%s"`, k, v)
			if !strings.Contains(line, needle) {
				allMatch = false
				break
			}
		}
		if !allMatch {
			continue
		}

		// Value is the last token after the label set.
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		var val float64
		if _, err := fmt.Sscanf(parts[len(parts)-1], "%f", &val); err == nil {
			total += val
		}
	}
	return total
}

type metricSample struct {
	ts time.Time
	// Row counts (caesium_db_writes_total).
	taskRunInsert float64
	taskRunStatus float64
	eventInsert   float64
	leaseRenewal  float64
	callback      float64
	command       float64
	checkpoint    float64
	// Statement counts (caesium_db_statements_total). Same categories;
	// rows/statements is the batching factor.
	taskRunInsertStmts float64
	taskRunStatusStmts float64
	eventInsertStmts   float64
	leaseRenewalStmts  float64
	callbackStmts      float64
	commandStmts       float64
	checkpointStmts    float64
	dbBusyRetries      float64
	claimsTotal        float64
}

func sampleMetrics(ctx context.Context, c *client) (metricSample, error) {
	text, err := c.fetchMetrics(ctx)
	if err != nil {
		return metricSample{}, err
	}
	s := metricSample{ts: time.Now()}
	cat := func(name, category string) float64 {
		return parseCounter(text, name, map[string]string{"category": category})
	}
	s.taskRunInsert = cat("caesium_db_writes_total", "task_run_insert")
	s.taskRunStatus = cat("caesium_db_writes_total", "task_run_status")
	s.eventInsert = cat("caesium_db_writes_total", "event_insert")
	s.leaseRenewal = cat("caesium_db_writes_total", "lease_renewal")
	s.callback = cat("caesium_db_writes_total", "callback")
	s.command = cat("caesium_db_writes_total", "command")
	s.checkpoint = cat("caesium_db_writes_total", "checkpoint")
	s.taskRunInsertStmts = cat("caesium_db_statements_total", "task_run_insert")
	s.taskRunStatusStmts = cat("caesium_db_statements_total", "task_run_status")
	s.eventInsertStmts = cat("caesium_db_statements_total", "event_insert")
	s.leaseRenewalStmts = cat("caesium_db_statements_total", "lease_renewal")
	s.callbackStmts = cat("caesium_db_statements_total", "callback")
	s.commandStmts = cat("caesium_db_statements_total", "command")
	s.checkpointStmts = cat("caesium_db_statements_total", "checkpoint")
	s.dbBusyRetries = parseCounter(text, "caesium_db_busy_retries_total", nil)
	s.claimsTotal = parseCounter(text, "caesium_worker_claims_total", nil)
	return s, nil
}

// ---------------------------------------------------------------------------
// Run result
// ---------------------------------------------------------------------------

type runResult struct {
	runID      string
	alias      string
	startedAt  time.Time
	finishedAt time.Time
	status     string
	err        error
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

type harness struct {
	cfg    config
	client *client
}

func newHarness(cfg config) *harness {
	return &harness{
		cfg:    cfg,
		client: newClient(cfg.serverURL, cfg.apiKey),
	}
}

// waitForServer blocks until the server returns 200 on /health or ctx expires.
func (h *harness) waitForServer(ctx context.Context) error {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			resp, err := h.client.do(ctx, http.MethodGet, "/health", nil)
			if err == nil && resp.StatusCode == http.StatusOK {
				resp.Body.Close()
				return nil
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}
}

// appliedJob pairs a job's stable alias with its server-assigned UUID.
type appliedJob struct {
	alias string
	id    string
}

// applyJobs builds the synthetic job definitions, applies them in a single
// batched POST /v1/jobdefs/apply, then resolves all aliases to IDs in a single
// GET /v1/jobs. Both round-trips are O(1) in jobCount instead of O(N) per
// definition, which keeps harness setup time bounded for large workloads.
func (h *harness) applyJobs(ctx context.Context) ([]appliedJob, error) {
	cfg := h.cfg
	steps := buildDAGSteps(cfg.fanOut, cfg.depth, cfg.taskDuration, cfg.engine)

	defs := make([]jobDef, 0, cfg.jobCount)
	aliases := make([]string, 0, cfg.jobCount)
	for i := 0; i < cfg.jobCount; i++ {
		alias := fmt.Sprintf("load-test-job-%d", i)
		path := fmt.Sprintf("/load/%s", alias)
		defs = append(defs, jobDef{
			APIVersion: "v1",
			Kind:       "Job",
			Metadata:   jobMeta{Alias: alias},
			Trigger: triggerDef{
				Type:          "http",
				Configuration: map[string]any{"path": path},
			},
			Steps: steps,
		})
		aliases = append(aliases, alias)
	}

	if err := h.client.applyDefinitions(ctx, defs); err != nil {
		return nil, fmt.Errorf("apply %d definitions: %w", len(defs), err)
	}

	entries, err := h.client.listJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list jobs for ID resolution: %w", err)
	}
	idByAlias := make(map[string]string, len(entries))
	for _, e := range entries {
		idByAlias[e.Alias] = e.ID
	}

	results := make([]appliedJob, 0, cfg.jobCount)
	for _, alias := range aliases {
		jobID, ok := idByAlias[alias]
		if !ok {
			return nil, fmt.Errorf("alias %s missing from list-jobs response", alias)
		}
		results = append(results, appliedJob{alias: alias, id: jobID})
	}
	return results, nil
}

// run executes the full load harness and returns a report.
func (h *harness) run(ctx context.Context) (*report, error) {
	fmt.Println("Waiting for server to be ready...")
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := h.waitForServer(waitCtx); err != nil {
		cancel()
		return nil, fmt.Errorf("server not reachable: %w", err)
	}
	cancel()
	fmt.Printf("Server ready at %s\n", h.cfg.serverURL)

	fmt.Printf("Applying %d synthetic jobs (fan-out=%d, depth=%d, task-duration=%s)...\n",
		h.cfg.jobCount, h.cfg.fanOut, h.cfg.depth, h.cfg.taskDuration)
	jobs, err := h.applyJobs(ctx)
	if err != nil {
		return nil, fmt.Errorf("apply jobs: %w", err)
	}
	fmt.Printf("Applied %d jobs.\n", len(jobs))

	// Sample baseline metrics.
	baselineSample, err := sampleMetrics(ctx, h.client)
	if err != nil {
		return nil, fmt.Errorf("baseline metrics sample: %w", err)
	}

	startTime := time.Now()
	fmt.Printf("Triggering %d runs (concurrency=%d)...\n", h.cfg.jobCount, h.cfg.concurrency)

	var (
		resultsMu sync.Mutex
		results   []runResult
		pending   atomic.Int64
	)
	pending.Store(int64(h.cfg.jobCount))

	sem := make(chan struct{}, h.cfg.concurrency)

	for _, j := range jobs {
		j := j
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			rr := h.triggerAndWait(ctx, j.alias, j.id)
			resultsMu.Lock()
			results = append(results, rr)
			resultsMu.Unlock()
			pending.Add(-1)
		}()
	}

	// Periodic metric sampling.
	var samples []metricSample
	ticker := time.NewTicker(h.cfg.sampleRate)
	defer ticker.Stop()

	for pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			s, err := sampleMetrics(ctx, h.client)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warn: metrics sample failed: %v\n", err)
				continue
			}
			samples = append(samples, s)
			remaining := pending.Load()
			fmt.Printf("  [%s] %d runs remaining...\n", time.Since(startTime).Round(time.Second), remaining)
		}
	}

	// Drain sem.
	for i := 0; i < h.cfg.concurrency; i++ {
		sem <- struct{}{}
	}

	endSample, err := sampleMetrics(ctx, h.client)
	if err != nil {
		return nil, fmt.Errorf("final metrics sample: %w", err)
	}

	totalDuration := time.Since(startTime)

	return buildReport(h.cfg, results, baselineSample, endSample, samples, totalDuration), nil
}

// triggerAndWait fires a job run via its HTTP trigger and polls until terminal.
func (h *harness) triggerAndWait(ctx context.Context, alias, jobID string) runResult {
	rr := runResult{alias: alias, startedAt: time.Now()}

	runID, err := h.startRun(ctx, jobID)
	if err != nil {
		rr.err = fmt.Errorf("trigger %s: %w", alias, err)
		rr.finishedAt = time.Now()
		rr.status = "trigger_failed"
		return rr
	}
	rr.runID = runID

	// Poll until terminal.
	const pollInterval = 2 * time.Second
	const maxWait = 30 * time.Minute
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			rr.err = ctx.Err()
			rr.finishedAt = time.Now()
			rr.status = "canceled"
			return rr
		case <-time.After(pollInterval):
		}

		status, pollErr := h.client.getRunStatus(ctx, jobID, runID)
		if pollErr != nil {
			// Transient; keep polling.
			continue
		}
		switch status {
		case "succeeded", "failed":
			rr.status = status
			rr.finishedAt = time.Now()
			if status == "failed" {
				rr.err = errors.New("run ended with status: failed")
			}
			return rr
		}
	}

	rr.err = fmt.Errorf("run %s timed out after %s", runID, maxWait)
	rr.finishedAt = time.Now()
	rr.status = "timeout"
	return rr
}

// startRun POSTs to /v1/jobs/:id/run and returns the new run's ID. The webhook
// path (/v1/hooks/*) is not used because its 202 response carries no body,
// which would make per-run tracking impossible. POST /v1/jobs/:id/run returns
// the JobRun object including the run ID.
func (h *harness) startRun(ctx context.Context, jobID string) (string, error) {
	resp, err := h.client.do(ctx, http.MethodPost, "/v1/jobs/"+jobID+"/run", nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("run job %s: HTTP %d: %s", jobID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	raw, _ := io.ReadAll(resp.Body)
	var result struct {
		ID string `json:"id"`
	}
	if jErr := json.Unmarshal(raw, &result); jErr != nil {
		return "", fmt.Errorf("parse run response: %w", jErr)
	}
	if result.ID == "" {
		return "", fmt.Errorf("run job %s: response missing id", jobID)
	}
	return result.ID, nil
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

type report struct {
	cfg               config
	totalDuration     time.Duration
	runsSucceeded     int
	runsFailed        int
	runsTimeout       int
	runsTriggerFailed int

	// Delta row counts (end - baseline).
	deltaTaskRunInsert float64
	deltaTaskRunStatus float64
	deltaEventInsert   float64
	deltaLeaseRenewal  float64
	deltaCallback      float64
	deltaCommand       float64
	deltaCheckpoint    float64
	// Delta statement counts (end - baseline). Rows / statements is the
	// batching factor: > 1 means each statement touches multiple rows.
	deltaTaskRunInsertStmts float64
	deltaTaskRunStatusStmts float64
	deltaEventInsertStmts   float64
	deltaLeaseRenewalStmts  float64
	deltaCallbackStmts      float64
	deltaCommandStmts       float64
	deltaCheckpointStmts    float64
	deltaDBBusyRetries      float64
	deltaClaimsTotal        float64

	// Per-second rates during the run.
	peakTaskRunStatusPerSec float64
	peakEventInsertPerSec   float64
	peakLeaseRenewalPerSec  float64

	// Task latency (approximated from run duration and task count).
	totalTasks   int
	taskDuration time.Duration
	endToEndP50  time.Duration
	endToEndP99  time.Duration

	// Intermediate samples for rate computation.
	samples []metricSample
}

func buildReport(
	cfg config,
	results []runResult,
	baseline, end metricSample,
	samples []metricSample,
	totalDuration time.Duration,
) *report {
	r := &report{
		cfg:           cfg,
		totalDuration: totalDuration,
		samples:       samples,
	}

	// Tally run statuses and end-to-end durations.
	durations := make([]time.Duration, 0, len(results))
	for _, rr := range results {
		switch rr.status {
		case "succeeded":
			r.runsSucceeded++
		case "failed":
			r.runsFailed++
		case "timeout":
			r.runsTimeout++
		default:
			r.runsTriggerFailed++
		}
		if !rr.finishedAt.IsZero() && !rr.startedAt.IsZero() {
			durations = append(durations, rr.finishedAt.Sub(rr.startedAt))
		}
	}

	// End-to-end latency percentiles.
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if len(durations) > 0 {
		r.endToEndP50 = durations[len(durations)/2]
		r.endToEndP99 = durations[int(float64(len(durations))*0.99)]
	}

	// Delta row counts.
	r.deltaTaskRunInsert = end.taskRunInsert - baseline.taskRunInsert
	r.deltaTaskRunStatus = end.taskRunStatus - baseline.taskRunStatus
	r.deltaEventInsert = end.eventInsert - baseline.eventInsert
	r.deltaLeaseRenewal = end.leaseRenewal - baseline.leaseRenewal
	r.deltaCallback = end.callback - baseline.callback
	r.deltaCommand = end.command - baseline.command
	r.deltaCheckpoint = end.checkpoint - baseline.checkpoint
	// Delta statement counts.
	r.deltaTaskRunInsertStmts = end.taskRunInsertStmts - baseline.taskRunInsertStmts
	r.deltaTaskRunStatusStmts = end.taskRunStatusStmts - baseline.taskRunStatusStmts
	r.deltaEventInsertStmts = end.eventInsertStmts - baseline.eventInsertStmts
	r.deltaLeaseRenewalStmts = end.leaseRenewalStmts - baseline.leaseRenewalStmts
	r.deltaCallbackStmts = end.callbackStmts - baseline.callbackStmts
	r.deltaCommandStmts = end.commandStmts - baseline.commandStmts
	r.deltaCheckpointStmts = end.checkpointStmts - baseline.checkpointStmts
	r.deltaDBBusyRetries = end.dbBusyRetries - baseline.dbBusyRetries
	r.deltaClaimsTotal = end.claimsTotal - baseline.claimsTotal

	// Estimate total tasks: 1 root + fanOut*(depth-1) lanes + 1 join per run.
	tasksPerRun := 1 + cfg.fanOut*(cfg.depth-1) + 1
	if cfg.fanOut <= 1 || cfg.depth <= 1 {
		tasksPerRun = 1
	}
	r.totalTasks = cfg.jobCount * tasksPerRun
	r.taskDuration = cfg.taskDuration

	// Peak per-second rates from adjacent samples.
	for i := 1; i < len(samples); i++ {
		dt := samples[i].ts.Sub(samples[i-1].ts).Seconds()
		if dt <= 0 {
			continue
		}
		if rate := (samples[i].taskRunStatus - samples[i-1].taskRunStatus) / dt; rate > r.peakTaskRunStatusPerSec {
			r.peakTaskRunStatusPerSec = rate
		}
		if rate := (samples[i].eventInsert - samples[i-1].eventInsert) / dt; rate > r.peakEventInsertPerSec {
			r.peakEventInsertPerSec = rate
		}
		if rate := (samples[i].leaseRenewal - samples[i-1].leaseRenewal) / dt; rate > r.peakLeaseRenewalPerSec {
			r.peakLeaseRenewalPerSec = rate
		}
	}

	return r
}

func (r *report) dominantCategory() (string, float64) {
	categories := map[string]float64{
		"task_run_insert": r.deltaTaskRunInsert,
		"task_run_status": r.deltaTaskRunStatus,
		"event_insert":    r.deltaEventInsert,
		"lease_renewal":   r.deltaLeaseRenewal,
		"callback":        r.deltaCallback,
		"command":         r.deltaCommand,
		"checkpoint":      r.deltaCheckpoint,
	}
	var topCat string
	var topVal float64
	for cat, val := range categories {
		if val > topVal {
			topVal = val
			topCat = cat
		}
	}
	return topCat, topVal
}

func (r *report) totalWrites() float64 {
	return r.deltaTaskRunInsert + r.deltaTaskRunStatus + r.deltaEventInsert +
		r.deltaLeaseRenewal + r.deltaCallback + r.deltaCommand + r.deltaCheckpoint
}

func (r *report) print(w io.Writer) {
	domCat, domVal := r.dominantCategory()
	total := r.totalWrites()

	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "=== Caesium Load Harness — Baseline Report ===")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Server:              %s\n", r.cfg.serverURL)
	fmt.Fprintf(w, "Jobs:                %d\n", r.cfg.jobCount)
	fmt.Fprintf(w, "Fan-out width:       %d\n", r.cfg.fanOut)
	fmt.Fprintf(w, "DAG depth:           %d\n", r.cfg.depth)
	fmt.Fprintf(w, "Task duration:       %s\n", r.cfg.taskDuration)
	fmt.Fprintf(w, "Concurrency:         %d\n", r.cfg.concurrency)
	fmt.Fprintf(w, "Total run time:      %s\n", r.totalDuration.Round(time.Second))
	fmt.Fprintf(w, "Tasks estimated:     %d\n", r.totalTasks)
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Runs succeeded:      %d\n", r.runsSucceeded)
	fmt.Fprintf(w, "Runs failed:         %d\n", r.runsFailed)
	fmt.Fprintf(w, "Runs timeout:        %d\n", r.runsTimeout)
	fmt.Fprintf(w, "Runs trigger-failed: %d\n", r.runsTriggerFailed)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "--- DB Write Breakdown (delta over harness run) ---")
	fmt.Fprintln(w, "")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "Category\tRows\tStmts\tRows/Stmt\tShare\n")
	fmt.Fprintf(tw, "--------\t----\t-----\t---------\t-----\n")
	cats := []struct {
		name  string
		rows  float64
		stmts float64
	}{
		{"task_run_insert", r.deltaTaskRunInsert, r.deltaTaskRunInsertStmts},
		{"task_run_status", r.deltaTaskRunStatus, r.deltaTaskRunStatusStmts},
		{"event_insert", r.deltaEventInsert, r.deltaEventInsertStmts},
		{"lease_renewal", r.deltaLeaseRenewal, r.deltaLeaseRenewalStmts},
		{"callback", r.deltaCallback, r.deltaCallbackStmts},
		{"command", r.deltaCommand, r.deltaCommandStmts},
		{"checkpoint", r.deltaCheckpoint, r.deltaCheckpointStmts},
	}
	var totalStmts float64
	for _, c := range cats {
		totalStmts += c.stmts
		pct := 0.0
		if total > 0 {
			pct = c.rows / total * 100
		}
		batching := "—"
		if c.stmts > 0 {
			batching = fmt.Sprintf("%.1f", c.rows/c.stmts)
		}
		fmt.Fprintf(tw, "%s\t%.0f\t%.0f\t%s\t%.1f%%\n", c.name, c.rows, c.stmts, batching, pct)
	}
	overallBatching := "—"
	if totalStmts > 0 {
		overallBatching = fmt.Sprintf("%.1f", total/totalStmts)
	}
	fmt.Fprintf(tw, "TOTAL\t%.0f\t%.0f\t%s\t100%%\n", total, totalStmts, overallBatching)
	tw.Flush()
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Dominant category:   %s (%.0f writes, %.1f%% of total)\n",
		domCat, domVal, func() float64 {
			if total > 0 {
				return domVal / total * 100
			}
			return 0
		}())
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "--- Peak Write Rates (per second) ---")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "task_run_status/s:   %.1f\n", r.peakTaskRunStatusPerSec)
	fmt.Fprintf(w, "event_insert/s:      %.1f\n", r.peakEventInsertPerSec)
	fmt.Fprintf(w, "lease_renewal/s:     %.1f\n", r.peakLeaseRenewalPerSec)
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "--- Latency ---")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "End-to-end p50:      %s\n", r.endToEndP50.Round(time.Second))
	fmt.Fprintf(w, "End-to-end p99:      %s\n", r.endToEndP99.Round(time.Second))
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "--- Contention ---")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "DB busy retries:     %.0f\n", r.deltaDBBusyRetries)
	fmt.Fprintf(w, "Claims total:        %.0f\n", r.deltaClaimsTotal)
	if r.deltaClaimsTotal > 0 {
		fmt.Fprintf(w, "Writes per claim:    %.2f\n", total/r.deltaClaimsTotal)
	}
	fmt.Fprintln(w, "")
}

func (r *report) markdown() string {
	var b strings.Builder
	r.print(&b)
	return b.String()
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	cfg := defaultConfig()

	flag.StringVar(&cfg.serverURL, "server", cfg.serverURL, "Caesium server URL")
	flag.IntVar(&cfg.jobCount, "jobs", cfg.jobCount, "Number of synthetic jobs to create and run")
	flag.IntVar(&cfg.fanOut, "fan-out", cfg.fanOut, "DAG fan-out width")
	flag.IntVar(&cfg.depth, "depth", cfg.depth, "DAG depth (layers)")
	flag.DurationVar(&cfg.taskDuration, "task-duration", cfg.taskDuration, "How long each task sleeps (container execution time)")
	flag.IntVar(&cfg.concurrency, "concurrency", cfg.concurrency, "How many runs to trigger concurrently")
	flag.DurationVar(&cfg.sampleRate, "sample-rate", cfg.sampleRate, "How often to sample Prometheus metrics")
	flag.StringVar(&cfg.outputFile, "output", cfg.outputFile, "Write report to file (default: stdout only)")
	flag.StringVar(&cfg.apiKey, "api-key", cfg.apiKey, "API key for authenticated endpoints")
	flag.StringVar(&cfg.engine, "engine", cfg.engine, "Task engine: docker (default), kubernetes, or podman. Must match what the target Caesium deployment supports.")
	flag.Parse()

	h := newHarness(cfg)
	ctx := context.Background()
	rep, err := h.run(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load harness failed: %v\n", err)
		os.Exit(1)
	}

	rep.print(os.Stdout)

	if cfg.outputFile != "" {
		content := rep.markdown()
		if err := os.WriteFile(cfg.outputFile, []byte(content), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "failed to write output file: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Report written to %s\n", cfg.outputFile)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOrDefault(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envDurOrDefault(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

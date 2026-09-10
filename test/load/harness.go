// Package load provides a synthetic load harness for measuring Caesium's
// per-shard write throughput and per-write-category distribution.
//
// The harness generates parameterized DAG workloads (fan-out width, depth,
// task duration) against a running Caesium server, waits for completion, and
// samples prometheus metrics throughout.
//
// Usage:
//
//	just load-test # configure with CAESIUM_LOAD_* environment variables
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
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"os"
	"slices"
	"strings"
	"sync"
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
	jsonFile     string
	timeout      time.Duration
	image        string
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
		jsonFile:     envOrDefault("CAESIUM_LOAD_JSON_OUTPUT", ""),
		timeout:      envDurOrDefault("CAESIUM_LOAD_TIMEOUT", 30*time.Minute),
		image:        envOrDefault("CAESIUM_LOAD_IMAGE", "busybox:1.36.1"),
		apiKey:       envOrDefault("CAESIUM_MANUAL_TRIGGER_API_KEY", ""),
		engine:       envOrDefault("CAESIUM_LOAD_ENGINE", "docker"),
	}
}

// validate runs before allocation, ticker creation, or any network side effect.
func (c config) validate() error {
	u, err := url.Parse(c.serverURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("server must be an absolute http(s) URL without credentials, query, or fragment")
	}
	if c.jobCount <= 0 || c.fanOut <= 0 || c.depth <= 0 || c.concurrency <= 0 {
		return errors.New("jobs, fan-out, depth, and concurrency must be positive integers")
	}
	// Bound the generated workload as well as integer arithmetic before allocating.
	if c.jobCount > 100000 || c.fanOut > 100000 || c.depth > 100000 || int64(c.jobCount)*(int64(c.fanOut)*int64(c.depth-1)+2) > 1000000 {
		return errors.New("workload exceeds safety limit of 100000 jobs/dimension or 1000000 estimated tasks")
	}
	if c.taskDuration <= 0 || c.sampleRate <= 0 || c.timeout <= 0 {
		return errors.New("task-duration, sample-rate, and timeout must be positive durations")
	}
	if c.engine != "docker" && c.engine != "podman" && c.engine != "kubernetes" {
		return errors.New("unsupported engine")
	}
	if strings.TrimSpace(c.image) == "" {
		return errors.New("image must not be empty")
	}
	if c.outputFile != "" && c.outputFile == c.jsonFile {
		return errors.New("human and JSON output paths must differ")
	}
	return nil
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
	for w := range fanOut {
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
		for w := range fanOut {
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
	for w := range fanOut {
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

func (c *client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
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
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metrics: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	return string(raw), err
}

// ---------------------------------------------------------------------------
// Metrics parsing
// ---------------------------------------------------------------------------

type metricSample struct {
	counters map[string]float64
	ts       time.Time
	phase    string
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
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		return metricSample{}, fmt.Errorf("parse metrics: %w", err)
	}
	// This unlabelled counter is always registered, even before the first run.
	// Category vectors may legitimately be absent until their first write.
	required := families["caesium_db_busy_retries_total"]
	if required == nil || len(required.Metric) == 0 || required.Metric[0].Counter == nil {
		return metricSample{}, errors.New("metrics missing required caesium_db_busy_retries_total counter")
	}
	for name, family := range families {
		if !strings.HasPrefix(name, "caesium_db_") && name != "caesium_worker_claims_total" {
			continue
		}
		for _, m := range family.Metric {
			if m.Counter != nil && (math.IsNaN(m.Counter.GetValue()) || math.IsInf(m.Counter.GetValue(), 0) || m.Counter.GetValue() < 0) {
				return metricSample{}, fmt.Errorf("invalid counter %s", name)
			}
		}
	}
	s := metricSample{ts: time.Now(), counters: make(map[string]float64)}
	for name, family := range families {
		if name != "caesium_db_writes_total" && name != "caesium_db_statements_total" && name != "caesium_db_busy_retries_total" && name != "caesium_worker_claims_total" {
			continue
		}
		for _, m := range family.Metric {
			if m.Counter == nil {
				return metricSample{}, fmt.Errorf("expected counter %s", name)
			}
			labels := make([]string, 0, len(m.Label))
			for _, label := range m.Label {
				labels = append(labels, label.GetName()+"="+strconv.Quote(label.GetValue()))
			}
			slices.Sort(labels)
			s.counters[name+"{"+strings.Join(labels, ",")+"}"] = m.Counter.GetValue()
		}
	}
	counter := func(name, category string) float64 {
		var total float64
		if f := families[name]; f != nil {
			for _, m := range f.Metric {
				if category == "" {
					total += m.GetCounter().GetValue()
					continue
				}
				for _, label := range m.Label {
					if label.GetName() == "category" && label.GetValue() == category {
						total += m.GetCounter().GetValue()
					}
				}
			}
		}
		return total
	}
	cat := counter
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
	s.dbBusyRetries = counter("caesium_db_busy_retries_total", "")
	s.claimsTotal = counter("caesium_worker_claims_total", "")
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
		resp, err := h.client.do(ctx, http.MethodGet, "/health", nil)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
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
	for i := range steps {
		steps[i].Image = cfg.image
	}

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
	if err := h.cfg.validate(); err != nil {
		return &report{cfg: h.cfg, failure: "invalid_config", failureDetail: err.Error()}, err
	}
	ctx, cancel := context.WithTimeout(ctx, h.cfg.timeout)
	defer cancel()
	started := time.Now()
	results := make([]runResult, h.cfg.jobCount)
	for i := range results {
		results[i] = runResult{alias: fmt.Sprintf("load-test-job-%d", i), status: "untriggered"}
	}
	var baseline, end metricSample
	var samples []metricSample
	finish := func(class string, err error) (*report, error) {
		r := buildReport(h.cfg, results, baseline, end, samples, time.Since(started))
		r.startedAt, r.finishedAt = started, time.Now()
		r.failure = class
		if err != nil {
			r.failureDetail = err.Error()
		}
		return r, err
	}
	fmt.Fprintln(os.Stderr, "Waiting for server to be ready...")
	if err := h.waitForServer(ctx); err != nil {
		return finish("server_unavailable", err)
	}
	jobs, err := h.applyJobs(ctx)
	if err != nil {
		return finish("apply_failed", err)
	}
	baseline, err = sampleMetrics(ctx, h.client)
	if err != nil {
		return finish("metrics_missing", fmt.Errorf("baseline sample: %w", err))
	}
	baseline.phase = "baseline"
	samples = append(samples, baseline)

	// Start sampling before dispatch. A single submission slot must never block it.
	type samplingResult struct {
		samples []metricSample
		err     error
	}
	stopSamples := make(chan struct{})
	sampled := make(chan samplingResult, 1)
	go func() {
		ticker := time.NewTicker(h.cfg.sampleRate)
		defer ticker.Stop()
		var out samplingResult
		for {
			select {
			case <-ctx.Done():
				sampled <- out
				return
			case <-stopSamples:
				sampled <- out
				return
			case <-ticker.C:
				s, err := sampleMetrics(ctx, h.client)
				if err != nil {
					out.err = errors.Join(out.err, fmt.Errorf("periodic sample: %w", err))
					continue
				}
				s.phase = "periodic"
				out.samples = append(out.samples, s)
			}
		}
	}()
	queue := make(chan int)
	var workers sync.WaitGroup
	for range min(h.cfg.concurrency, len(jobs)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for i := range queue {
				if ctx.Err() != nil {
					continue
				}
				results[i] = h.triggerAndWait(ctx, jobs[i].alias, jobs[i].id)
			}
		}()
	}
dispatch:
	for i := range jobs {
		select {
		case <-ctx.Done():
			break dispatch
		case queue <- i:
		}
	}
	close(queue)
	workers.Wait()
	close(stopSamples)
	collected := <-sampled
	samples = append(samples, collected.samples...)
	end, err = sampleMetrics(ctx, h.client)
	if err == nil {
		end.phase = "final"
		samples = append(samples, end)
	}
	sampleErr := errors.Join(collected.err, err)
	for i := 1; i < len(samples); i++ {
		for key, previous := range samples[i-1].counters {
			value, present := samples[i].counters[key]
			if !present || value < previous {
				sampleErr = errors.Join(sampleErr, fmt.Errorf("counter reset or disappeared: %s", key))
			}
		}
	}
	if len(collected.samples) == 0 && time.Since(baseline.ts) >= h.cfg.sampleRate {
		sampleErr = errors.Join(sampleErr, errors.New("missing periodic samples during workload"))
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return finish("cancelled", ctx.Err())
		}
		return finish("deadline_exceeded", ctx.Err())
	}
	if sampleErr != nil {
		return finish("metrics_missing", sampleErr)
	}
	seenRuns := make(map[string]bool)
	for _, rr := range results {
		if rr.runID != "" {
			if seenRuns[rr.runID] {
				return finish("run_identity_invalid", fmt.Errorf("duplicate run ID %s", rr.runID))
			}
			seenRuns[rr.runID] = true
		}
		if rr.status != "succeeded" {
			return finish("run_failure", errors.New("one or more expected runs did not succeed; see run outcomes"))
		}
	}
	return finish("", nil)
}

// triggerAndWait records admission separately from the observed terminal status.
// A failed trigger response is uncertain: its write may have committed.
func (h *harness) triggerAndWait(ctx context.Context, alias, jobID string) runResult {
	rr := runResult{alias: alias, startedAt: time.Now()}
	runID, err := h.startRun(ctx, jobID)
	if err != nil {
		rr.err = fmt.Errorf("trigger response unavailable or rejected (admission unconfirmed): %w", err)
		rr.finishedAt, rr.status = time.Now(), "trigger_failed"
		return rr
	}
	rr.runID = runID
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		status, pollErr := h.client.getRunStatus(ctx, jobID, runID)
		if pollErr == nil {
			switch status {
			case "succeeded", "failed", "cancelled", "skipped":
				rr.status, rr.finishedAt = status, time.Now()
				if status != "succeeded" {
					rr.err = fmt.Errorf("run ended with status: %s", status)
				}
				return rr
			}
		}
		select {
		case <-ctx.Done():
			rr.err, rr.finishedAt, rr.status = ctx.Err(), time.Now(), "timeout"
			return rr
		case <-tick.C:
		}
	}
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
	cfg                                                       config
	startedAt, finishedAt                                     time.Time
	failure, failureDetail                                    string
	results                                                   []runResult
	runsObserved, runsUntriggered, runsCancelled, runsSkipped int
	totalDuration                                             time.Duration
	runsSucceeded                                             int
	runsFailed                                                int
	runsTimeout                                               int
	runsTriggerFailed                                         int

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
		results:       results,
	}

	// Tally run statuses and end-to-end durations.
	durations := make([]time.Duration, 0, len(results))
	seenRuns := make(map[string]bool)
	for _, rr := range results {
		if rr.runID != "" && !seenRuns[rr.runID] {
			seenRuns[rr.runID] = true
			r.runsObserved++
		}
		switch rr.status {
		case "succeeded":
			r.runsSucceeded++
		case "failed":
			r.runsFailed++
		case "timeout":
			r.runsTimeout++
		case "untriggered":
			r.runsUntriggered++
		case "cancelled":
			r.runsCancelled++
		case "skipped":
			r.runsSkipped++
		default:
			r.runsTriggerFailed++
		}
		if rr.status == "succeeded" && !rr.finishedAt.IsZero() && !rr.startedAt.IsZero() {
			durations = append(durations, rr.finishedAt.Sub(rr.startedAt))
		}
	}

	// End-to-end latency percentiles.
	slices.Sort(durations)
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

func (r *report) metricsValid() bool {
	return r.failure != "metrics_missing" && len(r.samples) >= 2 && r.samples[0].phase == "baseline" && r.samples[len(r.samples)-1].phase == "final"
}

func (r *report) outcome() string {
	if r.failure != "" {
		return "failed"
	}
	return "passed"
}

// MarshalJSON is the versioned evidence contract. Credentials are never included.
// observed counts only runs with acknowledged IDs; trigger_failed is unconfirmed
// admission and must not be interpreted as proof the server rejected the write.
func (r *report) MarshalJSON() ([]byte, error) {
	type resultJSON struct {
		Alias      string    `json:"alias"`
		RunID      string    `json:"run_id,omitempty"`
		Status     string    `json:"status"`
		Error      string    `json:"error,omitempty"`
		StartedAt  time.Time `json:"started_at"`
		FinishedAt time.Time `json:"finished_at"`
	}
	results := make([]resultJSON, 0, len(r.results))
	for _, rr := range r.results {
		out := resultJSON{Alias: rr.alias, RunID: rr.runID, Status: rr.status, StartedAt: rr.startedAt, FinishedAt: rr.finishedAt}
		if rr.err != nil {
			out.Error = rr.err.Error()
		}
		results = append(results, out)
	}
	samples := make([]map[string]any, 0, len(r.samples))
	for _, s := range r.samples {
		samples = append(samples, map[string]any{"at": s.ts, "phase": s.phase,
			"rows":            map[string]float64{"task_run_insert": s.taskRunInsert, "task_run_status": s.taskRunStatus, "event_insert": s.eventInsert, "lease_renewal": s.leaseRenewal, "callback": s.callback, "command": s.command, "checkpoint": s.checkpoint},
			"statements":      map[string]float64{"task_run_insert": s.taskRunInsertStmts, "task_run_status": s.taskRunStatusStmts, "event_insert": s.eventInsertStmts, "lease_renewal": s.leaseRenewalStmts, "callback": s.callbackStmts, "command": s.commandStmts, "checkpoint": s.checkpointStmts},
			"db_busy_retries": s.dbBusyRetries, "claims": s.claimsTotal})
	}
	var delta any
	if r.metricsValid() {
		delta = map[string]any{
			"rows":       map[string]float64{"task_run_insert": r.deltaTaskRunInsert, "task_run_status": r.deltaTaskRunStatus, "event_insert": r.deltaEventInsert, "lease_renewal": r.deltaLeaseRenewal, "callback": r.deltaCallback, "command": r.deltaCommand, "checkpoint": r.deltaCheckpoint},
			"statements": map[string]float64{"task_run_insert": r.deltaTaskRunInsertStmts, "task_run_status": r.deltaTaskRunStatusStmts, "event_insert": r.deltaEventInsertStmts, "lease_renewal": r.deltaLeaseRenewalStmts, "callback": r.deltaCallbackStmts, "command": r.deltaCommandStmts, "checkpoint": r.deltaCheckpointStmts},
		}
	}
	var workloadSeconds any
	if len(r.samples) >= 2 && r.samples[len(r.samples)-1].phase == "final" {
		workloadSeconds = r.samples[len(r.samples)-1].ts.Sub(r.samples[0].ts).Seconds()
	}
	return json.Marshal(map[string]any{
		"workload_interval_seconds": workloadSeconds,
		"schema_version":            1, "outcome": r.outcome(), "failure_class": r.failure, "failure_detail": r.failureDetail,
		"started_at": r.startedAt, "finished_at": r.finishedAt, "duration_seconds": r.totalDuration.Seconds(),
		"config": map[string]any{"jobs": r.cfg.jobCount, "fan_out": r.cfg.fanOut, "depth": r.cfg.depth, "concurrency": r.cfg.concurrency, "task_duration_seconds": r.cfg.taskDuration.Seconds(), "sample_interval_seconds": r.cfg.sampleRate.Seconds(), "timeout_seconds": r.cfg.timeout.Seconds(), "engine": r.cfg.engine, "image": r.cfg.image},
		"counts": map[string]int{"expected": r.cfg.jobCount, "observed": r.runsObserved, "succeeded": r.runsSucceeded, "failed": r.runsFailed, "timeout": r.runsTimeout, "trigger_failed": r.runsTriggerFailed, "untriggered": r.runsUntriggered, "cancelled": r.runsCancelled, "skipped": r.runsSkipped},
		"runs":   results, "samples": samples, "metric_delta": delta,
		"latency": map[string]any{"population": "succeeded_runs", "samples": r.runsSucceeded, "p50_seconds": r.endToEndP50.Seconds(), "p99_seconds": r.endToEndP99.Seconds()},
	})
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
	fmt.Fprintf(w, "Outcome:             %s\n", r.outcome())
	fmt.Fprintf(w, "Failure:             %s %s\n", r.failure, r.failureDetail)
	fmt.Fprintf(w, "Runs observed:       %d\n", r.runsObserved)
	fmt.Fprintf(w, "Runs untriggered:    %d\n", r.runsUntriggered)
	fmt.Fprintf(w, "Runs cancelled:      %d\n", r.runsCancelled)
	fmt.Fprintf(w, "Runs skipped:        %d\n", r.runsSkipped)
	fmt.Fprintf(w, "Metric samples:      %d\n", len(r.samples))
	fmt.Fprintf(w, "Runs succeeded:      %d\n", r.runsSucceeded)
	fmt.Fprintf(w, "Runs failed:         %d\n", r.runsFailed)
	fmt.Fprintf(w, "Runs timeout:        %d\n", r.runsTimeout)
	fmt.Fprintf(w, "Runs trigger-failed: %d\n", r.runsTriggerFailed)
	fmt.Fprintln(w, "")
	if !r.metricsValid() {
		fmt.Fprintln(w, "Metrics incomplete or invalid; write deltas and rates unavailable.")
		return
	}
	fmt.Fprintf(w, "Measured interval:   %s\n", r.samples[len(r.samples)-1].ts.Sub(r.samples[0].ts))
	fmt.Fprintln(w, "--- DB Write Breakdown (delta over measured interval) ---")
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
	fmt.Fprintln(w, "--- Successful Run Latency (includes status polling) ---")
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

func main() { os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr)) }

func runMain(args []string, stdout, stderr io.Writer) int {
	cfg := defaultConfig()
	flags := flag.NewFlagSet("load", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.serverURL, "server", cfg.serverURL, "Caesium server URL")
	flags.IntVar(&cfg.jobCount, "jobs", cfg.jobCount, "Number of synthetic jobs to create and run")
	flags.IntVar(&cfg.fanOut, "fan-out", cfg.fanOut, "DAG fan-out width")
	flags.IntVar(&cfg.depth, "depth", cfg.depth, "DAG depth (layers)")
	flags.DurationVar(&cfg.taskDuration, "task-duration", cfg.taskDuration, "How long each task sleeps (container execution time)")
	flags.IntVar(&cfg.concurrency, "concurrency", cfg.concurrency, "How many runs to trigger concurrently")
	flags.DurationVar(&cfg.sampleRate, "sample-rate", cfg.sampleRate, "How often to sample Prometheus metrics")
	flags.StringVar(&cfg.outputFile, "output", cfg.outputFile, "Write report to file (default: stdout only)")
	flags.StringVar(&cfg.apiKey, "api-key", cfg.apiKey, "API key for authenticated endpoints")
	flags.StringVar(&cfg.engine, "engine", cfg.engine, "Task engine: docker (default), kubernetes, or podman. Must match what the target Caesium deployment supports.")
	flags.DurationVar(&cfg.timeout, "timeout", cfg.timeout, "Overall deadline including readiness, apply, sampling and runs")
	flags.StringVar(&cfg.image, "image", cfg.image, "Task container image")
	flags.StringVar(&cfg.jsonFile, "json-output", cfg.jsonFile, "Versioned JSON report file, or - for clean JSON stdout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	rep, err := newHarness(cfg).run(context.Background())
	human := stdout
	if cfg.jsonFile == "-" {
		human = stderr
	}
	rep.print(human)
	if cfg.outputFile != "" {
		if writeErr := os.WriteFile(cfg.outputFile, []byte(rep.markdown()), 0644); writeErr != nil {
			fmt.Fprintln(stderr, writeErr)
			return 1
		}
	}
	if cfg.jsonFile != "" {
		data, writeErr := json.MarshalIndent(rep, "", "  ")
		if writeErr == nil {
			data = append(data, '\n')
			if cfg.jsonFile == "-" {
				_, writeErr = stdout.Write(data)
			} else {
				writeErr = os.WriteFile(cfg.jsonFile, data, 0644)
			}
		}
		if writeErr != nil {
			fmt.Fprintln(stderr, writeErr)
			return 1
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "load harness failed: %v\n", err)
		return 1
	}
	return 0
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
	if v, ok := os.LookupEnv(key); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0
		} // Validation rejects malformed values instead of silently using defaults.
		return n
	}
	return def
}

func envDurOrDefault(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0
		}
		return d
	}
	return def
}

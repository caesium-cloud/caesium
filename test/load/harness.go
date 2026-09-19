// Package load provides a synthetic load harness for measuring Caesium's
// per-shard write throughput, per-write-category distribution, admission
// accounting and observed run/task lifecycle intervals.
//
// The harness runs in one of two modes.
//
//   - mode=closed (the default, and what E1 shipped): a fixed set of runs is
//     dispatched through a bounded worker pool; each worker waits for its run
//     to reach a terminal status before it starts the next one. Throughput is
//     therefore governed by completion — a closed loop.
//   - mode=open: arrivals are scheduled from a wall-clock rate plan that never
//     waits on a completion or on the previous request's response. Every
//     offered arrival lands in exactly one accounting bucket (dropped by the
//     driver, admitted, queued/skipped, rejected by the server, or transport
//     uncertain), every admitted run is reconciled to a terminal status, and
//     the reporter refuses to call a result a pass while any admitted run is
//     unreconciled or while admission "improves" only because backlog grows.
//
// Usage:
//
//	just load-test # configure with CAESIUM_LOAD_* environment variables
//
// The harness is not an integration test — it runs against an already-started
// Caesium server reachable at CAESIUM_LOAD_SERVER (default http://127.0.0.1:8080).
// test/performance/load_test.go drives the versioned workload catalog in
// test/performance/workloads.json through this binary against a live server.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"os"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// reportSchemaVersion is the versioned machine-readable evidence contract.
//
//	1 — E1: closed-loop counts, metric deltas, run outcomes.
//	2 — E2: adds mode/workload identity, the open-loop admission ledger and its
//	    accounting identity, backlog and throughput verdicts, observed lifecycle
//	    intervals with explicit unavailability markers, external container
//	    resource observations, API-read and SSE-subscriber mixes, and the
//	    workload-driven / timer-driven split of the existing DB counters.
//	    Every schema 1 field is still emitted and still means what it meant.
const reportSchemaVersion = 2

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const (
	modeClosed = "closed"
	modeOpen   = "open"

	cacheOff  = "off"
	cacheMiss = "miss"
	cacheHit  = "hit"
)

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

	// ---- E2: open-loop arrival scheduling -------------------------------
	// mode selects closed (E1's completion-governed worker pool) or open
	// (arrival-rate scheduling that never waits on completion).
	mode string
	// workload is a free-form label recorded in the report. When a catalog
	// entry is selected it carries that entry's name.
	workload string
	// catalogFile / catalogWorkload select one entry of the versioned workload
	// catalog (test/performance/workloads.json) as the configuration base.
	catalogFile     string
	catalogWorkload string
	// rate is the offered arrival rate in runs/second; arrivalWindow is how
	// long arrivals are offered for. total offered = floor(rate*window), fixed
	// before the first request so the ledger's denominator can never drift.
	rate          float64
	arrivalWindow time.Duration
	// maxInFlight hard-caps concurrent trigger requests AND goroutines. An
	// arrival that finds the cap full is DROPPED by the driver, never queued:
	// the driver must never build an unbounded internal backlog.
	maxInFlight int
	// lateBudget is how far behind its scheduled instant an arrival may be
	// offered. Beyond it the arrival is dropped (client_scheduler_lag) rather
	// than fired as part of a catch-up burst, which would silently convert
	// a lagging driver into an unrequested spike.
	lateBudget time.Duration
	// drainTimeout bounds the post-window reconciliation phase.
	drainTimeout time.Duration
	// reconcileWorkers/pollInterval bound the run-status polling that
	// reconciles every admitted run to a terminal status.
	reconcileWorkers int
	pollInterval     time.Duration
	// requireSustained makes growing backlog across the arrival window a
	// failure. Deliberate-overload workloads set it false and assert the
	// drop/reject/backlog counters instead.
	requireSustained bool
	// allowRunFailures permits admitted runs to end non-succeeded. Workloads
	// whose whole point is a non-success terminal state (concurrency replace
	// cancels the run it replaces) set it; everything else must not.
	allowRunFailures bool

	// ---- E2: workload shape ---------------------------------------------
	// concurrencyStrategy/maxRuns write metadata.concurrency on every applied
	// job, which is how queue/replace/skip/fail admission is exercised.
	concurrencyStrategy string
	maxRuns             int
	// cacheMode: off (no cache metadata), miss (cache on, one run per job so
	// every task is a first execution), hit (cache on, a warm-up run per job
	// completes before the arrival window opens).
	cacheMode string
	// apiReadRate drives a concurrent open-loop read mix against the public
	// list/summary endpoints; subscribers holds N passive /v1/events SSE
	// connections open for the whole workload.
	apiReadRate float64
	subscribers int
	// lifecycle enables the /v1/events observer used to derive lifecycle
	// intervals from server-observed events.
	lifecycle bool
	// resourceContainer is the name/ID of the server container to observe from
	// OUTSIDE via the container runtime's stats API. This is an external
	// observation of a container the operator launched, not product telemetry:
	// per-task resource telemetry belongs to the resource right-sizing plan.
	resourceContainer string
	dockerHost        string
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

		mode:                envOrDefault("CAESIUM_LOAD_MODE", modeClosed),
		workload:            envOrDefault("CAESIUM_LOAD_WORKLOAD", ""),
		catalogFile:         envOrDefault("CAESIUM_LOAD_CATALOG", ""),
		catalogWorkload:     envOrDefault("CAESIUM_LOAD_CATALOG_WORKLOAD", ""),
		rate:                envFloatOrDefault("CAESIUM_LOAD_RATE", 2),
		arrivalWindow:       envDurOrDefault("CAESIUM_LOAD_ARRIVAL_WINDOW", 30*time.Second),
		maxInFlight:         envIntOrDefault("CAESIUM_LOAD_MAX_IN_FLIGHT", 32),
		lateBudget:          envDurOrDefault("CAESIUM_LOAD_LATE_BUDGET", 500*time.Millisecond),
		drainTimeout:        envDurOrDefault("CAESIUM_LOAD_DRAIN_TIMEOUT", 5*time.Minute),
		reconcileWorkers:    envIntOrDefault("CAESIUM_LOAD_RECONCILE_WORKERS", 8),
		pollInterval:        envDurOrDefault("CAESIUM_LOAD_POLL_INTERVAL", time.Second),
		requireSustained:    envBoolOrDefault("CAESIUM_LOAD_REQUIRE_SUSTAINED", false),
		allowRunFailures:    envBoolOrDefault("CAESIUM_LOAD_ALLOW_RUN_FAILURES", false),
		concurrencyStrategy: envOrDefault("CAESIUM_LOAD_CONCURRENCY_STRATEGY", ""),
		maxRuns:             envNonNegIntOrDefault("CAESIUM_LOAD_MAX_RUNS", 0),
		cacheMode:           envOrDefault("CAESIUM_LOAD_CACHE", cacheOff),
		apiReadRate:         envNonNegFloatOrDefault("CAESIUM_LOAD_API_READ_RATE", 0),
		subscribers:         envNonNegIntOrDefault("CAESIUM_LOAD_SUBSCRIBERS", 0),
		lifecycle:           envBoolOrDefault("CAESIUM_LOAD_LIFECYCLE", true),
		resourceContainer:   envOrDefault("CAESIUM_LOAD_RESOURCE_CONTAINER", ""),
		dockerHost:          envOrDefault("CAESIUM_LOAD_DOCKER_HOST", envOrDefault("DOCKER_HOST", "unix:///var/run/docker.sock")),
	}
}

// normalized fills the open-loop knobs on a configuration that predates them.
// A config built as a literal (schema 1 shape) carries an empty mode and zero
// open-loop values; those are legacy closed-mode configurations, not requests
// for a zero arrival rate. Anything that went through defaultConfig — every
// flag and environment path — always carries a mode, so an explicit zero from
// a real caller still fails validation.
func (c config) normalized() config {
	if c.mode != "" {
		return c
	}
	d := defaultConfig()
	c.mode = modeClosed
	c.rate = d.rate
	c.arrivalWindow = d.arrivalWindow
	c.maxInFlight = d.maxInFlight
	c.lateBudget = d.lateBudget
	c.drainTimeout = d.drainTimeout
	c.reconcileWorkers = d.reconcileWorkers
	c.pollInterval = d.pollInterval
	if c.cacheMode == "" {
		c.cacheMode = cacheOff
	}
	if c.dockerHost == "" {
		c.dockerHost = d.dockerHost
	}
	return c
}

// totalArrivals is the offered denominator: fixed before the first request so
// every later count can be reconciled against it. It is deliberately computed
// from the plan, never from how many requests the driver managed to send.
func (c config) totalArrivals() int {
	if c.mode != modeOpen {
		return 0
	}
	n := int(math.Floor(c.rate * c.arrivalWindow.Seconds()))
	if n < 1 {
		n = 1
	}
	return n
}

// validate runs before allocation, ticker creation, or any network side effect.
func (c config) validate() error {
	c = c.normalized()
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
	return c.validateOpenLoop()
}

// validateOpenLoop checks the E2 surface. The open-loop knobs are validated in
// BOTH modes so a malformed CAESIUM_LOAD_* value is rejected up front rather
// than lying dormant until someone flips mode=open.
func (c config) validateOpenLoop() error {
	if c.mode != modeClosed && c.mode != modeOpen {
		return errors.New("mode must be closed or open")
	}
	if !(c.rate > 0) || math.IsNaN(c.rate) || math.IsInf(c.rate, 0) || c.rate > 10000 {
		return errors.New("rate must be a positive arrival rate of at most 10000 runs/second")
	}
	if c.arrivalWindow <= 0 || c.lateBudget <= 0 || c.drainTimeout <= 0 || c.pollInterval <= 0 {
		return errors.New("arrival-window, late-budget, drain-timeout, and poll-interval must be positive durations")
	}
	if c.maxInFlight <= 0 || c.maxInFlight > 4096 {
		return errors.New("max-in-flight must be between 1 and 4096")
	}
	if c.reconcileWorkers <= 0 || c.reconcileWorkers > 512 {
		return errors.New("reconcile-workers must be between 1 and 512")
	}
	// Bound the ledger (and therefore every buffer sized from it) before
	// allocating: an open-loop driver with an unbounded plan is exactly the
	// unbounded internal queue this mode exists to avoid.
	if c.mode == modeOpen && c.rate*c.arrivalWindow.Seconds() > 100000 {
		return errors.New("arrival plan exceeds the safety limit of 100000 offered runs")
	}
	if c.apiReadRate < 0 || math.IsNaN(c.apiReadRate) || math.IsInf(c.apiReadRate, 0) || c.apiReadRate > 10000 {
		return errors.New("api-read-rate must be a non-negative rate of at most 10000 reads/second")
	}
	if c.subscribers < 0 || c.subscribers > 256 {
		return errors.New("subscribers must be between 0 and 256")
	}
	switch c.cacheMode {
	case cacheOff, cacheMiss, cacheHit:
	default:
		return errors.New("cache must be off, miss, or hit")
	}
	switch c.concurrencyStrategy {
	case "", "queue", "replace", "skip", "fail":
	default:
		return errors.New("concurrency-strategy must be empty, queue, replace, skip, or fail")
	}
	if c.maxRuns < 0 || c.maxRuns > 10000 {
		return errors.New("max-runs must be between 0 and 10000")
	}
	if c.concurrencyStrategy != "" && c.maxRuns <= 0 {
		return errors.New("concurrency-strategy requires a positive max-runs")
	}
	if c.resourceContainer != "" && strings.TrimSpace(c.dockerHost) == "" {
		return errors.New("resource-container requires a docker-host")
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
	// Concurrency is metadata.concurrency — the run-level admission policy that
	// turns excess arrivals into queued/skipped/failed outcomes instead of new
	// runs. Omitted unless a workload asks for it.
	Concurrency *jobConcurrency `json:"concurrency,omitempty"`
	// Cache is metadata.cache. The server's schema types it as `any`, so the
	// harness sends the documented object form {enabled, ttl}.
	Cache map[string]any `json:"cache,omitempty"`
}

type jobConcurrency struct {
	MaxRuns  int    `json:"maxRuns,omitempty"`
	Strategy string `json:"strategy,omitempty"`
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

// runSnapshot is the public run read, keeping only the server-supplied
// timestamps and counters the lifecycle report is derived from. Every time
// here is the SERVER's, never the driver's clock.
type runSnapshot struct {
	ID            string         `json:"id"`
	JobID         string         `json:"job_id"`
	Status        string         `json:"status"`
	CreatedAt     time.Time      `json:"created_at"`
	StartedAt     time.Time      `json:"started_at"`
	CompletedAt   *time.Time     `json:"completed_at"`
	CacheHits     int            `json:"cache_hits"`
	ExecutedTasks int            `json:"executed_tasks"`
	TotalTasks    int            `json:"total_tasks"`
	Tasks         []taskSnapshot `json:"tasks"`
}

type taskSnapshot struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	CacheHit    bool       `json:"cache_hit"`
	CreatedAt   time.Time  `json:"created_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// terminal reports whether the run has reached a status that will not change.
func (r runSnapshot) terminal() bool {
	switch r.Status {
	case "succeeded", "failed", "cancelled", "skipped":
		return true
	}
	return false
}

// getRun returns the full public run read. It is the authoritative
// reconciliation surface: an admitted run is only considered reconciled once
// this endpoint reports a terminal status.
func (c *client) getRun(ctx context.Context, jobID, runID string) (runSnapshot, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/jobs/"+jobID+"/runs/"+runID, nil)
	if err != nil {
		return runSnapshot{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return runSnapshot{}, fmt.Errorf("get run %s: HTTP %d: %s", runID, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out runSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		return runSnapshot{}, fmt.Errorf("parse run read: %w", err)
	}
	return out, nil
}

// listRuns pages GET /v1/jobs/:id/runs. It is how arrivals with NO acknowledged
// run ID — a bare-202 queue/skip, or a transport timeout that DT-QUORUM-01 says
// is possibly committed — are reconciled after the fact: the server's own run
// census for the job is the only thing that can say whether a write landed.
func (c *client) listRuns(ctx context.Context, jobID string, maxPages int) ([]runSnapshot, error) {
	var all []runSnapshot
	offset := 0
	for page := 0; page < maxPages; page++ {
		resp, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/v1/jobs/%s/runs?limit=1000&offset=%d", jobID, offset), nil)
		if err != nil {
			return all, err
		}
		raw, _ := io.ReadAll(resp.Body)
		next := resp.Header.Get("X-Caesium-Next-Offset")
		code := resp.StatusCode
		resp.Body.Close()
		if code >= 300 {
			return all, fmt.Errorf("list runs for job %s: HTTP %d: %s", jobID, code, strings.TrimSpace(string(raw)))
		}
		var runs []runSnapshot
		if err := json.Unmarshal(raw, &runs); err != nil {
			return all, fmt.Errorf("parse run list: %w", err)
		}
		all = append(all, runs...)
		if next == "" || len(runs) == 0 {
			return all, nil
		}
		parsed, err := strconv.Atoi(next)
		if err != nil || parsed <= offset {
			return all, nil
		}
		offset = parsed
	}
	return all, fmt.Errorf("list runs for job %s: exceeded %d pages", jobID, maxPages)
}

// queueDepth returns how many runs are waiting in the job's concurrency queue.
func (c *client) queueDepth(ctx context.Context, jobID string) (int, error) {
	resp, err := c.do(ctx, http.MethodGet, "/v1/jobs/"+jobID+"/queue", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("list queue for job %s: HTTP %d", jobID, resp.StatusCode)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, fmt.Errorf("parse queue list: %w", err)
	}
	return len(rows), nil
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
// Observed events (/v1/events) — the lifecycle measurement source
// ---------------------------------------------------------------------------

// serverEvent is the subset of the published event envelope the harness reads.
// Timestamp is the SERVER's clock; nothing here is a client-side guess.
type serverEvent struct {
	Sequence  uint64    `json:"sequence"`
	Type      string    `json:"type"`
	JobID     string    `json:"job_id"`
	RunID     string    `json:"run_id"`
	TaskID    string    `json:"task_id"`
	Timestamp time.Time `json:"timestamp"`
}

// identity is the deduplication key. DT-EVENT-01 makes delivery at-least-once
// and per-run sequences sparse, so the harness dedupes by event identity and
// never assumes a gap-free counter. Events published without a persisted
// sequence fall back to their natural composite identity.
func (e serverEvent) identity() string {
	if e.Sequence > 0 {
		return "seq:" + strconv.FormatUint(e.Sequence, 10)
	}
	return strings.Join([]string{"nat", e.Type, e.RunID, e.TaskID, e.Timestamp.UTC().Format(time.RFC3339Nano)}, "|")
}

// streamEvents opens GET /v1/events and calls onEvent for every parsed frame
// until the callback returns false, the body ends, or ctx is done. It is a
// minimal SSE reader: enough for `id:`/`event:`/`data:` frames and `:` comments.
func (c *client) streamEvents(ctx context.Context, query string, onEvent func(serverEvent) bool) error {
	return c.streamEventsWithOpen(ctx, query, nil, onEvent)
}

// streamEventsWithOpen is streamEvents plus an onOpen callback fired once the
// server has answered with a usable SSE response. Callers that measure how long
// a subscription was actually held must time from THERE: a connection that
// stalls before headers is not an open stream.
func (c *client) streamEventsWithOpen(ctx context.Context, query string, onOpen func(), onEvent func(serverEvent) bool) error {
	path := "/v1/events"
	if query != "" {
		path += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	// The streaming client must not carry the 30s request timeout the pooled
	// client uses: a long-lived subscriber is the point.
	streamClient := &http.Client{Transport: c.http.Transport}
	resp, err := streamClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("events stream: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if onOpen != nil {
		onOpen()
	}
	reader := bufio.NewScanner(resp.Body)
	reader.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var id, data string
	for reader.Scan() {
		line := reader.Text()
		switch {
		case line == "":
			if data != "" {
				var evt serverEvent
				if err := json.Unmarshal([]byte(data), &evt); err == nil {
					if evt.Sequence == 0 && id != "" {
						if parsed, convErr := strconv.ParseUint(id, 10, 64); convErr == nil {
							evt.Sequence = parsed
						}
					}
					if !onEvent(evt) {
						return nil
					}
				}
			}
			id, data = "", ""
		case strings.HasPrefix(line, ":"):
			// comment/keepalive
		case strings.HasPrefix(line, "id:"):
			id = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if err := reader.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// readUntilIdle drains the persisted backlog from cursor and returns once no
// frame has arrived for idle (the stream stays open and live afterwards, so
// silence is the only available end-of-backlog signal), or once cap frames
// have been read, or ctx is done. It reports how many frames it read and
// whether the cap truncated the read.
func (c *client) readUntilIdle(ctx context.Context, query string, idle time.Duration, limit int, sink func(serverEvent)) (int, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	frames := make(chan serverEvent, 1024)
	errCh := make(chan error, 1)
	go func() {
		errCh <- c.streamEvents(ctx, query, func(e serverEvent) bool {
			select {
			case frames <- e:
				return true
			case <-ctx.Done():
				return false
			}
		})
	}()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	read, truncated := 0, false
	for {
		select {
		case <-ctx.Done():
			return read, truncated, ctx.Err()
		case err := <-errCh:
			// Drain whatever is already buffered before reporting.
			for {
				select {
				case e := <-frames:
					sink(e)
					read++
					continue
				default:
				}
				break
			}
			return read, truncated, err
		case e := <-frames:
			sink(e)
			read++
			if read >= limit {
				return read, true, nil
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idle)
		case <-timer.C:
			return read, truncated, nil
		}
	}
}

// taskTimeline is what one task's observed events say about its lifecycle.
type taskTimeline struct {
	ready    time.Time
	started  time.Time
	terminal time.Time
	cached   bool
}

// runTimeline is what one run's observed events say about its lifecycle.
type runTimeline struct {
	started  time.Time
	terminal time.Time
	tasks    map[string]*taskTimeline
}

// eventObserver accumulates deduplicated events into per-run timelines.
type eventObserver struct {
	mu         sync.Mutex
	seen       map[string]struct{}
	runs       map[string]*runTimeline
	received   int
	duplicates int
	maxSeq     uint64
	streamErrs []string
	truncated  bool
}

func newEventObserver() *eventObserver {
	return &eventObserver{seen: map[string]struct{}{}, runs: map[string]*runTimeline{}}
}

func (o *eventObserver) observe(e serverEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	key := e.identity()
	if _, dup := o.seen[key]; dup {
		o.duplicates++
		return
	}
	o.seen[key] = struct{}{}
	o.received++
	if e.Sequence > o.maxSeq {
		o.maxSeq = e.Sequence
	}
	if e.RunID == "" {
		return
	}
	rt := o.runs[e.RunID]
	if rt == nil {
		rt = &runTimeline{tasks: map[string]*taskTimeline{}}
		o.runs[e.RunID] = rt
	}
	task := func() *taskTimeline {
		if e.TaskID == "" {
			return nil
		}
		t := rt.tasks[e.TaskID]
		if t == nil {
			t = &taskTimeline{}
			rt.tasks[e.TaskID] = t
		}
		return t
	}
	// Keep the EARLIEST observation of each landmark: at-least-once delivery
	// plus catch-up replay means the same landmark can be seen more than once
	// with a different arrival order, but its server timestamp is stable.
	earliest := func(dst *time.Time, ts time.Time) {
		if ts.IsZero() {
			return
		}
		if dst.IsZero() || ts.Before(*dst) {
			*dst = ts
		}
	}
	switch e.Type {
	case "run_started":
		earliest(&rt.started, e.Timestamp)
	case "run_completed", "run_failed", "run_cancelled", "run_terminal", "run_timed_out":
		earliest(&rt.terminal, e.Timestamp)
	case "task_ready":
		if t := task(); t != nil {
			earliest(&t.ready, e.Timestamp)
		}
	case "task_started":
		if t := task(); t != nil {
			earliest(&t.started, e.Timestamp)
		}
	case "task_succeeded", "task_failed", "task_skipped":
		if t := task(); t != nil {
			earliest(&t.terminal, e.Timestamp)
		}
	case "task_cached":
		if t := task(); t != nil {
			t.cached = true
			earliest(&t.terminal, e.Timestamp)
		}
	}
}

// snapshotRun returns an independent copy of one run's timeline, safe to read
// while the live subscriber keeps observing.
func (o *eventObserver) snapshotRun(runID string) *runTimeline {
	o.mu.Lock()
	defer o.mu.Unlock()
	src := o.runs[runID]
	if src == nil {
		return nil
	}
	out := &runTimeline{started: src.started, terminal: src.terminal, tasks: make(map[string]*taskTimeline, len(src.tasks))}
	for id, t := range src.tasks {
		copied := *t
		out.tasks[id] = &copied
	}
	return out
}

func (o *eventObserver) cursor() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.maxSeq
}

func (o *eventObserver) noteError(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.streamErrs) < 8 {
		o.streamErrs = append(o.streamErrs, err.Error())
	}
}

// ---------------------------------------------------------------------------
// Lifecycle intervals
// ---------------------------------------------------------------------------

// intervalStats accumulates one named lifecycle interval. An interval that
// could not be observed is NEVER reported as zero and never omitted: it
// carries an explicit unavailable count keyed by reason.
type intervalStats struct {
	name        string
	source      string
	samples     []time.Duration
	unavailable map[string]int
}

func (s *intervalStats) observe(d time.Duration) {
	if d < 0 {
		// Two publishers' clocks, or a replayed landmark, produced an
		// impossible interval. Record it as unavailable rather than folding a
		// negative duration into a percentile.
		s.mark("negative_interval")
		return
	}
	s.samples = append(s.samples, d)
}

func (s *intervalStats) mark(reason string) {
	if s.unavailable == nil {
		s.unavailable = map[string]int{}
	}
	s.unavailable[reason]++
}

// between observes end-start when both landmarks exist, and otherwise marks
// the named reason for whichever landmark is missing.
func (s *intervalStats) between(start, end time.Time, startReason, endReason string) {
	switch {
	case start.IsZero():
		s.mark(startReason)
	case end.IsZero():
		s.mark(endReason)
	default:
		s.observe(end.Sub(start))
	}
}

func (s *intervalStats) json() map[string]any {
	out := map[string]any{"source": s.source, "samples": len(s.samples)}
	unavailable := map[string]int{}
	total := 0
	for reason, n := range s.unavailable {
		unavailable[reason] = n
		total += n
	}
	out["unavailable"] = total
	out["unavailable_reasons"] = unavailable
	if len(s.samples) == 0 {
		// Explicit marker: an interval with no observation reports its status,
		// never 0 seconds and never an absent key.
		out["status"] = "unavailable"
		if total == 0 {
			out["unavailable_reasons"] = map[string]int{"not_measured": 1}
			out["unavailable"] = 1
		}
		out["p50_seconds"], out["p95_seconds"], out["p99_seconds"] = nil, nil, nil
		out["min_seconds"], out["max_seconds"] = nil, nil
		return out
	}
	sorted := slices.Clone(s.samples)
	slices.Sort(sorted)
	out["status"] = "ok"
	out["p50_seconds"] = percentile(sorted, 50).Seconds()
	out["p95_seconds"] = percentile(sorted, 95).Seconds()
	out["p99_seconds"] = percentile(sorted, 99).Seconds()
	out["min_seconds"] = sorted[0].Seconds()
	out["max_seconds"] = sorted[len(sorted)-1].Seconds()
	return out
}

// lifecycleReport holds every declared interval, from both observation
// sources, so a consumer always finds the key and an explicit status.
type lifecycleReport struct {
	intervals    []*intervalStats
	eventsStatus string
	eventsReason string
	received     int
	duplicates   int
	truncated    bool
}

func (l *lifecycleReport) interval(name, source string) *intervalStats {
	for _, s := range l.intervals {
		if s.name == name {
			return s
		}
	}
	s := &intervalStats{name: name, source: source}
	l.intervals = append(l.intervals, s)
	return s
}

func (l *lifecycleReport) json() map[string]any {
	intervals := map[string]any{}
	for _, s := range l.intervals {
		intervals[s.name] = s.json()
	}
	return map[string]any{
		"event_stream_status":  l.eventsStatus,
		"event_stream_reason":  l.eventsReason,
		"events_observed":      l.received,
		"events_deduplicated":  l.duplicates,
		"event_read_truncated": l.truncated,
		"intervals":            intervals,
	}
}

// ---------------------------------------------------------------------------
// External container resource observation
// ---------------------------------------------------------------------------

// containerStats is one external observation of the server container, read
// from the container runtime's own stats API from OUTSIDE the server process.
// It is deliberately NOT product telemetry: per-task resource capture, OOM
// semantics and resource fields belong to the resource right-sizing plan.
type containerStats struct {
	at          time.Time
	cpuPercent  float64
	memoryBytes float64
	memoryLimit float64
}

// dockerStatsClient talks the Docker Engine HTTP API over a unix socket using
// only the standard library — the harness adds no module dependency.
type dockerStatsClient struct {
	http *http.Client
	host string
}

func newDockerStatsClient(host string) (*dockerStatsClient, error) {
	socket := strings.TrimPrefix(host, "unix://")
	if !strings.HasPrefix(host, "unix://") && strings.HasPrefix(host, "/") {
		socket = host
	} else if !strings.HasPrefix(host, "unix://") {
		return nil, fmt.Errorf("unsupported docker host %q: only unix sockets are supported", host)
	}
	if _, err := os.Stat(socket); err != nil {
		return nil, fmt.Errorf("docker socket %s is not reachable: %w", socket, err)
	}
	return &dockerStatsClient{
		host: socket,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socket)
				},
			},
		},
	}, nil
}

func (d *dockerStatsClient) sample(ctx context.Context, container string) (containerStats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+url.PathEscape(container)+"/stats?stream=false&one-shot=false", nil)
	if err != nil {
		return containerStats{}, err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return containerStats{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return containerStats{}, fmt.Errorf("container stats %s: HTTP %d: %s", container, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var payload struct {
		CPUStats struct {
			CPUUsage struct {
				TotalUsage  float64   `json:"total_usage"`
				PerCPUUsage []float64 `json:"percpu_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage float64 `json:"system_cpu_usage"`
			OnlineCPUs     float64 `json:"online_cpus"`
		} `json:"cpu_stats"`
		PreCPUStats struct {
			CPUUsage struct {
				TotalUsage float64 `json:"total_usage"`
			} `json:"cpu_usage"`
			SystemCPUUsage float64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		MemoryStats struct {
			Usage float64 `json:"usage"`
			Limit float64 `json:"limit"`
		} `json:"memory_stats"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return containerStats{}, fmt.Errorf("parse container stats: %w", err)
	}
	out := containerStats{at: time.Now(), memoryBytes: payload.MemoryStats.Usage, memoryLimit: payload.MemoryStats.Limit}
	cpuDelta := payload.CPUStats.CPUUsage.TotalUsage - payload.PreCPUStats.CPUUsage.TotalUsage
	sysDelta := payload.CPUStats.SystemCPUUsage - payload.PreCPUStats.SystemCPUUsage
	cpus := payload.CPUStats.OnlineCPUs
	if cpus == 0 {
		cpus = float64(len(payload.CPUStats.CPUUsage.PerCPUUsage))
	}
	if cpus == 0 {
		cpus = 1
	}
	if cpuDelta > 0 && sysDelta > 0 {
		out.cpuPercent = cpuDelta / sysDelta * cpus * 100
	}
	return out, nil
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
	// invocationID makes this process's arrival identities unique.
	invocationID string
}

func newHarness(cfg config) *harness {
	cfg = cfg.normalized()
	return &harness{
		cfg:          cfg,
		client:       newClient(cfg.serverURL, cfg.apiKey),
		invocationID: strconv.FormatInt(time.Now().UnixNano(), 36),
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

// aliasFor names job i. A workload label gives each catalog entry its own job
// rows, so one workload's cache state, concurrency queue and run history can
// never be mistaken for another's.
func (c config) aliasFor(i int) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, strings.TrimSpace(c.workload))
	if slug == "" {
		return fmt.Sprintf("load-test-job-%d", i)
	}
	return fmt.Sprintf("load-%s-job-%d", slug, i)
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
		alias := cfg.aliasFor(i)
		path := fmt.Sprintf("/load/%s", alias)
		meta := jobMeta{Alias: alias}
		if cfg.concurrencyStrategy != "" {
			meta.Concurrency = &jobConcurrency{MaxRuns: cfg.maxRuns, Strategy: cfg.concurrencyStrategy}
		}
		if cfg.cacheMode != cacheOff {
			meta.Cache = map[string]any{"enabled": true, "ttl": "1h"}
		}
		defs = append(defs, jobDef{
			APIVersion: "v1",
			Kind:       "Job",
			Metadata:   meta,
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

// startEventObserver probes the event store's current cursor, then holds one
// live subscription open for the whole workload. The cursor matters: resuming
// from it keeps both the live stream and the post-drain catch-up scoped to
// THIS workload's events instead of replaying the server's whole history.
func (h *harness) startEventObserver(ctx context.Context) (*eventObserver, uint64, context.CancelFunc, <-chan struct{}) {
	obs := newEventObserver()
	done := make(chan struct{})
	if !h.cfg.lifecycle {
		close(done)
		return obs, 0, func() {}, done
	}
	probe := newEventObserver()
	probeCtx, probeCancel := context.WithTimeout(ctx, 30*time.Second)
	_, truncated, err := h.client.readUntilIdle(probeCtx, "", time.Second, 200000, probe.observe)
	probeCancel()
	cursor := probe.cursor()
	obs.truncated = truncated
	obs.noteError(err)

	streamCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer close(done)
		obs.noteError(h.client.streamEvents(streamCtx, "cursor="+strconv.FormatUint(cursor, 10), func(e serverEvent) bool {
			obs.observe(e)
			return true
		}))
	}()
	return obs, cursor, cancel, done
}

// warmCache runs each job once to completion before the arrival window opens,
// so the cache=hit workload measures REUSE rather than a first execution. It
// returns how many warm-up runs completed.
func (h *harness) warmCache(ctx context.Context, jobs []appliedJob) (int, error) {
	if h.cfg.cacheMode != cacheHit {
		return 0, nil
	}
	fmt.Fprintf(os.Stderr, "Warming cache with %d run(s)...\n", len(jobs))
	warmCtx, cancel := context.WithTimeout(ctx, h.cfg.drainTimeout)
	defer cancel()
	warmed := 0
	for _, job := range jobs {
		rr := h.triggerAndWait(warmCtx, job.alias, job.id)
		if rr.status != "succeeded" {
			return warmed, fmt.Errorf("cache warm-up run for %s ended %s: %v", job.alias, rr.status, rr.err)
		}
		warmed++
	}
	return warmed, nil
}

// ledgerResults projects the arrival ledger onto the report's run-result
// vocabulary so the schema 1 counts stay meaningful in open mode.
func ledgerResults(lg *ledger) []runResult {
	arrivals := lg.snapshotArrivals()
	out := make([]runResult, len(arrivals))
	for i, a := range arrivals {
		rr := runResult{alias: a.alias, runID: a.runID, startedAt: a.offeredAt, finishedAt: a.settledAt}
		switch a.outcome {
		case outcomeAdmitted:
			if a.reconciled {
				rr.status = a.terminalStatus
				// End-to-end latency is a DRIVER-clock pair: offeredAt to the
				// driver's own terminal observation. Substituting the server's
				// completed_at here mixed two clocks — a server five seconds
				// behind turned a one-second run into ~-4s — and silently
				// dropped the status-polling delay the report claims to
				// include. The server's own timestamps stay in a.snapshot,
				// where the lifecycle intervals pair them with each other.
				if !a.reconciledAt.IsZero() {
					rr.finishedAt = a.reconciledAt
				}
				if rr.status != "succeeded" {
					rr.err = fmt.Errorf("run ended with status: %s", rr.status)
				}
			} else {
				rr.status = "unreconciled"
				rr.err = errors.New("admitted run never reached a terminal status inside the drain deadline")
			}
		case outcomeQueued:
			rr.status = outcomeQueued
			rr.err = errors.New("bare 202: queued or skipped by the concurrency policy, no run identity acknowledged")
		case outcomeRejected:
			rr.status = outcomeRejected
			rr.err = fmt.Errorf("server rejected the trigger: HTTP %d %s", a.statusCode, a.reason)
		case outcomeUncertain:
			rr.status = outcomeUncertain
			rr.err = fmt.Errorf("trigger response unavailable (admission unconfirmed, possibly committed): %s", a.reason)
		default:
			rr.status = outcomeDropped
			rr.err = fmt.Errorf("driver did not offer this arrival: %s", a.reason)
		}
		out[i] = rr
	}
	return out
}

// run executes the full load harness and returns a report.
func (h *harness) run(ctx context.Context) (*report, error) {
	if err := h.cfg.validate(); err != nil {
		// Nothing was offered, so the (empty) ledger trivially balances; the
		// failure class is what carries the verdict.
		return &report{cfg: h.cfg, failure: "invalid_config", failureDetail: err.Error(), identityOK: true}, err
	}
	ctx, cancel := context.WithTimeout(ctx, h.cfg.timeout)
	defer cancel()
	started := time.Now()
	open := h.cfg.mode == modeOpen
	expected := h.cfg.jobCount
	initialStatus := "untriggered"
	if open {
		expected = h.cfg.totalArrivals()
		// Every planned arrival starts life accounted for. If the driver never
		// reaches it, it is a DROP the driver owns — not a silent absence.
		initialStatus = outcomeDropped
	}
	results := make([]runResult, expected)
	for i := range results {
		results[i] = runResult{alias: h.cfg.aliasFor(i % h.cfg.jobCount), status: initialStatus}
	}
	var baseline, end metricSample
	var samples []metricSample
	var openRes *openLoopResult
	var lg *ledger
	finish := func(class string, err error) (*report, error) {
		r := buildReport(h.cfg, results, baseline, end, samples, time.Since(started))
		r.startedAt, r.finishedAt = started, time.Now()
		r.open, r.ledger = openRes, lg
		r.deriveOpenLoop()
		r.failure = class
		if err != nil {
			r.failureDetail = err.Error()
		}
		return r, err
	}
	fmt.Fprintln(os.Stderr, "Waiting for server to be ready...")
	readyCtx, readyCancel := context.WithTimeout(ctx, 30*time.Second)
	readyErr := h.waitForServer(readyCtx)
	readyCancel()
	if readyErr != nil {
		return finish("server_unavailable", readyErr)
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
	// measurementStart is where the MEASURED sample set begins. A cache warm-up
	// moves it forward past the warm-up's own cold executions.
	measurementStart := baseline.ts

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
	if open {
		lg = newLedger(expected)
		obs, cursor, obsCancel, obsDone := h.startEventObserver(ctx)
		warmupBegan := time.Now()
		warmed, warmErr := h.warmCache(ctx, jobs)
		if warmErr != nil {
			obsCancel()
			<-obsDone
			close(stopSamples)
			<-sampled
			return finish("warmup_failed", warmErr)
		}
		// The warm-up executed real, COLD runs. Measuring the window from the
		// pre-warm-up baseline charged those executions to the workload, so the
		// cache-hit entry reported three warm-up runs' SQL alongside six
		// measured arrivals. Keep the warm-up's own delta as evidence, then
		// restart the measured sample set from a FRESH baseline.
		var warmupEvidence *warmupMetrics
		if warmed > 0 {
			fresh, freshErr := sampleMetrics(ctx, h.client)
			if freshErr != nil {
				obsCancel()
				<-obsDone
				close(stopSamples)
				<-sampled
				return finish("metrics_missing", fmt.Errorf("post-warmup baseline sample: %w", freshErr))
			}
			fresh.phase = "baseline"
			warmRows, warmStmts := writeDeltas(baseline, fresh)
			warmupEvidence = &warmupMetrics{
				runs: warmed, seconds: time.Since(warmupBegan).Seconds(),
				rows: warmRows, statements: warmStmts,
			}
			baseline = fresh
			measurementStart = fresh.ts
			samples = []metricSample{fresh}
		}
		openRes = h.runOpenLoop(ctx, jobs, lg, obs, cursor)
		openRes.warmupRuns = warmed
		openRes.warmup = warmupEvidence
		obsCancel()
		<-obsDone
		results = ledgerResults(lg)
	} else {
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
	}
	workloadFinished := time.Now()
	close(stopSamples)
	collected := <-sampled
	for _, sample := range collected.samples {
		// Samples taken before the measured baseline belong to the cache
		// warm-up, whose cold executions are reported separately.
		if sample.ts.Before(measurementStart) {
			continue
		}
		// A slow scrape that finishes after the workers cannot establish coverage
		// of execution. The explicit final scrape records the post-workload state.
		if sample.ts.Before(workloadFinished) {
			samples = append(samples, sample)
		}
	}
	end, err = sampleMetrics(ctx, h.client)
	if err == nil {
		end.phase = "final"
		samples = append(samples, end)
	}
	sampleErr := errors.Join(collected.err, err)
	// A completed workload must expose both SQL statements and rows. Empty
	// category vectors are only legitimate before the first measured write.
	if err == nil {
		for _, family := range []string{"caesium_db_writes_total", "caesium_db_statements_total"} {
			found := false
			for key := range end.counters {
				if strings.HasPrefix(key, family+"{") {
					found = true
					break
				}
			}
			if !found {
				sampleErr = errors.Join(sampleErr, fmt.Errorf("final sample missing required %s", family))
			}
		}
	}
	for i := 1; i < len(samples); i++ {
		for key, previous := range samples[i-1].counters {
			value, present := samples[i].counters[key]
			if !present || value < previous {
				sampleErr = errors.Join(sampleErr, fmt.Errorf("counter reset or disappeared: %s", key))
			}
		}
	}
	periodicSamples := 0
	for _, sample := range samples {
		if sample.phase == "periodic" {
			periodicSamples++
		}
	}
	if periodicSamples == 0 && workloadFinished.Sub(baseline.ts) >= h.cfg.sampleRate {
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
		if !open && rr.status != "succeeded" {
			return finish("run_failure", errors.New("one or more expected runs did not succeed; see run outcomes"))
		}
	}
	if open {
		probe := buildReport(h.cfg, results, baseline, end, samples, time.Since(started))
		probe.open, probe.ledger = openRes, lg
		probe.deriveOpenLoop()
		if class, openErr := probe.judgeOpenLoop(); openErr != nil {
			return finish(class, openErr)
		}
	}
	return finish("", nil)
}

// judgeOpenLoop applies the open-loop pass conditions. It is deliberately
// strict: an unreconciled admission or a broken accounting identity is a
// failure or an inconclusive result, never a pass, and "more admitted" is not
// progress while the backlog is climbing.
func (r *report) judgeOpenLoop() (string, error) {
	if !r.identityOK {
		return "run_identity_invalid", errors.New(r.identityDetail)
	}
	if r.backlogNegative {
		return "accounting_mismatch", errors.New(
			"a backlog sample went negative: admitted and terminal are counting different populations")
	}
	attempted := r.runsObserved + r.runsQueued + r.runsRejected + r.runsUncertain
	if offered := len(r.results); offered != r.runsDropped+attempted {
		return "accounting_mismatch", fmt.Errorf(
			"offered=%d != dropped=%d + attempted=%d (admitted=%d queued_or_skipped=%d rejected=%d transport_uncertain=%d)",
			offered, r.runsDropped, attempted, r.runsObserved, r.runsQueued, r.runsRejected, r.runsUncertain)
	}
	settled := r.runsSucceeded + r.runsFailed + r.runsCancelled + r.runsSkipped + r.runsUnreconciled
	if r.runsObserved != settled {
		return "accounting_mismatch", fmt.Errorf(
			"admitted=%d != completed_ok=%d + completed_failed=%d + unreconciled=%d",
			r.runsObserved, r.runsSucceeded, r.runsFailed+r.runsCancelled+r.runsSkipped, r.runsUnreconciled)
	}
	if r.runsUnreconciled > 0 {
		return "unreconciled_admission", fmt.Errorf(
			"%d admitted run(s) never reached a terminal status inside the drain deadline", r.runsUnreconciled)
	}
	// Arrivals that acknowledged no run identity — bare-202 queue/skip answers
	// and possibly-committed transport timeouts — are reconciled ONLY through
	// the server's run census. If the census could not be taken, or a run it
	// discovered never settled, the workload has unaccounted work and is not a
	// pass.
	// r.censusStatus is empty only when no ledger was attached (reporter-level
	// fixtures); a real open-loop run always carries one.
	if identityLess := r.runsQueued + r.runsUncertain; identityLess > 0 && r.censusStatus != "" && r.censusStatus != "ok" {
		return "census_unavailable", fmt.Errorf(
			"%d arrival(s) acknowledged no run identity but the server run census is %q (%s), so possibly-committed work is unaccounted",
			identityLess, r.censusStatus, r.censusReason)
	}
	if r.censusExtra != r.censusTerminal {
		return "unreconciled_census_run", fmt.Errorf(
			"the census discovered %d run(s) with no acknowledged admission but only %d settled inside the drain deadline",
			r.censusExtra, r.censusTerminal)
	}
	if !r.cfg.allowRunFailures && r.censusFailed > 0 {
		return "run_failure", fmt.Errorf("%d census-discovered run(s) ended non-succeeded", r.censusFailed)
	}
	// Queue observation must fail CLOSED. A bare-202 arrival's work lives in the
	// concurrency queue until the dequeuer starts it, so it is accounted for
	// only once the queue is VERIFIABLY empty — an unreadable /queue endpoint
	// is not evidence of drainage, and neither is silence.
	if r.cfg.concurrencyStrategy == "queue" && r.open != nil && r.runsQueued > 0 {
		if r.open.queueStatus != "ok" {
			return "queue_unobserved", fmt.Errorf(
				"%d arrival(s) came back as bare 202s but the concurrency queue is %q (%s), so queued work cannot be shown to have drained",
				r.runsQueued, r.open.queueStatus, r.open.queueReason)
		}
		if !r.open.queueDrainVerified {
			return "queue_not_drained", fmt.Errorf(
				"%d arrival(s) were queued and no successful queue read ever returned 0 (final depth %d)",
				r.runsQueued, r.open.queueFinal)
		}
		if r.open.queueFinal > 0 {
			return "queue_not_drained", fmt.Errorf(
				"the concurrency queue still holds %d row(s) after the drain, so queued arrivals are not accounted for",
				r.open.queueFinal)
		}
	}
	// Requested subscribers are a fan-out COST the workload claims to have
	// applied. A stream that ends early — an ordinary EOF included — stops
	// paying it, so coverage is enforced rather than merely reported.
	if r.cfg.subscribers > 0 && r.open != nil {
		if r.open.subscribers.mixNanos <= 0 {
			return "subscriber_coverage", fmt.Errorf(
				"%d subscriber(s) were requested but the covered interval could not be measured", r.cfg.subscribers)
		}
		if r.subscriberCoverage < minSubscriberCoverage {
			return "subscriber_coverage", fmt.Errorf(
				"%d subscriber(s) held the event stream for only %.1f%% of the measured interval (min %.0f%%, %d stream exit(s) before the window closed): the requested fan-out was not applied for most of the window",
				r.cfg.subscribers, r.subscriberCoverage*100, minSubscriberCoverage*100, r.open.subscribers.lost.Load())
		}
	}
	if !r.cfg.allowRunFailures && r.runsFailed+r.runsCancelled+r.runsSkipped > 0 {
		return "run_failure", fmt.Errorf("%d admitted run(s) ended non-succeeded (failed=%d cancelled=%d skipped=%d)",
			r.runsFailed+r.runsCancelled+r.runsSkipped, r.runsFailed, r.runsCancelled, r.runsSkipped)
	}
	if r.cfg.requireSustained {
		switch r.sustainedVerdict {
		case "sustained":
		case "inconclusive_insufficient_samples":
			return "backlog_inconclusive", errors.New(
				"too few in-window backlog samples to decide whether admission outran completion")
		case "inconclusive_queue_unobserved":
			return "backlog_inconclusive", errors.New(
				"the concurrency queue could not be observed for part of the arrival window, so outstanding work is unknown and sustained cannot be decided")
		default:
			return "backlog_growth", errors.New(
				"admission outran completion: the backlog grew across the arrival window, so a higher admitted count is queueing rather than throughput")
		}
	}
	return "", nil
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
// Open-loop arrivals
// ---------------------------------------------------------------------------

// Admission outcomes. Every offered arrival lands in exactly one of them, and
// they are deliberately NOT collapsed:
//
//   - dropped            the DRIVER could not offer it (in-flight cap, its own
//     scheduling lag, or the overall deadline). Nothing
//     reached the server; it is not a server rejection.
//   - admitted           DT-ADMIT-01: HTTP 202 WITH a run body carrying a UUID.
//   - queued_or_skipped  a bare 202 — the concurrency policy queued or skipped
//     the request. It is NOT an admission: no run identity
//     was acknowledged, so nothing can be reconciled by ID.
//   - rejected           the server said no (4xx/429/503, or any other status
//     that is not an acknowledged admission).
//   - transport_uncertain a timeout/disconnect. DT-QUORUM-01: possibly
//     committed. Never reported as a rejection; reconciled
//     afterwards against the server's own run census.
const (
	outcomeDropped   = "dropped"
	outcomeAdmitted  = "admitted"
	outcomeQueued    = "queued_or_skipped"
	outcomeRejected  = "rejected"
	outcomeUncertain = "transport_uncertain"
)

// arrival is one scheduled offer and everything later learned about it.
type arrival struct {
	index       int
	alias       string
	jobID       string
	scheduledAt time.Time
	offeredAt   time.Time
	settledAt   time.Time
	outcome     string
	reason      string
	statusCode  int
	runID       string

	reconciled     bool
	terminalStatus string
	snapshot       runSnapshot
	// reconciledAt is the DRIVER's own clock reading when it first observed a
	// terminal status. End-to-end latency pairs it with offeredAt; the server's
	// timestamps live in snapshot and are only ever paired with each other.
	reconciledAt time.Time
}

// ledger is the bounded arrival ledger. Its length is fixed by the arrival
// plan before the first request, which is what makes the accounting identity
// checkable instead of a running tally that can silently lose entries.
type ledger struct {
	mu       sync.Mutex
	arrivals []arrival

	// countersMu makes the offered/admitted/terminal triple COHERENT. They are
	// atomics so any single value can be read cheaply, but a backlog sample
	// reads all three and differences two of them: three independent Load()s
	// can interleave with an arrival that is admitted AND settled between the
	// admitted and terminal reads, which fabricates backlog = -1 and (since a
	// negative sample is a hard accounting_mismatch) fails a healthy workload.
	countersMu sync.Mutex
	admitted   atomic.Int64
	offered    atomic.Int64
	terminal   atomic.Int64
	// queueDepth is the most recent observed concurrency-queue depth, and
	// queueObserved says whether ANY successful observation stands behind it.
	// Without that flag a failing /queue endpoint silently republishes its last
	// good value (or zero) and the verdict reads a stale queue as drained.
	queueDepth    atomic.Int64
	queueObserved atomic.Bool
	// queueObsStatus/queueObsReason carry the queue-observation failure out to
	// the reporter: queue observation must fail CLOSED.
	queueObsStatus   string
	queueObsReason   string
	queueObsFailures int

	// census records, per job, the runs the SERVER reports for the workload
	// window — the only way to reconcile bare-202 and transport-uncertain
	// arrivals, which carry no run identity of their own.
	censusRuns     int
	censusAdmitted int
	censusExtra    int
	censusStatus   string
	censusReason   string
	censusTerminal int
	censusFailed   int
	// censusSeen is every census-discovered run id, so a second census does not
	// double-count what the first already queued for reconciliation.
	censusSeen map[string]bool
	// runBaseline is the set of run identities that existed BEFORE the window
	// opened, per job. Census membership is a set difference against it, which
	// needs no comparison between the server's clock and the driver's.
	runBaseline       map[string]map[string]bool
	runBaselineOK     bool
	runBaselineReason string
}

func newLedger(total int) *ledger {
	l := &ledger{arrivals: make([]arrival, total), censusStatus: "not_collected"}
	for i := range l.arrivals {
		l.arrivals[i] = arrival{index: i, outcome: outcomeDropped, reason: "not_reached"}
	}
	return l
}

func (l *ledger) set(i int, mutate func(*arrival)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	mutate(&l.arrivals[i])
}

func (l *ledger) snapshotArrivals() []arrival {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.arrivals)
}

// noteOffered/noteAdmitted/noteTerminal and counts are the ONLY writers and
// reader of the arrival counters. They share one mutex so a sample can never
// observe an admission without the settlement that already happened, which is
// what made backlog momentarily negative.
func (l *ledger) noteOffered() {
	l.countersMu.Lock()
	defer l.countersMu.Unlock()
	l.offered.Add(1)
}

func (l *ledger) noteAdmitted() {
	l.countersMu.Lock()
	defer l.countersMu.Unlock()
	l.admitted.Add(1)
}

func (l *ledger) noteTerminal() {
	l.countersMu.Lock()
	defer l.countersMu.Unlock()
	l.terminal.Add(1)
}

// counts returns a coherent offered/admitted/terminal triple.
func (l *ledger) counts() (offered, admitted, terminal int64) {
	l.countersMu.Lock()
	defer l.countersMu.Unlock()
	return l.offered.Load(), l.admitted.Load(), l.terminal.Load()
}

// noteQueueDepth records a successful queue observation; noteQueueFailure
// records that the queue could not be observed at all.
func (l *ledger) noteQueueDepth(total int64) {
	l.queueDepth.Store(total)
	l.queueObserved.Store(true)
}

func (l *ledger) noteQueueFailure(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queueObsFailures++
	if l.queueObsStatus != "unavailable" {
		l.queueObsStatus, l.queueObsReason = "unavailable", err.Error()
	}
}

// arrivalPlanFor returns the instant arrival i is scheduled for. Arrivals are
// placed on an absolute grid anchored at start, so a slow offer can never
// shift later arrivals: that is precisely what "independent of completion"
// means.
func arrivalInstant(start time.Time, i int, rate float64) time.Time {
	return start.Add(time.Duration(float64(i) / rate * float64(time.Second)))
}

// openLoopResult is what runOpenLoop hands back to the reporter.
type openLoopResult struct {
	windowStart time.Time
	// windowEnd is the PLANNED end of the arrival window; lastResponseAt is
	// when the final outstanding trigger response actually settled.
	windowEnd      time.Time
	plannedEnd     time.Time
	lastResponseAt time.Time
	drainEnd       time.Time
	backlog        []backlogSample
	lifecycle      *lifecycleReport
	resources      resourceReport
	apiReads       apiReadReport
	subscribers    subscriberReport
	queueDepths    []int
	queueFinal     int
	queueStatus    string
	queueReason    string
	// queueDrainVerified is true ONLY when a successful /queue read returned 0.
	// Absent it, bare-202 arrivals cannot be treated as accounted for.
	queueDrainVerified bool
	// queueObsStatus/queueObsReason/queueObsMissing carry in-window queue
	// observation out of the ledger for the reporter.
	queueObsStatus  string
	queueObsReason  string
	queueObsMissing bool
	// finalCensus* record the post-drain DT-QUORUM-01 sweep.
	finalCensusStatus string
	finalCensusReason string
	finalCensusLate   int
	// windowTruncated is true when the run context ended before the planned
	// arrival window closed, so the measured window is short of the plan.
	windowTruncated bool
	cacheHits       int
	cacheExecuted   int
	cacheTotal      int
	warmupRuns      int
	warmup          *warmupMetrics
}

// warmupMetrics is the cache warm-up's own evidence. The warm-up executes real,
// COLD runs before the arrival window opens; charging them to the measured
// deltas made the cache-hit workload a mixture of cold and cached work, so they
// are measured separately and the measured sample set restarts from a fresh
// baseline taken after the warm-up.
type warmupMetrics struct {
	runs       int
	seconds    float64
	rows       map[string]float64
	statements map[string]float64
}

// writeDeltas returns the per-category row and statement deltas between two
// metric samples.
func writeDeltas(from, to metricSample) (rows, statements map[string]float64) {
	rows = map[string]float64{
		"task_run_insert": to.taskRunInsert - from.taskRunInsert,
		"task_run_status": to.taskRunStatus - from.taskRunStatus,
		"event_insert":    to.eventInsert - from.eventInsert,
		"lease_renewal":   to.leaseRenewal - from.leaseRenewal,
		"callback":        to.callback - from.callback,
		"command":         to.command - from.command,
		"checkpoint":      to.checkpoint - from.checkpoint,
	}
	statements = map[string]float64{
		"task_run_insert": to.taskRunInsertStmts - from.taskRunInsertStmts,
		"task_run_status": to.taskRunStatusStmts - from.taskRunStatusStmts,
		"event_insert":    to.eventInsertStmts - from.eventInsertStmts,
		"lease_renewal":   to.leaseRenewalStmts - from.leaseRenewalStmts,
		"callback":        to.callbackStmts - from.callbackStmts,
		"command":         to.commandStmts - from.commandStmts,
		"checkpoint":      to.checkpointStmts - from.checkpointStmts,
	}
	return rows, statements
}

type backlogSample struct {
	at       time.Time
	offered  int64
	admitted int64
	terminal int64
	backlog  int64
	// queued is the server's OBSERVED concurrency-queue depth. Work parked in
	// the queue is outstanding work: without it a single-slot queue can grow
	// without bound while the acknowledged backlog sits at ~1 and the verdict
	// reads sustained. queueKnown is false when the workload uses the queue
	// strategy but no successful observation stands behind the value, which
	// makes the sustained verdict inconclusive rather than optimistic.
	queued     int64
	queueKnown bool
	// total is what the verdict judges: acknowledged backlog plus queued work.
	total int64
}

type resourceReport struct {
	status  string
	reason  string
	samples []containerStats
}

type apiReadReport struct {
	offered    int64
	dropped    int64
	ok         int64
	failed     int64
	statuses   map[int]int
	latencies  []time.Duration
	mu         sync.Mutex
	configured bool
}

func (a *apiReadReport) record(status int, d time.Duration, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.statuses == nil {
		a.statuses = map[int]int{}
	}
	if err != nil || status >= 300 {
		a.failed++
	} else {
		a.ok++
	}
	if status > 0 {
		a.statuses[status]++
	}
	if len(a.latencies) < 100000 {
		a.latencies = append(a.latencies, d)
	}
}

type subscriberReport struct {
	requested int
	connected atomic.Int64
	events    atomic.Int64
	// disconnects counts stream exits that carried an error; lost counts EVERY
	// stream exit before the mix interval closed, error or not. An ordinary
	// HTTP EOF is a lost subscriber too: without counting it, a stream that
	// delivers one frame and closes still satisfies a min-events expectation
	// while the rest of the window runs with no fan-out at all.
	disconnects atomic.Int64
	lost        atomic.Int64
	attempts    atomic.Int64
	// opened counts attempts that reached a confirmed SSE response; an attempt
	// that stalls before headers never becomes an opened stream.
	opened atomic.Int64
	// connectedNanos is the summed time subscriptions were actually open;
	// mixNanos is how long ONE subscriber was supposed to stay open.
	connectedNanos atomic.Int64
	mixNanos       int64
	mu             sync.Mutex
	errs           []string
}

// subscriberReconnectDelay keeps a server that closes every stream immediately
// from becoming a hot reconnect loop, while staying short enough that a healthy
// stream's coverage stays ~1.
const subscriberReconnectDelay = 100 * time.Millisecond

// minSubscriberCoverage is the enforced floor: the requested subscribers must
// hold the event stream open for at least this fraction of the measured
// interval, or the workload did not actually pay the fan-out cost it claims.
const minSubscriberCoverage = 0.9

// runSubscriber holds one event-stream subscription open for the whole mix
// interval, reconnecting whenever the stream ends early, and records how much
// of the interval was actually covered.
func (h *harness) runSubscriber(ctx context.Context, cursor uint64, out *subscriberReport) {
	out.connected.Add(1)
	for ctx.Err() == nil {
		out.attempts.Add(1)
		// Coverage counts from a CONFIRMED SSE response, not from the moment
		// the request was issued: a server that accepts the connection and
		// never sends headers pays none of the fan-out cost, yet timing the
		// whole call would have credited it with the full interval.
		var openedAt time.Time
		err := h.client.streamEventsWithOpen(ctx, "cursor="+strconv.FormatUint(cursor, 10),
			func() { openedAt = time.Now() },
			func(serverEvent) bool {
				out.events.Add(1)
				return true
			})
		if !openedAt.IsZero() {
			out.connectedNanos.Add(int64(time.Since(openedAt)))
			out.opened.Add(1)
		}
		if ctx.Err() != nil {
			// The mix interval closed: this is the expected end, not a loss.
			return
		}
		out.lost.Add(1)
		if err != nil {
			out.disconnects.Add(1)
			out.noteErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(subscriberReconnectDelay):
		}
	}
}

func (s *subscriberReport) noteErr(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) < 8 {
		s.errs = append(s.errs, err.Error())
	}
}

// reconcileItem is a run that must be driven to a terminal status. Items come
// from admissions and, after the window, from the server's run census.
type reconcileItem struct {
	jobID        string
	runID        string
	arrivalIndex int // -1 for census-discovered runs
}

// runOpenLoop is the open-loop driver.
//
// Arrivals are emitted from an absolute clock grid and NEVER wait on a
// completion or on the previous request's response. A bounded worker pool
// reconciles every admitted run to a terminal status concurrently, so backlog
// (admitted minus terminal) is observable while arrivals are still being
// offered.
func (h *harness) runOpenLoop(ctx context.Context, jobs []appliedJob, lg *ledger, obs *eventObserver, baselineCursor uint64) *openLoopResult {
	cfg := h.cfg
	res := &openLoopResult{queueStatus: "not_collected", finalCensusStatus: "not_collected"}

	// Identity baseline BEFORE anything is offered: census membership is a set
	// difference against it, never a server-vs-driver clock comparison.
	baselineCtx, baselineCancel := context.WithTimeout(ctx, 60*time.Second)
	h.collectRunBaseline(baselineCtx, jobs, lg)
	baselineCancel()

	// Bounded by the arrival plan, which validation already caps, so the driver
	// keeps no unbounded queue: at most one admitted run per arrival, plus at
	// most one census-discovered run per arrival (a queued arrival becomes a
	// run of its own once the dequeuer starts it).
	reconcile := make(chan reconcileItem, 2*len(lg.arrivals)+8)
	var reconcilers sync.WaitGroup
	drainCtx, drainCancel := context.WithCancel(ctx)
	defer drainCancel()

	// The measured window opens here, and because it is fixed by the PLAN the
	// drain deadline is knowable before the first arrival. ONE deadline context
	// therefore governs everything that runs inside the drain — the reconcilers,
	// queue polling and the census reads — so -drain-timeout cannot be deferred
	// by a slow census read or an open queue poll.
	res.windowStart = time.Now()
	res.plannedEnd = res.windowStart.Add(cfg.arrivalWindow)
	drainDeadline := res.plannedEnd.Add(cfg.drainTimeout)
	deadlineCtx, deadlineCancel := context.WithDeadline(drainCtx, drainDeadline)
	defer deadlineCancel()

	for range cfg.reconcileWorkers {
		reconcilers.Add(1)
		go func() {
			defer reconcilers.Done()
			for item := range reconcile {
				h.reconcileRun(deadlineCtx, item, lg)
			}
		}()
	}

	// Background mixes: read traffic and passive subscribers run for the whole
	// workload, so their cost is inside the measured interval rather than
	// beside it.
	mixCtx, mixCancel := context.WithCancel(ctx)
	var mixes sync.WaitGroup
	res.apiReads.configured = cfg.apiReadRate > 0
	if cfg.apiReadRate > 0 {
		mixes.Add(1)
		go func() { defer mixes.Done(); h.runAPIReadMix(mixCtx, jobs, &res.apiReads) }()
	}
	res.subscribers.requested = cfg.subscribers
	mixStart := time.Now()
	for range cfg.subscribers {
		mixes.Add(1)
		go func() {
			defer mixes.Done()
			h.runSubscriber(mixCtx, baselineCursor, &res.subscribers)
		}()
	}

	// Sampling of backlog and external container resources runs on the same
	// cadence as the metric sampler.
	sampleCtx, sampleCancel := context.WithCancel(ctx)
	var samplers sync.WaitGroup
	samplers.Add(1)
	go func() { defer samplers.Done(); h.sampleBacklog(sampleCtx, jobs, lg, res) }()
	samplers.Add(1)
	go func() { defer samplers.Done(); h.sampleResources(sampleCtx, &res.resources) }()

	inFlight := make(chan struct{}, cfg.maxInFlight)
	var offers sync.WaitGroup
	total := len(lg.arrivals)
	for i := 0; i < total; i++ {
		scheduled := arrivalInstant(res.windowStart, i, cfg.rate)
		if wait := time.Until(scheduled); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				h.dropRemaining(lg, i, total, "deadline_before_offer")
				i = total
				continue
			case <-timer.C:
			}
			timer.Stop()
		}
		if ctx.Err() != nil {
			h.dropRemaining(lg, i, total, "deadline_before_offer")
			break
		}
		now := time.Now()
		if now.Sub(scheduled) > cfg.lateBudget {
			// Offering it now would be a catch-up burst nobody asked for. An
			// arrival the driver could not place on time is the driver's own
			// loss, and is counted as such.
			lg.set(i, func(a *arrival) {
				a.scheduledAt, a.offeredAt = scheduled, now
				a.outcome, a.reason = outcomeDropped, "client_scheduler_lag"
			})
			lg.noteOffered()
			continue
		}
		select {
		case inFlight <- struct{}{}:
		default:
			lg.set(i, func(a *arrival) {
				a.scheduledAt, a.offeredAt = scheduled, now
				a.outcome, a.reason = outcomeDropped, "client_in_flight_cap"
			})
			lg.noteOffered()
			continue
		}
		offers.Add(1)
		go func(index int, scheduled time.Time) {
			defer offers.Done()
			defer func() { <-inFlight }()
			job := jobs[index%len(jobs)]
			// Offers run under the SAME absolute deadline as the drain: an
			// offer still outstanding when it expires is cancelled and recorded
			// as transport_uncertain (possibly committed), never left to run
			// past the bound on the overall run context.
			h.offer(deadlineCtx, job, index, scheduled, lg, reconcile)
		}(i, scheduled)
	}
	// The arrival WINDOW ends when the plan says it does — and measurement must
	// actually RUN until then. The last arrival is scheduled at
	// windowStart+(n-1)/rate, so returning as soon as the offers settle stopped
	// sampling, subscribers and the read mix up to 1/rate early (for a sparse
	// plan, almost immediately) while the report still claimed the full window
	// and the drain duration could come out negative.
	offers.Wait()
	if wait := time.Until(res.plannedEnd); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			res.windowTruncated = true
		}
		timer.Stop()
	}
	res.windowEnd = res.plannedEnd
	res.lastResponseAt = time.Now()

	// Drain. The arrival window is closed; every acknowledged run, plus every
	// run the server's census attributes to this workload, must now reach a
	// terminal status inside the deadline established above.
	// A queue-strategy workload's excess arrivals became queue rows, not runs.
	// The census can only see the runs the dequeuer has already started, so
	// wait for the queue to empty first — inside that SAME deadline.
	// The queue wait gets most of the drain, never all of it: a queue that never
	// empties must not consume the whole deadline and leave the census — the
	// only reconciliation surface for identity-less arrivals — no time at all.
	queueDeadline := time.Now().Add(3 * time.Until(drainDeadline) / 4)
	queueCtx, queueCancel := context.WithDeadline(deadlineCtx, queueDeadline)
	h.waitForQueueDrain(queueCtx, jobs, res, queueDeadline)
	queueCancel()
	h.collectCensus(deadlineCtx, jobs, lg, res, reconcile)
	close(reconcile)
	done := make(chan struct{})
	go func() { reconcilers.Wait(); close(done) }()
	select {
	case <-done:
	case <-deadlineCtx.Done():
		drainCancel()
		<-done
	}
	// The server admits on a background context (internal/run/store.go), so a
	// disconnected transport_uncertain offer can still commit AFTER the first
	// census. While any uncertain offer exists, take a FINAL census inside the
	// same deadline and settle anything new it finds.
	h.finalCensus(deadlineCtx, jobs, lg, res)
	res.drainEnd = time.Now()

	sampleCancel()
	samplers.Wait()
	mixCancel()
	mixes.Wait()
	// The denominator of subscriber coverage: how long the fan-out was supposed
	// to be applied for.
	res.subscribers.mixNanos = int64(time.Since(mixStart))

	h.collectQueueDepth(ctx, jobs, res, drainDeadline)
	lg.mu.Lock()
	res.queueObsStatus, res.queueObsReason = lg.queueObsStatus, lg.queueObsReason
	lg.mu.Unlock()
	h.summarizeCache(lg, res)
	res.lifecycle = h.buildLifecycle(ctx, lg, obs, baselineCursor)
	return res
}

// dropRemaining accounts for arrivals the plan contained but the driver never
// offered, so offered = dropped + attempted always holds.
func (h *harness) dropRemaining(lg *ledger, from, to int, reason string) {
	for i := from; i < to; i++ {
		lg.set(i, func(a *arrival) {
			if a.reason == "not_reached" {
				a.outcome, a.reason = outcomeDropped, reason
			}
		})
		lg.noteOffered()
	}
}

// triggerBody decides what POST /v1/jobs/:id/run carries.
//
// Run parameters are part of a task's cache identity, so a per-arrival nonce is
// what makes an open-loop workload measure EXECUTION. Without it, a server with
// task caching enabled short-circuits every repeat of the same job and the
// "load" test measures cache lookups: honest numbers, but not the ones the
// workload asked for. cache=hit deliberately omits the nonce so the window's
// arrivals share the warm-up run's identity and DO hit.
func (h *harness) triggerBody(index int) any {
	if h.cfg.cacheMode == cacheHit {
		return nil
	}
	label := h.cfg.workload
	if label == "" {
		label = "load"
	}
	return map[string]any{"params": map[string]string{
		// The invocation id is what stops a SECOND run of the same workload,
		// inside the cache TTL, from being all hits: label+index alone repeats
		// across invocations and so reproduces the previous run's identity.
		"caesium_load_arrival": fmt.Sprintf("%s-%s-%d", label, h.invocationID, index),
	}}
}

// offer performs exactly one trigger request and classifies its outcome.
func (h *harness) offer(ctx context.Context, job appliedJob, index int, scheduled time.Time, lg *ledger, reconcile chan<- reconcileItem) {
	offeredAt := time.Now()
	lg.noteOffered()
	resp, err := h.client.do(ctx, http.MethodPost, "/v1/jobs/"+job.id+"/run", h.triggerBody(index))
	settledAt := time.Now()
	record := func(outcome, reason string, code int, runID string) {
		lg.set(index, func(a *arrival) {
			a.alias, a.jobID = job.alias, job.id
			a.scheduledAt, a.offeredAt, a.settledAt = scheduled, offeredAt, settledAt
			a.outcome, a.reason, a.statusCode, a.runID = outcome, reason, code, runID
		})
	}
	if err != nil {
		// DT-QUORUM-01: a lost response is POSSIBLY COMMITTED. Do not call it
		// a rejection and do not retry it as if it were idempotent.
		record(outcomeUncertain, "transport: "+err.Error(), 0, "")
		return
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	code := resp.StatusCode
	if readErr != nil {
		// Headers arrived but the body did not. The run may well have been
		// created, so this is DT-QUORUM-01 uncertainty — not a queue/skip.
		record(outcomeUncertain, "response body truncated after HTTP "+strconv.Itoa(code)+": "+readErr.Error(), code, "")
		return
	}
	switch {
	case code == http.StatusAccepted:
		var body struct {
			ID string `json:"id"`
		}
		if jsonErr := json.Unmarshal(raw, &body); jsonErr != nil || strings.TrimSpace(body.ID) == "" {
			// A bare 202 is the concurrency policy's queue/skip answer. It
			// acknowledges no run identity, so it is its own outcome.
			record(outcomeQueued, "202 without a run body", code, "")
			return
		}
		if !looksLikeUUID(body.ID) {
			record(outcomeRejected, "202 body carried a non-UUID run id", code, "")
			return
		}
		record(outcomeAdmitted, "", code, body.ID)
		lg.noteAdmitted()
		select {
		case reconcile <- reconcileItem{jobID: job.id, runID: body.ID, arrivalIndex: index}:
		default:
			// Impossible while the channel is sized from the arrival plan;
			// recorded rather than dropped silently if it ever is not.
			lg.set(index, func(a *arrival) { a.reason = "reconcile_queue_full" })
		}
	case code >= 400:
		record(outcomeRejected, strings.TrimSpace(truncate(string(raw), 200)), code, "")
	default:
		record(outcomeRejected, "unexpected non-admission status", code, "")
	}
}

func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
				return false
			}
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// reconcileRun polls one run to a terminal status. It is the only thing that
// may mark an admitted arrival reconciled.
func (h *harness) reconcileRun(ctx context.Context, item reconcileItem, lg *ledger) {
	tick := time.NewTicker(h.cfg.pollInterval)
	defer tick.Stop()
	for {
		snap, err := h.client.getRun(ctx, item.jobID, item.runID)
		if err == nil && snap.terminal() {
			if item.arrivalIndex >= 0 {
				// Acknowledged admissions and census-discovered runs are
				// SEPARATE populations. Counting a discovered run into
				// lg.terminal (which is differenced against lg.admitted, a
				// count of UUID-bearing 202s only) drove the backlog negative:
				// the queue workload admitted 7 and settled 30.
				lg.noteTerminal()
				observedAt := time.Now()
				lg.set(item.arrivalIndex, func(a *arrival) {
					a.reconciled, a.terminalStatus, a.snapshot = true, snap.Status, snap
					a.reconciledAt = observedAt
				})
			} else {
				h.recordCensusTerminal(lg, snap.Status)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// recordCensusTerminal settles a census-discovered run. It deliberately does
// NOT touch lg.terminal: that counter is differenced against lg.admitted,
// which counts acknowledged UUID-bearing 202s only.
func (h *harness) recordCensusTerminal(lg *ledger, status string) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	lg.censusTerminal++
	if status != "succeeded" {
		lg.censusFailed++
	}
}

// sampleBacklog records the admitted-minus-terminal backlog on the metric
// sampling cadence. Backlog is what makes "admission got faster" checkable:
// admitting more while the backlog climbs is queueing, not throughput.
func (h *harness) sampleBacklog(ctx context.Context, jobs []appliedJob, lg *ledger, res *openLoopResult) {
	tick := time.NewTicker(h.cfg.sampleRate)
	defer tick.Stop()
	queueMatters := h.cfg.concurrencyStrategy == "queue"
	record := func() {
		offered, admitted, terminal := lg.counts()
		queued, queueKnown := lg.queueDepth.Load(), true
		if queueMatters && !lg.queueObserved.Load() {
			// No successful /queue read stands behind this value. Reporting it
			// as zero would let an unobservable queue read as drained.
			queued, queueKnown = 0, false
		}
		res.backlog = append(res.backlog, backlogSample{
			at: time.Now(), offered: offered, admitted: admitted, terminal: terminal,
			backlog: admitted - terminal, queued: queued, queueKnown: queueKnown,
			total: admitted - terminal + queued,
		})
	}
	// Poll the concurrency queue on the sampling cadence so queued work is
	// inside the in-window growth decision, not only observed after the drain.
	// The FIRST read is synchronous: otherwise the first sample is always taken
	// before any observation exists and a perfectly healthy queue reads as
	// unobserved for the whole window.
	if queueMatters {
		h.pollQueueOnce(ctx, jobs, lg)
		go h.pollQueueDepth(ctx, jobs, lg)
	}
	record()
	for {
		select {
		case <-ctx.Done():
			record()
			return
		case <-tick.C:
			record()
		}
	}
}

// pollQueueDepth keeps the ledger's observed queue depth fresh while arrivals
// are still being offered.
func (h *harness) pollQueueDepth(ctx context.Context, jobs []appliedJob, lg *ledger) {
	tick := time.NewTicker(h.cfg.sampleRate)
	defer tick.Stop()
	for {
		h.pollQueueOnce(ctx, jobs, lg)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pollQueueOnce takes one queue-depth observation across every job, recording
// either the depth or the failure.
func (h *harness) pollQueueOnce(ctx context.Context, jobs []appliedJob, lg *ledger) {
	total := int64(0)
	var failure error
	for _, job := range jobs {
		depth, err := h.client.queueDepth(ctx, job.id)
		if err != nil {
			failure = err
			break
		}
		total += int64(depth)
	}
	switch {
	case failure == nil:
		lg.noteQueueDepth(total)
	case ctx.Err() == nil:
		// A failed read must not leave the previous value standing as if it
		// were current: record the failure so the verdict can fail closed.
		lg.queueObserved.Store(false)
		lg.noteQueueFailure(failure)
	}
}

// sampleResources observes the server container from outside on the same
// cadence. Unavailability is recorded with a reason, never as a zero reading.
func (h *harness) sampleResources(ctx context.Context, out *resourceReport) {
	if h.cfg.resourceContainer == "" {
		out.status, out.reason = "unavailable", "not_configured: pass -resource-container to observe the server container"
		return
	}
	statsClient, err := newDockerStatsClient(h.cfg.dockerHost)
	if err != nil {
		out.status, out.reason = "unavailable", err.Error()
		return
	}
	tick := time.NewTicker(h.cfg.sampleRate)
	defer tick.Stop()
	sample := func() {
		s, err := statsClient.sample(ctx, h.cfg.resourceContainer)
		if err != nil {
			if out.reason == "" {
				out.reason = err.Error()
			}
			return
		}
		out.samples = append(out.samples, s)
	}
	sample()
	for {
		select {
		case <-ctx.Done():
			if len(out.samples) == 0 {
				out.status = "unavailable"
				if out.reason == "" {
					out.reason = "no successful container stats sample"
				}
			} else {
				out.status, out.reason = "ok", ""
			}
			return
		case <-tick.C:
			sample()
		}
	}
}

// runAPIReadMix issues public read traffic on its own open-loop schedule,
// bounded by the same in-flight cap discipline as arrivals.
func (h *harness) runAPIReadMix(ctx context.Context, jobs []appliedJob, out *apiReadReport) {
	paths := []string{"/v1/jobs", "/v1/stats/summary"}
	for _, j := range jobs {
		paths = append(paths, "/v1/jobs/"+j.id+"/runs?limit=10")
		if len(paths) >= 8 {
			break
		}
	}
	inFlight := make(chan struct{}, h.cfg.maxInFlight)
	var wg sync.WaitGroup
	defer wg.Wait()
	start := time.Now()
	// Bound the read plan the same way the arrival plan is bounded: enough
	// reads to cover the whole measured interval, and not one more.
	maxReads := int(h.cfg.apiReadRate*(h.cfg.arrivalWindow+h.cfg.drainTimeout).Seconds()) + 1
	for i := 0; i < maxReads; i++ {
		scheduled := arrivalInstant(start, i, h.cfg.apiReadRate)
		if wait := time.Until(scheduled); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		atomic.AddInt64(&out.offered, 1)
		select {
		case inFlight <- struct{}{}:
		default:
			atomic.AddInt64(&out.dropped, 1)
			continue
		}
		path := paths[i%len(paths)]
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			defer func() { <-inFlight }()
			began := time.Now()
			resp, err := h.client.do(ctx, http.MethodGet, path, nil)
			status := 0
			if resp != nil {
				status = resp.StatusCode
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
			}
			out.record(status, time.Since(began), err)
		}(path)
	}
}

// collectCensus asks the server which runs it actually holds for each job in
// the workload window. It is the reconciliation surface for arrivals that
// carry no run identity: bare-202 queue/skip answers and transport-uncertain
// offers whose write may still have committed.
func (h *harness) collectCensus(ctx context.Context, jobs []appliedJob, lg *ledger, res *openLoopResult, reconcile chan<- reconcileItem) {
	admittedByJob := map[string]map[string]bool{}
	for _, a := range lg.snapshotArrivals() {
		if a.outcome == outcomeAdmitted && a.runID != "" {
			if admittedByJob[a.jobID] == nil {
				admittedByJob[a.jobID] = map[string]bool{}
			}
			admittedByJob[a.jobID][a.runID] = true
		}
	}
	// The caller's context already carries the single drain deadline.
	censusCtx := ctx
	// Membership is decided by run IDENTITY against a baseline taken before the
	// window opened, never by comparing a SERVER timestamp with the DRIVER's
	// clock: that comparison silently mis-attributes runs under clock skew
	// larger than its slack, in either direction. If the baseline could not be
	// taken, membership is not establishable and the census fails explicitly.
	lg.mu.Lock()
	baseline, baselineOK := lg.runBaseline, lg.runBaselineOK
	lg.mu.Unlock()
	if !baselineOK {
		lg.mu.Lock()
		lg.censusStatus, lg.censusReason = "unavailable",
			"run-identity baseline unavailable, so census membership cannot be established without comparing server and driver clocks"
		lg.mu.Unlock()
		return
	}
	totalRuns, totalAdmitted, extra := 0, 0, 0
	for _, job := range jobs {
		runs, err := h.client.listRuns(censusCtx, job.id, 16)
		if err != nil {
			lg.mu.Lock()
			lg.censusStatus, lg.censusReason = "unavailable", err.Error()
			lg.mu.Unlock()
			return
		}
		for _, r := range runs {
			if baseline[job.id][r.ID] {
				// Predates this invocation.
				continue
			}
			totalRuns++
			if admittedByJob[job.id][r.ID] {
				totalAdmitted++
				continue
			}
			extra++
			lg.mu.Lock()
			if lg.censusSeen == nil {
				lg.censusSeen = map[string]bool{}
			}
			lg.censusSeen[r.ID] = true
			lg.mu.Unlock()
			select {
			case reconcile <- reconcileItem{jobID: job.id, runID: r.ID, arrivalIndex: -1}:
			default:
			}
		}
	}
	lg.mu.Lock()
	lg.censusStatus, lg.censusReason = "ok", ""
	lg.censusRuns, lg.censusAdmitted, lg.censusExtra = totalRuns, totalAdmitted, extra
	lg.mu.Unlock()
}

// collectRunBaseline records the run identities that already existed before the
// arrival window opened. It is what makes census membership a set difference
// rather than a cross-clock timestamp comparison.
func (h *harness) collectRunBaseline(ctx context.Context, jobs []appliedJob, lg *ledger) {
	baseline := map[string]map[string]bool{}
	for _, job := range jobs {
		runs, err := h.client.listRuns(ctx, job.id, 16)
		if err != nil {
			lg.mu.Lock()
			lg.runBaselineOK, lg.runBaselineReason = false, err.Error()
			lg.mu.Unlock()
			return
		}
		ids := map[string]bool{}
		for _, r := range runs {
			ids[r.ID] = true
		}
		baseline[job.id] = ids
	}
	lg.mu.Lock()
	lg.runBaseline, lg.runBaselineOK = baseline, true
	lg.mu.Unlock()
}

// finalCensus closes DT-QUORUM-01's remaining window: the server admits on a
// background context, so a transport_uncertain offer whose connection died can
// still commit AFTER the first census. While any uncertain offer exists, look
// once more and settle whatever is new, inside the same drain deadline.
func (h *harness) finalCensus(ctx context.Context, jobs []appliedJob, lg *ledger, res *openLoopResult) {
	uncertain := 0
	for _, a := range lg.snapshotArrivals() {
		if a.outcome == outcomeUncertain {
			uncertain++
		}
	}
	if uncertain == 0 {
		res.finalCensusStatus = "not_required"
		return
	}
	lg.mu.Lock()
	baseline, baselineOK := lg.runBaseline, lg.runBaselineOK
	admitted := map[string]bool{}
	seen := map[string]bool{}
	for id := range lg.censusSeen {
		seen[id] = true
	}
	lg.mu.Unlock()
	if !baselineOK {
		res.finalCensusStatus, res.finalCensusReason = "unavailable", "run-identity baseline unavailable"
		lg.mu.Lock()
		lg.censusStatus, lg.censusReason = "unavailable", "final census could not establish membership"
		lg.mu.Unlock()
		return
	}
	for _, a := range lg.snapshotArrivals() {
		if a.outcome == outcomeAdmitted && a.runID != "" {
			admitted[a.runID] = true
		}
	}
	var late []reconcileItem
	for _, job := range jobs {
		runs, err := h.client.listRuns(ctx, job.id, 16)
		if err != nil {
			res.finalCensusStatus, res.finalCensusReason = "unavailable", err.Error()
			lg.mu.Lock()
			lg.censusStatus, lg.censusReason = "unavailable", "final census read failed: "+err.Error()
			lg.mu.Unlock()
			return
		}
		for _, r := range runs {
			if baseline[job.id][r.ID] || admitted[r.ID] || seen[r.ID] {
				continue
			}
			late = append(late, reconcileItem{jobID: job.id, runID: r.ID, arrivalIndex: -1})
		}
	}
	res.finalCensusStatus, res.finalCensusLate = "ok", len(late)
	if len(late) == 0 {
		return
	}
	// The reconciler pool has already been joined, so settle these here — still
	// inside the drain deadline. Anything left unsettled leaves
	// censusExtra != censusTerminal, which judgeOpenLoop already fails.
	lg.mu.Lock()
	if lg.censusSeen == nil {
		lg.censusSeen = map[string]bool{}
	}
	for _, item := range late {
		lg.censusSeen[item.runID] = true
		lg.censusExtra++
		lg.censusRuns++
	}
	lg.mu.Unlock()
	for _, item := range late {
		if ctx.Err() != nil {
			return
		}
		h.reconcileRun(ctx, item, lg)
	}
}

// waitForQueueDrain polls the concurrency queue until it is empty or the drain
// deadline passes, recording the depths it observed. It never asserts: a queue
// that does not empty is reported through queue_depth_final.
func (h *harness) waitForQueueDrain(ctx context.Context, jobs []appliedJob, res *openLoopResult, deadline time.Time) {
	if h.cfg.concurrencyStrategy != "queue" {
		return
	}
	tick := time.NewTicker(h.cfg.pollInterval)
	defer tick.Stop()
	for {
		total := 0
		var failure error
		for _, job := range jobs {
			depth, err := h.client.queueDepth(ctx, job.id)
			if err != nil {
				failure = err
				break
			}
			total += depth
		}
		if failure != nil {
			// Returning silently made queue observation fail OPEN: the drain
			// looked complete because nothing said otherwise.
			res.queueStatus, res.queueReason = "unavailable", failure.Error()
			return
		}
		res.queueDepths = append(res.queueDepths, total)
		if total == 0 {
			// The ONLY place drainage is verified rather than assumed.
			res.queueDrainVerified = true
			return
		}
		select {
		case <-ctx.Done():
			res.queueStatus, res.queueReason = "unavailable", "drain deadline expired with the queue still occupied"
			return
		case <-tick.C:
			if time.Now().After(deadline) {
				res.queueStatus, res.queueReason = "unavailable", "drain deadline expired with the queue still occupied"
				return
			}
		}
	}
}

// collectQueueDepth reports the concurrency queue's residual depth after the
// drain. A drain that leaves work queued is reported, never rounded to zero.
func (h *harness) collectQueueDepth(ctx context.Context, jobs []appliedJob, res *openLoopResult, deadline time.Time) {
	if h.cfg.concurrencyStrategy != "queue" {
		res.queueStatus, res.queueReason = "not_applicable", "workload does not use the queue concurrency strategy"
		return
	}
	// The drain deadline has already expired by the time this runs, so this
	// residual read needs its own bound. It is an ABSOLUTE cap anchored on that
	// same deadline, so a late-starting or stalled read cannot extend the run:
	// neither a hard-coded 30 s nor the overall run context.
	bound := min(max(h.cfg.drainTimeout, time.Second), 30*time.Second)
	queueCtx, cancel := context.WithDeadline(ctx, deadline.Add(bound))
	defer cancel()
	total := 0
	for _, job := range jobs {
		depth, err := h.client.queueDepth(queueCtx, job.id)
		if err != nil {
			res.queueStatus, res.queueReason = "unavailable", err.Error()
			return
		}
		total += depth
	}
	// This read is later and authoritative, so it supersedes an earlier
	// in-drain failure — including as the verification that the queue drained.
	res.queueStatus, res.queueReason, res.queueFinal = "ok", "", total
	if total == 0 {
		res.queueDrainVerified = true
	}
}

// summarizeCache aggregates the server's own per-run cache accounting from the
// terminal run reads the reconciler already fetched.
func (h *harness) summarizeCache(lg *ledger, res *openLoopResult) {
	for _, a := range lg.snapshotArrivals() {
		if !a.reconciled {
			continue
		}
		res.cacheHits += a.snapshot.CacheHits
		res.cacheExecuted += a.snapshot.ExecutedTasks
		res.cacheTotal += a.snapshot.TotalTasks
	}
}

// buildLifecycle derives every declared lifecycle interval from observed
// events and from the public run read's server timestamps. Each interval that
// could not be observed carries an explicit reason.
func (h *harness) buildLifecycle(ctx context.Context, lg *ledger, obs *eventObserver, baselineCursor uint64) *lifecycleReport {
	out := &lifecycleReport{eventsStatus: "disabled", eventsReason: "lifecycle observation disabled"}
	arrivals := lg.snapshotArrivals()

	if h.cfg.lifecycle && obs != nil {
		// Re-read the persisted backlog after the drain. DT-EVENT-01 makes
		// live delivery at-least-once WITH possible subscriber-buffer drops,
		// so the durable catch-up is what closes the gaps; dedupe by identity
		// merges it with what the live stream already saw.
		catchupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		read, truncated, err := h.client.readUntilIdle(catchupCtx,
			"cursor="+strconv.FormatUint(baselineCursor, 10), 2*time.Second, 200000, obs.observe)
		cancel()
		_ = read
		obs.truncated = obs.truncated || truncated
		obs.noteError(err)
		obs.mu.Lock()
		out.received, out.duplicates, out.truncated = obs.received, obs.duplicates, obs.truncated
		streamErrs := slices.Clone(obs.streamErrs)
		obs.mu.Unlock()
		switch {
		case out.received == 0:
			out.eventsStatus = "unavailable"
			out.eventsReason = "no events observed"
			if len(streamErrs) > 0 {
				out.eventsReason = strings.Join(streamErrs, "; ")
			}
		case len(streamErrs) > 0:
			out.eventsStatus, out.eventsReason = "degraded", strings.Join(streamErrs, "; ")
		default:
			out.eventsStatus, out.eventsReason = "ok", ""
		}
	}

	admissionToTask := out.interval("admission_to_first_task_start", "events")
	taskQueueWait := out.interval("task_queue_wait", "events")
	taskExecution := out.interval("task_execution", "events")
	runTerminal := out.interval("run_terminal", "events")
	readAdmissionToStart := out.interval("read_admission_to_run_start", "run_read")
	readRunTotal := out.interval("read_run_total", "run_read")
	readTaskPending := out.interval("read_task_pending", "run_read")
	readTaskExecution := out.interval("read_task_execution", "run_read")

	unavailableReason := "event_stream_unavailable"
	if out.eventsStatus == "disabled" {
		unavailableReason = "lifecycle_disabled"
	}

	for _, a := range arrivals {
		if a.outcome != outcomeAdmitted || a.runID == "" {
			continue
		}
		// ---- events source ------------------------------------------------
		if out.eventsStatus == "disabled" || out.eventsStatus == "unavailable" {
			admissionToTask.mark(unavailableReason)
			runTerminal.mark(unavailableReason)
		} else {
			// Deep-copy under the lock: the live subscriber is still calling
			// observe(), so iterating the shared tasks map here is a data race
			// (and a concurrent map iteration panic waiting to happen).
			timeline := obs.snapshotRun(a.runID)
			if timeline == nil {
				admissionToTask.mark("no_observed_event_for_run")
				runTerminal.mark("no_observed_event_for_run")
			} else {
				var firstTaskStart time.Time
				allCached := len(timeline.tasks) > 0
				for _, t := range timeline.tasks {
					if !t.cached {
						allCached = false
					}
					if !t.started.IsZero() && (firstTaskStart.IsZero() || t.started.Before(firstTaskStart)) {
						firstTaskStart = t.started
					}
					switch {
					case t.cached && t.started.IsZero():
						// A cache hit never starts a container, so there is no
						// queue wait or execution interval to report.
						taskQueueWait.mark("cache_hit_no_task_start")
						taskExecution.mark("cache_hit_no_task_start")
					case t.started.IsZero():
						taskQueueWait.mark("no_task_started_event")
						taskExecution.mark("no_task_started_event")
					default:
						taskQueueWait.between(t.ready, t.started, "no_task_ready_event", "no_task_started_event")
						taskExecution.between(t.started, t.terminal, "no_task_started_event", "no_task_terminal_event")
					}
				}
				switch {
				case firstTaskStart.IsZero() && allCached:
					admissionToTask.mark("cache_hit_no_task_start")
				case firstTaskStart.IsZero():
					admissionToTask.mark("no_task_started_event")
				default:
					admissionToTask.between(timeline.started, firstTaskStart, "no_run_started_event", "no_task_started_event")
				}
				runTerminal.between(timeline.started, timeline.terminal, "no_run_started_event", "no_run_terminal_event")
			}
		}

		// ---- public run read source ---------------------------------------
		if !a.reconciled {
			readAdmissionToStart.mark("run_not_reconciled")
			readRunTotal.mark("run_not_reconciled")
			readTaskPending.mark("run_not_reconciled")
			readTaskExecution.mark("run_not_reconciled")
			continue
		}
		snap := a.snapshot
		readAdmissionToStart.between(snap.CreatedAt, snap.StartedAt, "no_created_at", "no_started_at")
		if snap.CompletedAt == nil {
			readRunTotal.mark("no_completed_at")
		} else {
			readRunTotal.between(snap.CreatedAt, *snap.CompletedAt, "no_created_at", "no_completed_at")
		}
		if len(snap.Tasks) == 0 {
			readTaskPending.mark("run_read_returned_no_tasks")
			readTaskExecution.mark("run_read_returned_no_tasks")
		}
		for _, t := range snap.Tasks {
			switch {
			case t.CacheHit && t.StartedAt == nil:
				readTaskPending.mark("cache_hit_no_task_start")
				readTaskExecution.mark("cache_hit_no_task_start")
			case t.StartedAt == nil:
				readTaskPending.mark("no_task_started_at")
				readTaskExecution.mark("no_task_started_at")
			case t.CompletedAt == nil:
				readTaskPending.between(t.CreatedAt, *t.StartedAt, "no_task_created_at", "no_task_started_at")
				readTaskExecution.mark("no_task_completed_at")
			default:
				readTaskPending.between(t.CreatedAt, *t.StartedAt, "no_task_created_at", "no_task_started_at")
				readTaskExecution.between(*t.StartedAt, *t.CompletedAt, "no_task_started_at", "no_task_completed_at")
			}
		}
	}
	return out
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

	// ---- E2: open-loop accounting ---------------------------------------
	expected           int
	runsDropped        int
	runsRejected       int
	runsQueued         int
	runsUncertain      int
	runsUnreconciled   int
	dropReasons        map[string]int
	rejectStatusCodes  map[int]int
	identityOK         bool
	identityDetail     string
	censusStatus       string
	censusReason       string
	censusExtra        int
	censusTerminal     int
	censusFailed       int
	open               *openLoopResult
	ledger             *ledger
	backlogPeak        int64
	backlogAtClose     int64
	backlogFinal       int64
	backlogSlope       float64
	backlogSamplesUsed int
	backlogFirstMean   float64
	backlogSecondMean  float64
	backlogTolerance   float64
	backlogMeanGrowth  float64
	backlogSlopeRise   float64
	backlogNegative    bool
	subscriberCoverage float64
	sustainedVerdict   string
	offeredPerSecond   float64
	admittedPerSecond  float64
	completedPerSecond float64
	windowSeconds      float64
	drainSeconds       float64
}

// sustainedTolerance is how much of the work offered during one half of the
// arrival window may accumulate as extra backlog and still count as steady
// state. Above it, admission is outrunning completion and a higher admitted
// count is queueing, not throughput.
//
// The allowance is expressed as a backlog INCREASE ACROSS ONE HALF-WINDOW
// (tolerance = sustainedTolerance * rate * halfSeconds, floored at one run),
// not as a per-second slope bound. That distinction is the whole point of the
// unit: a per-second bound is under-determined at low arrival rates. At
// rate 0.5/s the old bound was 0.2*0.5 = 0.1 backlog/s, while ONE unit of
// backlog jitter across an ~10 s half-window already fits a slope of ~0.1/s —
// the decision threshold sat inside the quantisation noise of a single run, so
// a perfectly stationary system could be failed by ±1 queued run. Scaling the
// allowance with the measurement window removes that dependence.
const sustainedTolerance = 0.2

// minBacklogSamplesPerHalf is the evidence floor for the verdict. Fewer
// samples than this in either half makes the result inconclusive — never a
// pass. It also keeps the verdict away from the regime where one integer of
// backlog jitter dominates the measurement.
const minBacklogSamplesPerHalf = 3

// armVerdict renders one arm's outcome for the human report.
func armVerdict(value, tolerance float64) string {
	if value <= tolerance {
		return "within"
	}
	return "EXCEEDED"
}

// meanBacklog averages the OUTSTANDING work (acknowledged backlog plus the
// observed concurrency-queue depth).
func meanBacklog(samples []backlogSample) float64 {
	if len(samples) == 0 {
		return 0
	}
	total := 0.0
	for _, s := range samples {
		total += float64(s.total)
	}
	return total / float64(len(samples))
}

// leastSquaresSlope returns the per-second slope of backlog against time.
func leastSquaresSlope(samples []backlogSample) float64 {
	if len(samples) < 2 {
		return 0
	}
	base := samples[0].at
	var sumX, sumY, sumXY, sumXX float64
	n := float64(len(samples))
	for _, s := range samples {
		x := s.at.Sub(base).Seconds()
		y := float64(s.total)
		sumX += x
		sumY += y
		sumXY += x * y
		sumXX += x * x
	}
	den := n*sumXX - sumX*sumX
	if den == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / den
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
		expected:      cfg.jobCount,
		identityOK:    true,
	}
	if cfg.mode == modeOpen {
		r.expected = cfg.totalArrivals()
	}

	// Tally run statuses and end-to-end durations.
	durations := make([]time.Duration, 0, len(results))
	seenRuns := make(map[string]bool)
	for _, rr := range results {
		if rr.runID != "" {
			if seenRuns[rr.runID] {
				r.identityOK = false
				r.identityDetail = "duplicate run ID " + rr.runID
			} else {
				seenRuns[rr.runID] = true
				r.runsObserved++
			}
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
		case outcomeDropped:
			r.runsDropped++
		case outcomeRejected:
			r.runsRejected++
		case outcomeQueued:
			r.runsQueued++
		case outcomeUncertain:
			r.runsUncertain++
		case "unreconciled":
			r.runsUnreconciled++
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

// deriveOpenLoop computes the backlog, throughput and drop/reject summaries
// from the arrival ledger. It is safe to call when the workload never ran.
func (r *report) deriveOpenLoop() {
	if r.cfg.mode != modeOpen {
		r.sustainedVerdict = "not_applicable"
		return
	}
	r.dropReasons, r.rejectStatusCodes = map[string]int{}, map[int]int{}
	if r.ledger != nil {
		for _, a := range r.ledger.snapshotArrivals() {
			switch a.outcome {
			case outcomeDropped:
				r.dropReasons[a.reason]++
			case outcomeRejected:
				r.rejectStatusCodes[a.statusCode]++
			}
		}
		r.ledger.mu.Lock()
		r.censusStatus, r.censusReason = r.ledger.censusStatus, r.ledger.censusReason
		r.censusExtra, r.censusTerminal, r.censusFailed = r.ledger.censusExtra, r.ledger.censusTerminal, r.ledger.censusFailed
		r.ledger.mu.Unlock()
	}
	if r.open != nil && r.open.subscribers.requested > 0 && r.open.subscribers.mixNanos > 0 {
		r.subscriberCoverage = float64(r.open.subscribers.connectedNanos.Load()) /
			(float64(r.open.subscribers.requested) * float64(r.open.subscribers.mixNanos))
	}
	if r.open == nil || r.open.windowStart.IsZero() {
		r.sustainedVerdict = "inconclusive_insufficient_samples"
		return
	}
	r.windowSeconds = r.open.windowEnd.Sub(r.open.windowStart).Seconds()
	if !r.open.drainEnd.IsZero() {
		r.drainSeconds = r.open.drainEnd.Sub(r.open.windowEnd).Seconds()
	}
	if r.windowSeconds > 0 {
		r.offeredPerSecond = float64(len(r.results)) / r.windowSeconds
		r.admittedPerSecond = float64(r.runsObserved) / r.windowSeconds
	}
	if total := r.open.drainEnd.Sub(r.open.windowStart).Seconds(); total > 0 {
		r.completedPerSecond = float64(r.runsSucceeded+r.runsFailed+r.runsCancelled+r.runsSkipped) / total
	}

	var inWindow []backlogSample
	for _, s := range r.open.backlog {
		// An acknowledged backlog can never be negative: admitted and terminal
		// must count the SAME population. A negative reading means the driver
		// mixed census-discovered runs into the acknowledged tally.
		if s.backlog < 0 {
			r.backlogNegative = true
		}
		if s.total > r.backlogPeak {
			r.backlogPeak = s.total
		}
		if !s.at.Before(r.open.windowStart) && !s.at.After(r.open.windowEnd) {
			inWindow = append(inWindow, s)
			r.backlogAtClose = s.total
			if r.cfg.concurrencyStrategy == "queue" && !s.queueKnown {
				// An in-window sample with no queue observation behind it makes
				// the whole series untrustworthy for a growth decision: the
				// queue is where a queue-strategy workload's backlog LIVES.
				r.open.queueObsMissing = true
			}
		}
	}
	if n := len(r.open.backlog); n > 0 {
		r.backlogFinal = r.open.backlog[n-1].total
	}
	// The verdict has TWO arms, both expressed as a backlog increase across one
	// half-window so they share one allowance:
	//
	//   level arm — the MEAN backlog of the second half minus the first.
	//               Averaging is what keeps a small, genuinely steady system
	//               from being failed by one unit of integer jitter.
	//   trend arm — the fitted slope of the second half, projected across it.
	//               The level arm alone HALVES growth that starts late (a queue
	//               flat for most of the window and climbing only at the end
	//               moves the second-half mean by about half its rise), so
	//               without this arm late-onset growth could slip through.
	//
	// A workload is sustained only if BOTH arms are within the allowance.
	if r.open.queueObsMissing {
		// Missing in-window queue observations are inconclusive, never a pass.
		r.sustainedVerdict = "inconclusive_queue_unobserved"
		return
	}
	split := len(inWindow) / 2
	first, second := inWindow[:split], inWindow[split:]
	r.backlogSamplesUsed = len(second)
	r.backlogSlope = leastSquaresSlope(second)
	if len(first) < minBacklogSamplesPerHalf || len(second) < minBacklogSamplesPerHalf {
		// Too little evidence to decide. Inconclusive is not a pass.
		r.sustainedVerdict = "inconclusive_insufficient_samples"
		return
	}
	r.backlogFirstMean, r.backlogSecondMean = meanBacklog(first), meanBacklog(second)
	// Allowance: a fraction of the work OFFERED during one half-window, with a
	// floor of one run so an idle-to-one-run system is not called a regression.
	halfSeconds := second[len(second)-1].at.Sub(first[0].at).Seconds() / 2
	r.backlogTolerance = math.Max(1, sustainedTolerance*r.cfg.rate*halfSeconds)
	r.backlogMeanGrowth = r.backlogSecondMean - r.backlogFirstMean
	r.backlogSlopeRise = r.backlogSlope * halfSeconds
	if r.backlogMeanGrowth <= r.backlogTolerance && r.backlogSlopeRise <= r.backlogTolerance {
		r.sustainedVerdict = "sustained"
		return
	}
	r.sustainedVerdict = "backlog_growing"
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
	payload := map[string]any{
		"workload_interval_seconds": workloadSeconds,
		"schema_version":            reportSchemaVersion, "outcome": r.outcome(), "failure_class": r.failure, "failure_detail": r.failureDetail,
		"mode": r.cfg.mode, "workload": r.cfg.workload,
		"started_at": r.startedAt, "finished_at": r.finishedAt, "duration_seconds": r.totalDuration.Seconds(),
		"config": map[string]any{"jobs": r.cfg.jobCount, "fan_out": r.cfg.fanOut, "depth": r.cfg.depth, "concurrency": r.cfg.concurrency, "task_duration_seconds": r.cfg.taskDuration.Seconds(), "sample_interval_seconds": r.cfg.sampleRate.Seconds(), "timeout_seconds": r.cfg.timeout.Seconds(), "engine": r.cfg.engine, "image": r.cfg.image},
		"counts": map[string]int{"expected": r.expected, "observed": r.runsObserved, "succeeded": r.runsSucceeded, "failed": r.runsFailed, "timeout": r.runsTimeout, "trigger_failed": r.runsTriggerFailed, "untriggered": r.runsUntriggered, "cancelled": r.runsCancelled, "skipped": r.runsSkipped,
			"dropped": r.runsDropped, "rejected": r.runsRejected, "queued_or_skipped": r.runsQueued, "transport_uncertain": r.runsUncertain, "unreconciled": r.runsUnreconciled},
		"runs": results, "samples": samples, "metric_delta": delta,
		"metric_work_split": r.workSplitJSON(),
		"latency": map[string]any{"population": "succeeded_runs", "samples": r.runsSucceeded,
			"p50_seconds": r.endToEndP50.Seconds(), "p99_seconds": r.endToEndP99.Seconds(),
			"clock": "driver",
			"note":  "offer instant to the driver's own terminal observation, both on the driver clock: immune to a server clock offset and inclusive of the status-poll delay. Server-timestamp pairs are reported separately as the run_read lifecycle intervals.",
		},
	}
	if r.cfg.mode == modeOpen {
		for key, value := range r.openLoopJSON() {
			payload[key] = value
		}
	} else {
		// Say so rather than leaving the key absent: a consumer that finds no
		// "resources" key cannot tell "not collected" from "nothing to report".
		payload["resources"] = map[string]any{
			"status": "unavailable",
			"reason": "external container observation runs in open mode only (mode=closed)",
		}
	}
	return json.Marshal(payload)
}

// workSplitJSON separates workload-driven SQL work from the timed background
// work that runs whether or not the workload does. Folding lease renewal into
// the workload's totals would make a longer run look like a busier one.
func (r *report) workSplitJSON() map[string]any {
	if !r.metricsValid() {
		return map[string]any{"status": "unavailable", "reason": "metric samples incomplete"}
	}
	return map[string]any{
		"status": "ok",
		"workload_driven": map[string]any{
			"rows":       r.deltaTaskRunInsert + r.deltaTaskRunStatus + r.deltaEventInsert + r.deltaCallback + r.deltaCommand + r.deltaCheckpoint,
			"statements": r.deltaTaskRunInsertStmts + r.deltaTaskRunStatusStmts + r.deltaEventInsertStmts + r.deltaCallbackStmts + r.deltaCommandStmts + r.deltaCheckpointStmts,
			"categories": []string{"task_run_insert", "task_run_status", "event_insert", "callback", "command", "checkpoint"},
		},
		"timer_driven": map[string]any{
			"rows":       r.deltaLeaseRenewal,
			"statements": r.deltaLeaseRenewalStmts,
			"categories": []string{"lease_renewal"},
			"note":       "timer-driven background work; it scales with elapsed time, not with the workload",
		},
		"contention": map[string]any{"db_busy_retries": r.deltaDBBusyRetries, "worker_claims": r.deltaClaimsTotal},
	}
}

// openLoopJSON is the schema 2 open-loop evidence.
func (r *report) openLoopJSON() map[string]any {
	attempted := r.runsObserved + r.runsQueued + r.runsRejected + r.runsUncertain
	rejectCodes := map[string]int{}
	for code, n := range r.rejectStatusCodes {
		rejectCodes[strconv.Itoa(code)] = n
	}
	accounting := map[string]any{
		"offered": len(r.results), "dropped": r.runsDropped, "attempted": attempted,
		"admitted": r.runsObserved, "queued_or_skipped": r.runsQueued, "rejected": r.runsRejected,
		"transport_uncertain": r.runsUncertain,
		"completed_ok":        r.runsSucceeded, "completed_failed": r.runsFailed + r.runsCancelled + r.runsSkipped,
		"unreconciled":      r.runsUnreconciled,
		"terminal_statuses": map[string]int{"succeeded": r.runsSucceeded, "failed": r.runsFailed, "cancelled": r.runsCancelled, "skipped": r.runsSkipped},
		"dropped_reasons":   r.dropReasons, "rejected_status_codes": rejectCodes,
		"identity_ok": r.identityOK, "identity_detail": r.identityDetail,
		"offered_equals_dropped_plus_attempted": len(r.results) == r.runsDropped+attempted,
		"admitted_equals_settled":               r.runsObserved == r.runsSucceeded+r.runsFailed+r.runsCancelled+r.runsSkipped+r.runsUnreconciled,
	}
	if r.ledger != nil {
		r.ledger.mu.Lock()
		accounting["server_run_census"] = map[string]any{
			"status": r.ledger.censusStatus, "reason": r.ledger.censusReason,
			"runs_in_window": r.ledger.censusRuns, "matched_admitted": r.ledger.censusAdmitted,
			"unaccounted_runs": r.ledger.censusExtra, "unaccounted_settled": r.ledger.censusTerminal,
			"unaccounted_failed": r.ledger.censusFailed,
			"note":               "runs the server holds for the workload window that carry no acknowledged admission: bare-202 queue/skip answers and possibly-committed transport timeouts (DT-QUORUM-01)",
		}
		r.ledger.mu.Unlock()
	}

	out := map[string]any{
		"arrival_plan": map[string]any{
			"rate_per_second": r.cfg.rate, "window_seconds": r.cfg.arrivalWindow.Seconds(),
			"total_offered": len(r.results), "max_in_flight": r.cfg.maxInFlight,
			"late_budget_seconds": r.cfg.lateBudget.Seconds(), "drain_timeout_seconds": r.cfg.drainTimeout.Seconds(),
			"completion_independent": true,
		},
		"accounting": accounting,
		"throughput": map[string]any{
			"measured_window_seconds": r.windowSeconds, "drain_seconds": r.drainSeconds,
			"offered_per_second": r.offeredPerSecond, "admitted_per_second": r.admittedPerSecond,
			"completed_per_second": r.completedPerSecond,
			"verdict":              r.sustainedVerdict,
			"note":                 "a higher admitted_per_second is only an improvement while the backlog verdict stays sustained",
		},
		"backlog": map[string]any{
			"peak": r.backlogPeak, "at_window_close": r.backlogAtClose, "final": r.backlogFinal,
			"slope_per_second": r.backlogSlope, "slope_samples": r.backlogSamplesUsed,
			"includes_observed_queue_depth": true, "any_sample_negative": r.backlogNegative,
			"first_half_mean": r.backlogFirstMean, "second_half_mean": r.backlogSecondMean,
			"growth": r.backlogMeanGrowth, "slope_rise": r.backlogSlopeRise,
			"growth_tolerance": r.backlogTolerance,
			"level_arm_ok":     r.backlogMeanGrowth <= r.backlogTolerance,
			"trend_arm_ok":     r.backlogSlopeRise <= r.backlogTolerance,
			"note":             "sustained requires BOTH arms within one allowance, measured as a backlog increase across one half-window: growth = second-half mean minus first-half mean (ignores the unavoidable cold-start ramp), slope_rise = the second half's fitted slope projected across it (catches growth that starts late, which the mean alone would halve)",
		},
	}
	if r.open == nil {
		out["lifecycle"] = map[string]any{"event_stream_status": "unavailable", "event_stream_reason": "workload did not run", "intervals": map[string]any{}}
		out["resources"] = map[string]any{"status": "unavailable", "reason": "workload did not run"}
		return out
	}
	samples := make([]map[string]any, 0, len(r.open.backlog))
	for _, s := range r.open.backlog {
		samples = append(samples, map[string]any{"at": s.at, "offered": s.offered, "admitted": s.admitted,
			"terminal": s.terminal, "backlog": s.backlog, "queued": s.queued,
			"queue_observed": s.queueKnown, "total": s.total})
	}
	out["backlog"].(map[string]any)["samples"] = samples
	if r.open.lifecycle != nil {
		out["lifecycle"] = r.open.lifecycle.json()
	}
	out["resources"] = resourceJSON(r.open.resources)
	out["api_reads"] = apiReadJSON(&r.open.apiReads)
	out["subscribers"] = map[string]any{
		"requested": r.open.subscribers.requested, "connected": r.open.subscribers.connected.Load(),
		"events_received": r.open.subscribers.events.Load(), "disconnects": r.open.subscribers.disconnects.Load(),
		"stream_attempts":        r.open.subscribers.attempts.Load(),
		"streams_opened":         r.open.subscribers.opened.Load(),
		"lost_before_window_end": r.open.subscribers.lost.Load(),
		"connected_seconds":      time.Duration(r.open.subscribers.connectedNanos.Load()).Seconds(),
		"measured_seconds":       time.Duration(r.open.subscribers.mixNanos).Seconds(),
		"coverage_ratio":         r.subscriberCoverage, "min_coverage_ratio": minSubscriberCoverage,
		"errors": r.open.subscribers.errs,
		"note":   "a stream that ends before the measured interval closes — ordinary EOF included — is a LOST subscriber; the driver reconnects and coverage_ratio is enforced, because events_received alone can be satisfied by one frame per stream",
	}
	queuePeak := 0
	for _, depth := range r.open.queueDepths {
		if depth > queuePeak {
			queuePeak = depth
		}
	}
	out["drain"] = map[string]any{
		"seconds": r.drainSeconds, "backlog_at_end": r.backlogFinal,
		"all_admitted_reconciled": r.runsUnreconciled == 0,
		"queue_status":            r.open.queueStatus, "queue_reason": r.open.queueReason,
		"queue_depth_final": r.open.queueFinal, "queue_depth_peak": queuePeak,
		"queue_depth_samples":      len(r.open.queueDepths),
		"queue_drain_verified":     r.open.queueDrainVerified,
		"queue_observation_status": r.open.queueObsStatus, "queue_observation_reason": r.open.queueObsReason,
		"queue_observation_missing_in_window": r.open.queueObsMissing,
		"final_census_status":                 r.open.finalCensusStatus, "final_census_reason": r.open.finalCensusReason,
		"final_census_late_runs": r.open.finalCensusLate,
		"window_truncated":       r.open.windowTruncated,
		"note":                   "queue observation fails CLOSED: a bare-202 arrival is accounted for only once a successful queue read returned 0. The final census re-checks for runs the server committed after the first one (DT-QUORUM-01).",
	}
	cache := map[string]any{"mode": r.cfg.cacheMode, "warmup_runs": r.open.warmupRuns,
		"hits": r.open.cacheHits, "executed_tasks": r.open.cacheExecuted, "total_tasks": r.open.cacheTotal}
	if r.open.cacheTotal > 0 {
		cache["hit_ratio"] = float64(r.open.cacheHits) / float64(r.open.cacheTotal)
	} else {
		cache["hit_ratio"] = nil
		cache["status"] = "unavailable"
		cache["reason"] = "no reconciled run reported task counts"
	}
	if w := r.open.warmup; w != nil {
		cache["warmup"] = map[string]any{
			"runs": w.runs, "seconds": w.seconds,
			"metric_delta":                  map[string]any{"rows": w.rows, "statements": w.statements},
			"excluded_from_measured_deltas": true,
			"note":                          "cold warm-up executions, measured between the pre-warm-up scrape and the fresh baseline the measured window starts from; metric_delta, metric_work_split and the peak rates cover the measured window only",
		}
	}
	out["cache"] = cache
	return out
}

func resourceJSON(res resourceReport) map[string]any {
	out := map[string]any{
		"status": res.status, "reason": res.reason, "samples": len(res.samples),
		"source": "container runtime stats API, observed from outside the server container",
		"note":   "external observation only; per-task resource telemetry belongs to the resource right-sizing plan",
	}
	if len(res.samples) == 0 {
		out["cpu_percent"], out["memory_bytes"], out["memory_limit_bytes"] = nil, nil, nil
		if res.status == "" {
			out["status"] = "unavailable"
		}
		return out
	}
	cpu := make([]float64, 0, len(res.samples))
	mem := make([]float64, 0, len(res.samples))
	limit := 0.0
	for _, s := range res.samples {
		cpu = append(cpu, s.cpuPercent)
		mem = append(mem, s.memoryBytes)
		if s.memoryLimit > limit {
			limit = s.memoryLimit
		}
	}
	slices.Sort(cpu)
	slices.Sort(mem)
	mean := func(v []float64) float64 {
		total := 0.0
		for _, x := range v {
			total += x
		}
		return total / float64(len(v))
	}
	out["cpu_percent"] = map[string]any{"min": cpu[0], "max": cpu[len(cpu)-1], "mean": mean(cpu)}
	out["memory_bytes"] = map[string]any{"min": mem[0], "max": mem[len(mem)-1], "mean": mean(mem)}
	out["memory_limit_bytes"] = limit
	return out
}

func apiReadJSON(a *apiReadReport) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.configured {
		return map[string]any{"status": "unavailable", "reason": "not_configured: api-read-rate is zero", "offered": 0}
	}
	statuses := map[string]int{}
	for code, n := range a.statuses {
		statuses[strconv.Itoa(code)] = n
	}
	out := map[string]any{
		"status": "ok", "offered": atomic.LoadInt64(&a.offered), "dropped": atomic.LoadInt64(&a.dropped),
		"ok": a.ok, "failed": a.failed, "status_codes": statuses,
	}
	if len(a.latencies) == 0 {
		out["p50_seconds"], out["p99_seconds"] = nil, nil
		out["latency_status"] = "unavailable"
		out["latency_reason"] = "no completed read"
		return out
	}
	sorted := slices.Clone(a.latencies)
	slices.Sort(sorted)
	out["latency_status"] = "ok"
	out["p50_seconds"] = percentile(sorted, 50).Seconds()
	out["p99_seconds"] = percentile(sorted, 99).Seconds()
	return out
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
	fmt.Fprintf(w, "Mode:                %s\n", r.cfg.mode)
	if r.cfg.workload != "" {
		fmt.Fprintf(w, "Workload:            %s\n", r.cfg.workload)
	}
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
	if r.cfg.mode == modeOpen {
		r.printOpenLoop(w)
	}
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

// printOpenLoop renders the admission ledger, backlog verdict and observed
// lifecycle intervals. Unavailable values are printed as "unavailable
// (<reason>)" — never as a plausible-looking zero.
func (r *report) printOpenLoop(w io.Writer) {
	attempted := r.runsObserved + r.runsQueued + r.runsRejected + r.runsUncertain
	fmt.Fprintln(w, "--- Open-Loop Admission Ledger ---")
	fmt.Fprintln(w, "")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintf(tw, "Offered (planned)\t%d\n", len(r.results))
	fmt.Fprintf(tw, "  dropped by driver\t%d\n", r.runsDropped)
	for reason, n := range r.dropReasons {
		fmt.Fprintf(tw, "    %s\t%d\n", reason, n)
	}
	fmt.Fprintf(tw, "  attempted\t%d\n", attempted)
	fmt.Fprintf(tw, "    admitted (202 + run UUID)\t%d\n", r.runsObserved)
	fmt.Fprintf(tw, "    queued/skipped (bare 202)\t%d\n", r.runsQueued)
	fmt.Fprintf(tw, "    rejected by server\t%d\n", r.runsRejected)
	for code, n := range r.rejectStatusCodes {
		fmt.Fprintf(tw, "      HTTP %d\t%d\n", code, n)
	}
	fmt.Fprintf(tw, "    transport uncertain\t%d\n", r.runsUncertain)
	fmt.Fprintf(tw, "Admitted reconciliation\t%d\n", r.runsObserved)
	fmt.Fprintf(tw, "  completed ok\t%d\n", r.runsSucceeded)
	fmt.Fprintf(tw, "  completed failed\t%d\n", r.runsFailed+r.runsCancelled+r.runsSkipped)
	fmt.Fprintf(tw, "  UNRECONCILED\t%d\n", r.runsUnreconciled)
	fmt.Fprintf(tw, "Identity holds\t%t\n", len(r.results) == r.runsDropped+attempted &&
		r.runsObserved == r.runsSucceeded+r.runsFailed+r.runsCancelled+r.runsSkipped+r.runsUnreconciled)
	tw.Flush()
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Offered/s:           %.2f\n", r.offeredPerSecond)
	fmt.Fprintf(w, "Admitted/s:          %.2f\n", r.admittedPerSecond)
	fmt.Fprintf(w, "Completed/s:         %.2f\n", r.completedPerSecond)
	fmt.Fprintf(w, "Backlog peak/close:  %d / %d\n", r.backlogPeak, r.backlogAtClose)
	fmt.Fprintf(w, "Backlog mean 1st/2nd half: %.2f / %.2f\n", r.backlogFirstMean, r.backlogSecondMean)
	fmt.Fprintf(w, "Backlog level arm:   growth %.2f vs tolerance %.2f (%s)\n",
		r.backlogMeanGrowth, r.backlogTolerance, armVerdict(r.backlogMeanGrowth, r.backlogTolerance))
	fmt.Fprintf(w, "Backlog trend arm:   rise %.2f vs tolerance %.2f (%s), slope %.3f runs/s over %d samples\n",
		r.backlogSlopeRise, r.backlogTolerance, armVerdict(r.backlogSlopeRise, r.backlogTolerance),
		r.backlogSlope, r.backlogSamplesUsed)
	fmt.Fprintf(w, "Sustained verdict:   %s\n", r.sustainedVerdict)
	fmt.Fprintf(w, "Drain:               %.1fs\n", r.drainSeconds)
	fmt.Fprintln(w, "")
	if r.open == nil || r.open.lifecycle == nil {
		fmt.Fprintln(w, "Lifecycle intervals: unavailable (workload did not run)")
		fmt.Fprintln(w, "")
		return
	}
	fmt.Fprintln(w, "--- Observed Lifecycle Intervals ---")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Event stream:        %s %s (%d observed, %d duplicates)\n",
		r.open.lifecycle.eventsStatus, r.open.lifecycle.eventsReason, r.open.lifecycle.received, r.open.lifecycle.duplicates)
	lt := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintf(lt, "Interval\tSource\tN\tp50\tp99\tUnavailable\n")
	fmt.Fprintf(lt, "--------\t------\t-\t---\t---\t-----------\n")
	for _, s := range r.open.lifecycle.intervals {
		data := s.json()
		unavailable := fmt.Sprint(data["unavailable"])
		if reasons, ok := data["unavailable_reasons"].(map[string]int); ok && len(reasons) > 0 {
			parts := make([]string, 0, len(reasons))
			for reason, n := range reasons {
				parts = append(parts, fmt.Sprintf("%s=%d", reason, n))
			}
			slices.Sort(parts)
			unavailable = strings.Join(parts, " ")
		}
		if data["status"] == "unavailable" {
			fmt.Fprintf(lt, "%s\t%s\t0\tunavailable\tunavailable\t%s\n", s.name, s.source, unavailable)
			continue
		}
		fmt.Fprintf(lt, "%s\t%s\t%d\t%.3fs\t%.3fs\t%s\n", s.name, s.source, len(s.samples),
			data["p50_seconds"], data["p99_seconds"], unavailable)
	}
	lt.Flush()
	fmt.Fprintln(w, "")
	res := r.open.resources
	if res.status == "ok" {
		summary := resourceJSON(res)
		cpu, _ := summary["cpu_percent"].(map[string]any)
		mem, _ := summary["memory_bytes"].(map[string]any)
		fmt.Fprintf(w, "Server container:    cpu max %.1f%% mean %.1f%%, memory max %.0f bytes (%d samples)\n",
			cpu["max"], cpu["mean"], mem["max"], len(res.samples))
	} else {
		fmt.Fprintf(w, "Server container:    unavailable (%s)\n", res.reason)
	}
	fmt.Fprintln(w, "")
}

func (r *report) markdown() string {
	var b strings.Builder
	r.print(&b)
	return b.String()
}

// ---------------------------------------------------------------------------
// Workload catalog
// ---------------------------------------------------------------------------

// catalog is the versioned workload catalog (test/performance/workloads.json).
// Keeping the catalog inside the driver — rather than only inside the
// integration test — means the same entries can be replayed by hand and can be
// validated hermetically, without a server.
type catalog struct {
	SchemaVersion int            `json:"schema_version"`
	Description   string         `json:"description"`
	Workloads     []catalogEntry `json:"workloads"`
}

type catalogEntry struct {
	Name        string `json:"name"`
	Tier        string `json:"tier"`
	Description string `json:"description"`
	// Requires declares what the server under test must provide. A workload
	// whose prerequisites are absent is reported blocked/skipped with this
	// reason — never silently passed.
	Requires catalogRequires `json:"requires"`
	// Driver maps load-driver flag names to values. Using the real flag names
	// keeps the catalog honest: an unknown or malformed knob fails to parse
	// instead of being ignored.
	Driver map[string]any `json:"driver"`
	// Expect is the invariant set the integration runner asserts on the
	// driver's JSON result.
	Expect map[string]any `json:"expect"`
	// SustainedRationale records why this workload does or does not gate on
	// the backlog verdict. Required: see TestWorkloadCatalogIsValid.
	SustainedRationale string `json:"sustained_rationale"`
}

type catalogRequires struct {
	Engine    string   `json:"engine"`
	ServerEnv []string `json:"server_env"`
	Reason    string   `json:"reason"`
}

// catalogSchemaVersion is the version this driver understands.
const catalogSchemaVersion = 1

func loadCatalog(path string) (*catalog, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("read workload catalog: %w", err)
	}
	var c catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse workload catalog %s: %w", path, err)
	}
	if c.SchemaVersion != catalogSchemaVersion {
		return nil, fmt.Errorf("workload catalog %s has schema_version %d, this driver understands %d",
			path, c.SchemaVersion, catalogSchemaVersion)
	}
	if len(c.Workloads) == 0 {
		return nil, fmt.Errorf("workload catalog %s declares no workloads", path)
	}
	seen := map[string]bool{}
	for _, entry := range c.Workloads {
		if strings.TrimSpace(entry.Name) == "" {
			return nil, fmt.Errorf("workload catalog %s has an entry without a name", path)
		}
		if seen[entry.Name] {
			return nil, fmt.Errorf("workload catalog %s declares %q twice", path, entry.Name)
		}
		seen[entry.Name] = true
		if len(entry.Driver) == 0 {
			return nil, fmt.Errorf("workload %q declares no driver flags", entry.Name)
		}
		if len(entry.Expect) == 0 {
			return nil, fmt.Errorf("workload %q declares no expectations, so running it could not fail", entry.Name)
		}
	}
	return &c, nil
}

// flagValue renders a catalog value as the string the flag package parses.
func flagValue(v any) (string, error) {
	switch typed := v.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("unsupported catalog value %v of type %T", v, v)
	}
}

// applyEntry writes one catalog entry's driver flags onto flags, skipping any
// flag the caller set explicitly on the command line.
func applyEntry(entry catalogEntry, flags *flag.FlagSet, explicit map[string]bool) error {
	names := make([]string, 0, len(entry.Driver))
	for name := range entry.Driver {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if explicit[name] {
			continue
		}
		if flags.Lookup(name) == nil {
			return fmt.Errorf("workload %q sets unknown driver flag %q", entry.Name, name)
		}
		value, err := flagValue(entry.Driver[name])
		if err != nil {
			return fmt.Errorf("workload %q flag %q: %w", entry.Name, name, err)
		}
		if err := flags.Set(name, value); err != nil {
			return fmt.Errorf("workload %q flag %q=%q: %w", entry.Name, name, value, err)
		}
	}
	return nil
}

func applyCatalog(cfg *config, flags *flag.FlagSet) error {
	if cfg.catalogFile == "" || cfg.catalogWorkload == "" {
		return errors.New("-catalog and -catalog-workload must be given together")
	}
	c, err := loadCatalog(cfg.catalogFile)
	if err != nil {
		return err
	}
	explicit := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for _, entry := range c.Workloads {
		if entry.Name != cfg.catalogWorkload {
			continue
		}
		if err := applyEntry(entry, flags, explicit); err != nil {
			return err
		}
		if !explicit["workload"] {
			cfg.workload = entry.Name
		}
		return nil
	}
	available := make([]string, 0, len(c.Workloads))
	for _, entry := range c.Workloads {
		available = append(available, entry.Name)
	}
	return fmt.Errorf("workload %q is not in %s; available: %s",
		cfg.catalogWorkload, cfg.catalogFile, strings.Join(available, ", "))
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() { os.Exit(runMain(os.Args[1:], os.Stdout, os.Stderr)) }

// newFlagSet binds every driver knob to cfg. It is a named function so the
// workload catalog can be validated against the SAME flag set the binary uses,
// which is what stops the catalog from drifting into unknown or invalid knobs.
func newFlagSet(cfg *config, stderr io.Writer) *flag.FlagSet {
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
	flags.StringVar(&cfg.mode, "mode", cfg.mode, "closed (completion-governed worker pool) or open (arrival-rate scheduling independent of completion)")
	flags.StringVar(&cfg.workload, "workload", cfg.workload, "Workload label recorded in the report and used to namespace the generated job aliases")
	flags.StringVar(&cfg.catalogFile, "catalog", cfg.catalogFile, "Path to the versioned workload catalog (test/performance/workloads.json)")
	flags.StringVar(&cfg.catalogWorkload, "catalog-workload", cfg.catalogWorkload, "Name of the catalog entry to run; flags given after it still win")
	flags.Float64Var(&cfg.rate, "rate", cfg.rate, "Offered arrival rate in runs/second (open mode)")
	flags.DurationVar(&cfg.arrivalWindow, "arrival-window", cfg.arrivalWindow, "How long arrivals are offered for (open mode)")
	flags.IntVar(&cfg.maxInFlight, "max-in-flight", cfg.maxInFlight, "Hard cap on concurrent trigger requests; an arrival that finds it full is DROPPED, never queued")
	flags.DurationVar(&cfg.lateBudget, "late-budget", cfg.lateBudget, "How late an arrival may be offered before the driver drops it instead of firing a catch-up burst")
	flags.DurationVar(&cfg.drainTimeout, "drain-timeout", cfg.drainTimeout, "Bound on the post-window reconciliation phase")
	flags.IntVar(&cfg.reconcileWorkers, "reconcile-workers", cfg.reconcileWorkers, "Bounded pool that polls admitted runs to a terminal status")
	flags.DurationVar(&cfg.pollInterval, "poll-interval", cfg.pollInterval, "Run-status poll interval during reconciliation")
	flags.BoolVar(&cfg.requireSustained, "require-sustained", cfg.requireSustained, "Fail when the backlog grows across the arrival window (admission outrunning completion is not throughput)")
	flags.BoolVar(&cfg.allowRunFailures, "allow-run-failures", cfg.allowRunFailures, "Permit admitted runs to end non-succeeded (workloads whose point is cancellation)")
	flags.StringVar(&cfg.concurrencyStrategy, "concurrency-strategy", cfg.concurrencyStrategy, "metadata.concurrency strategy written on every generated job: queue, replace, skip, or fail")
	flags.IntVar(&cfg.maxRuns, "max-runs", cfg.maxRuns, "metadata.concurrency maxRuns")
	flags.StringVar(&cfg.cacheMode, "cache", cfg.cacheMode, "off (no cache metadata), miss (cache on, first executions), or hit (cache on, warmed before the window)")
	flags.Float64Var(&cfg.apiReadRate, "api-read-rate", cfg.apiReadRate, "Concurrent public read traffic in reads/second")
	flags.IntVar(&cfg.subscribers, "subscribers", cfg.subscribers, "Passive /v1/events SSE subscribers held open for the whole workload")
	flags.BoolVar(&cfg.lifecycle, "lifecycle", cfg.lifecycle, "Observe /v1/events to derive lifecycle intervals from server timestamps")
	flags.StringVar(&cfg.resourceContainer, "resource-container", cfg.resourceContainer, "Name/ID of the server container to observe externally via the runtime stats API")
	flags.StringVar(&cfg.dockerHost, "docker-host", cfg.dockerHost, "Container runtime socket used for the external resource observation")
	return flags
}

func runMain(args []string, stdout, stderr io.Writer) int {
	cfg := defaultConfig()
	flags := newFlagSet(&cfg, stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}
	// A catalog entry is a configuration BASE: it is applied first, then the
	// same flag set is re-parsed so an explicit flag still wins. That keeps the
	// catalog authoritative for a workload's shape while leaving the server
	// URL, credentials and output paths to the caller.
	if cfg.catalogFile != "" || cfg.catalogWorkload != "" {
		if err := applyCatalog(&cfg, flags); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
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

// envFloatOrDefault mirrors envIntOrDefault: a malformed value becomes 0 so
// validation rejects it instead of silently substituting the default.
func envFloatOrDefault(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return f
	}
	return def
}

// envNonNegIntOrDefault / envNonNegFloatOrDefault are for knobs where ZERO is a
// legitimate value ("no subscribers", "no read mix"). Returning 0 on a parse
// error would silently accept a typo, so they return -1, which validation
// rejects.
func envNonNegIntOrDefault(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return -1
		}
		return n
	}
	return def
}

func envNonNegFloatOrDefault(key string, def float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return -1
		}
		return f
	}
	return def
}

// envBoolOrDefault rejects malformed values by returning the inverse of the
// default, which every caller's validation or semantics treats as explicit.
func envBoolOrDefault(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false
		}
		return b
	}
	return def
}

// percentile returns the p-th percentile of an already-sorted slice using the
// nearest-rank method, clamped to the slice bounds.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

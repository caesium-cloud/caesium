package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const validMetrics = "# TYPE caesium_db_busy_retries_total counter\ncaesium_db_busy_retries_total 0\n# TYPE caesium_db_writes_total counter\ncaesium_db_writes_total{category=\"task_run_status\"} 5\n# TYPE caesium_db_statements_total counter\ncaesium_db_statements_total{category=\"task_run_status\"} 2\n"

func fixtureConfig(server string) config {
	return config{serverURL: server, jobCount: 3, fanOut: 2, depth: 2, taskDuration: time.Millisecond, concurrency: 1, sampleRate: 25 * time.Millisecond, timeout: 3 * time.Second, engine: "docker", image: "busybox:1.36.1"}
}

// These HTTP fixtures exercise the complete harness driver and JSON reporter;
// they are hermetic and make no claim to qualify the Caesium server itself.
type fixture struct {
	mu            sync.Mutex
	defs          []jobDef
	starts        map[string]time.Time
	status        string
	delay         time.Duration
	triggerStatus int
	triggerBody   string
	metrics       func(int32) (int, string)
	metricCalls   atomic.Int32
	triggerCalls  atomic.Int32
}

func (f *fixture) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case req.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	case req.URL.Path == "/metrics":
		n := f.metricCalls.Add(1)
		code, body := http.StatusOK, validMetrics
		if f.metrics != nil {
			code, body = f.metrics(n)
		}
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	case req.URL.Path == "/v1/jobdefs/apply":
		var payload struct {
			Definitions []jobDef `json:"definitions"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.defs = payload.Definitions
		fmt.Fprint(w, `{"applied":3}`)
	case req.URL.Path == "/v1/jobs":
		jobs := make([]map[string]string, 0, len(f.defs))
		for i, def := range f.defs {
			jobs = append(jobs, map[string]string{"id": fmt.Sprint(i), "alias": def.Metadata.Alias})
		}
		_ = json.NewEncoder(w).Encode(jobs)
	case strings.HasSuffix(req.URL.Path, "/run"):
		f.triggerCalls.Add(1)
		if f.triggerStatus != 0 {
			w.WriteHeader(f.triggerStatus)
			fmt.Fprint(w, f.triggerBody)
			return
		}
		id := strings.Split(req.URL.Path, "/")[3]
		if f.starts == nil {
			f.starts = map[string]time.Time{}
		}
		f.starts[id] = time.Now()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "run-" + id})
	case strings.Contains(req.URL.Path, "/runs/"):
		id := strings.Split(req.URL.Path, "/")[3]
		status := f.status
		if status == "" {
			status = "succeeded"
		}
		if time.Since(f.starts[id]) < f.delay {
			status = "running"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
	default:
		http.NotFound(w, req)
	}
}

func TestConfigRejectsInvalidValuesBeforeNetwork(t *testing.T) {
	cases := map[string]func(*config){
		"zero jobs": func(c *config) { c.jobCount = 0 }, "negative width": func(c *config) { c.fanOut = -1 }, "zero depth": func(c *config) { c.depth = 0 }, "zero concurrency": func(c *config) { c.concurrency = 0 }, "zero interval": func(c *config) { c.sampleRate = 0 }, "negative task": func(c *config) { c.taskDuration = -1 }, "zero timeout": func(c *config) { c.timeout = 0 }, "scheme": func(c *config) { c.serverURL = "file:///tmp/no" }, "relative server": func(c *config) { c.serverURL = "localhost:8080" }, "credentials": func(c *config) { c.serverURL = "http://secret@localhost" }, "engine": func(c *config) { c.engine = "bogus" }, "image": func(c *config) { c.image = " " }, "overflow": func(c *config) { c.jobCount = int(^uint(0) >> 1) }, "same output": func(c *config) { c.outputFile = "out"; c.jsonFile = "out" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := fixtureConfig("http://127.0.0.1:1")
			mutate(&cfg)
			r, err := newHarness(cfg).run(context.Background())
			if err == nil || r.failure != "invalid_config" {
				t.Fatalf("got report=%+v err=%v", r, err)
			}
		})
	}
}

func TestMalformedEnvironmentIsRejected(t *testing.T) {
	for _, value := range []string{"0", "-1", "3junk", "NaN", "", "99999999999999999999999999"} {
		t.Run("int="+value, func(t *testing.T) {
			t.Setenv("CAESIUM_LOAD_JOBS", value)
			if err := defaultConfig().validate(); err == nil {
				t.Fatal("accepted invalid jobs")
			}
		})
	}
	for _, value := range []string{"0s", "-1s", "NaN", "", "99999999999999999h"} {
		t.Run("duration="+value, func(t *testing.T) {
			t.Setenv("CAESIUM_LOAD_TIMEOUT", value)
			if err := defaultConfig().validate(); err == nil {
				t.Fatal("accepted invalid timeout")
			}
		})
	}
}

func TestDriverClassifiesEveryExpectedRun(t *testing.T) {
	cases := []struct {
		name, status, body string
		code               int
		want               string
	}{
		{name: "success", status: "succeeded", want: "succeeded"}, {name: "failed", status: "failed", want: "failed"}, {name: "cancelled", status: "cancelled", want: "cancelled"}, {name: "skipped", status: "skipped", want: "skipped"},
		{name: "trigger rejection", code: 503, want: "trigger_failed"}, {name: "unconfirmed admission", code: 202, body: `{}`, want: "trigger_failed"}, {name: "malformed admission", code: 200, body: `{`, want: "trigger_failed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &fixture{status: tt.status, triggerStatus: tt.code, triggerBody: tt.body}
			srv := httptest.NewServer(http.HandlerFunc(f.serve))
			defer srv.Close()
			r, err := newHarness(fixtureConfig(srv.URL)).run(context.Background())
			if (err == nil) != (tt.want == "succeeded") {
				t.Fatalf("outcome %s: %v", r.failure, err)
			}
			if len(r.results) != 3 {
				t.Fatalf("results=%d", len(r.results))
			}
			for _, rr := range r.results {
				if rr.status != tt.want {
					t.Fatalf("status=%s want=%s", rr.status, tt.want)
				}
			}
			observed := 3
			if tt.want == "trigger_failed" {
				observed = 0
			}
			if r.runsObserved != observed {
				t.Fatalf("observed=%d", r.runsObserved)
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Schema int               `json:"schema_version"`
				Counts map[string]int    `json:"counts"`
				Runs   []json.RawMessage `json:"runs"`
			}
			if err = json.Unmarshal(data, &decoded); err != nil {
				t.Fatal(err)
			}
			// Schema 2 (E2) keeps every schema 1 field meaningful; only the
			// version number moved.
			if decoded.Schema != reportSchemaVersion || decoded.Counts["expected"] != 3 || decoded.Counts[tt.want] != 3 || len(decoded.Runs) != 3 {
				t.Fatalf("JSON=%s", data)
			}
		})
	}
}

func TestSamplingCoversSerialEarlyAndMiddleExecution(t *testing.T) {
	f := &fixture{delay: 240 * time.Millisecond}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	r, err := newHarness(fixtureConfig(srv.URL)).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.samples) < 5 {
		t.Fatalf("only %d samples", len(r.samples))
	}
	if r.samples[0].phase != "baseline" || r.samples[len(r.samples)-1].phase != "final" {
		t.Fatal("missing boundary samples")
	}
	for _, rr := range r.results {
		early, middle := false, false
		duration := rr.finishedAt.Sub(rr.startedAt)
		for _, sample := range r.samples {
			if sample.phase != "periodic" {
				continue
			}
			offset := sample.ts.Sub(rr.startedAt)
			early = early || (offset > 0 && offset < duration/3)
			middle = middle || (offset >= duration/3 && offset < 2*duration/3)
		}
		if !early || !middle {
			t.Fatalf("run %s lacks early=%v middle=%v coverage", rr.alias, early, middle)
		}
	}
}

func TestDeadlineAccountsForUntriggeredAndUnfinished(t *testing.T) {
	f := &fixture{delay: time.Hour}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	cfg := fixtureConfig(srv.URL)
	cfg.timeout = 120 * time.Millisecond
	started := time.Now()
	r, err := newHarness(cfg).run(context.Background())
	if err == nil || r.failure != "deadline_exceeded" || r.runsTimeout != 1 || r.runsUntriggered != 2 || r.runsObserved != 1 {
		t.Fatalf("report=%+v err=%v", r, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("deadline did not bound execution")
	}
}

func TestUnavailableServerEmitsFailureJSON(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	var stdout, stderr bytes.Buffer
	code := runMain([]string{"-server", srv.URL, "-timeout", "50ms", "-jobs", "2", "-json-output", "-"}, &stdout, &stderr)
	var result struct {
		Failure string         `json:"failure_class"`
		Counts  map[string]int `json:"counts"`
		Delta   any            `json:"metric_delta"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout not clean JSON: %q: %v", stdout.String(), err)
	}
	if code == 0 || result.Failure != "server_unavailable" || result.Counts["expected"] != 2 || result.Counts["untriggered"] != 2 || result.Counts["observed"] != 0 || result.Delta != nil {
		t.Fatalf("code=%d JSON=%s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "Baseline Report") {
		t.Fatal("human report missing")
	}
}

func TestRequiredSamplesFailClosed(t *testing.T) {
	cases := []struct {
		name    string
		metrics func(int32) (int, string)
		delay   time.Duration
	}{
		{"baseline HTTP", func(int32) (int, string) { return 503, validMetrics }, 0},
		{"missing write families", func(int32) (int, string) {
			return 200, "# TYPE caesium_db_busy_retries_total counter\ncaesium_db_busy_retries_total 0\n"
		}, 0},
		{"baseline empty", func(int32) (int, string) { return 200, "" }, 0},
		{"wrong family type", func(int32) (int, string) {
			return 200, "# TYPE caesium_db_busy_retries_total gauge\ncaesium_db_busy_retries_total 0\n"
		}, 0},
		{"NaN", func(int32) (int, string) { return 200, strings.Replace(validMetrics, "total 0", "total NaN", 1) }, 0},
		{"malformed", func(int32) (int, string) { return 200, validMetrics + "broken{\n" }, 0},
		{"final", func(n int32) (int, string) {
			if n > 1 {
				return 503, ""
			}
			return 200, validMetrics
		}, 0},
		{"periodic", func(n int32) (int, string) {
			if n == 2 {
				return 503, ""
			}
			return 200, validMetrics
		}, 150 * time.Millisecond},
		{"counter reset", func(n int32) (int, string) {
			if n > 1 {
				return 200, strings.Replace(validMetrics, "} 5", "} 1", 1)
			}
			return 200, validMetrics
		}, 0},
		{"counter disappears", func(n int32) (int, string) {
			if n > 1 {
				return 200, "# TYPE caesium_db_busy_retries_total counter\ncaesium_db_busy_retries_total 0\n"
			}
			return 200, validMetrics
		}, 0},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &fixture{metrics: tt.metrics, delay: tt.delay}
			srv := httptest.NewServer(http.HandlerFunc(f.serve))
			defer srv.Close()
			r, err := newHarness(fixtureConfig(srv.URL)).run(context.Background())
			if err == nil || r.failure != "metrics_missing" {
				t.Fatalf("class=%s err=%v", r.failure, err)
			}
		})
	}
}

func TestMissingBaselineCategoriesAreLegitimateZero(t *testing.T) {
	f := &fixture{metrics: func(n int32) (int, string) {
		if n == 1 {
			return 200, "# TYPE caesium_db_busy_retries_total counter\ncaesium_db_busy_retries_total 0\n"
		}
		return 200, validMetrics
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	r, err := newHarness(fixtureConfig(srv.URL)).run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.deltaTaskRunStatus != 5 {
		t.Fatalf("delta=%v", r.deltaTaskRunStatus)
	}
}

func TestCancelledTriggerAdmissionRemainsUnconfirmed(t *testing.T) {
	f := &fixture{}
	triggered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/run") {
			close(triggered)
			<-req.Context().Done()
			return
		}
		f.serve(w, req)
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { <-triggered; cancel() }()
	r, err := newHarness(fixtureConfig(srv.URL)).run(ctx)
	if err == nil || r.failure != "cancelled" || r.runsTriggerFailed != 1 || r.runsUntriggered != 2 || r.runsObserved != 0 {
		t.Fatalf("report=%+v err=%v", r, err)
	}
	if !strings.Contains(r.results[0].err.Error(), "admission unconfirmed") {
		t.Fatal("trigger cancellation incorrectly claims rejection")
	}
}

func TestReporterRejectsDuplicateRunIdentity(t *testing.T) {
	f := &fixture{triggerStatus: 200, triggerBody: `{"id":"same-run"}`}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	defer srv.Close()
	r, err := newHarness(fixtureConfig(srv.URL)).run(context.Background())
	if err == nil || r.failure != "run_identity_invalid" || r.runsObserved != 1 {
		t.Fatalf("report=%+v err=%v", r, err)
	}
}

func TestPeriodicCoverageUsesExecutionWindowNotScrapeLatency(t *testing.T) {
	for _, slowPeriodic := range []bool{false, true} {
		t.Run(fmt.Sprint("slowPeriodic=", slowPeriodic), func(t *testing.T) {
			f := &fixture{}
			if slowPeriodic {
				f.delay = 60 * time.Millisecond
			}
			var metrics atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/metrics" {
					if metrics.Add(1) == 2 {
						time.Sleep(400 * time.Millisecond)
					}
					fmt.Fprint(w, validMetrics)
					return
				}
				f.serve(w, req)
			}))
			defer srv.Close()
			cfg := fixtureConfig(srv.URL)
			cfg.jobCount = 1
			r, err := newHarness(cfg).run(context.Background())
			if slowPeriodic {
				if err == nil || r.failure != "metrics_missing" {
					t.Fatalf("late scrape incorrectly establishes coverage: %v", err)
				}
			} else if err != nil {
				t.Fatalf("fast workload need not sample during slow final scrape: %v", err)
			}
		})
	}
}

// ===========================================================================
// E2 — open-loop arrivals, lifecycle measurement, workload catalog
//
// Everything below is hermetic: an httptest server stands in for Caesium so
// arrival scheduling, the admission ledger and the reporter's verdicts can be
// proven without Docker, without a database and without timing luck. The live
// surface is driven separately by test/performance/load_test.go.
// ===========================================================================

// openFixture is a controllable Caesium stand-in. Each knob isolates one
// accounting or timing property; none of them fakes a verdict.
type openFixture struct {
	mu sync.Mutex

	// triggerDelay stalls POST /v1/jobs/:id/run, so a closed loop would be
	// visibly slower than the arrival plan.
	triggerDelay time.Duration
	// triggerStatus/triggerBody override the admission response.
	triggerStatus int
	triggerBody   string
	// killConnection drops the TCP connection mid-request without a response,
	// which is the transport-uncertain case: the write may still have
	// committed (DT-QUORUM-01).
	killConnection bool
	// runDelay is how long after admission a run reports a terminal status.
	runDelay time.Duration
	// runStatus is the terminal status runs settle on.
	runStatus string
	// cacheHit makes the single task report a cache hit with no container start.
	cacheHit bool
	// clockSkew offsets every SERVER-side timestamp the fixture reports, which
	// is what an unsynchronized server clock looks like to the driver.
	clockSkew time.Duration
	// metrics overrides the /metrics body, so a test can make counters rise
	// during one phase of the run only.
	metrics func() string
	// queueStatus/queueDepth control GET …/queue: a non-zero status makes queue
	// observation fail, and queueDepth is how many rows a successful read
	// reports. queueDepth < 0 means "never empties".
	queueStatus int
	queueDepth  int
	// lateCommitAfterListCalls materializes a run the trigger never
	// acknowledged (the connection died) only once the run list has been read
	// this many times — the server commits on a background context, so the run
	// can appear AFTER the first census.
	lateCommitAfterListCalls int
	pendingLate              []string
	listCalls                int
	// stallHeaders makes /v1/events accept the connection and never answer.
	stallHeaders bool
	// eventShape is the per-run landmark sequence the fixture persists when a
	// run is admitted. Offsets are relative to that run's admission instant, so
	// the intervals derived from the served timestamps are exact. Repeating an
	// entry replays one landmark, which is the at-least-once delivery case.
	eventShape []fixtureEvent

	defs          []jobDef
	runs          map[string]time.Time
	runJob        map[string]string
	runOrder      []string
	triggerBodies []string
	triggers      atomic.Int32
	reads         atomic.Int32
}

// fixtureEvent is one landmark in the per-run event shape.
type fixtureEvent struct {
	typ    string
	taskID string
	offset time.Duration
	// replay repeats the PREVIOUS landmark's sequence, so the reader sees the
	// same event identity twice.
	replay bool
}

func (f *openFixture) serve(w http.ResponseWriter, req *http.Request) {
	switch {
	case req.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	case req.URL.Path == "/metrics":
		if f.metrics != nil {
			fmt.Fprint(w, f.metrics())
			return
		}
		fmt.Fprint(w, validMetrics)
	case req.URL.Path == "/v1/events":
		f.serveEvents(w, req)
	case req.URL.Path == "/v1/jobdefs/apply":
		var payload struct {
			Definitions []jobDef `json:"definitions"`
		}
		if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.defs = payload.Definitions
		f.mu.Unlock()
		fmt.Fprint(w, `{"applied":1}`)
	case req.URL.Path == "/v1/jobs":
		f.mu.Lock()
		jobs := make([]map[string]string, 0, len(f.defs))
		for i, def := range f.defs {
			jobs = append(jobs, map[string]string{"id": fixtureJobID(i), "alias": def.Metadata.Alias})
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(jobs)
	case req.URL.Path == "/v1/stats/summary":
		f.reads.Add(1)
		fmt.Fprint(w, `{"ok":true}`)
	case strings.HasSuffix(req.URL.Path, "/run"):
		f.serveTrigger(w, req)
	case strings.Contains(req.URL.Path, "/runs/"):
		f.serveRunRead(w, req)
	case strings.HasSuffix(req.URL.Path, "/runs"):
		f.reads.Add(1)
		f.serveRunList(w, req)
	case strings.HasSuffix(req.URL.Path, "/queue"):
		if f.queueStatus != 0 {
			http.Error(w, "queue unavailable", f.queueStatus)
			return
		}
		f.mu.Lock()
		depth := f.queueDepth
		f.mu.Unlock()
		rows := make([]map[string]any, 0, max(depth, 0))
		for i := 0; i < depth; i++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("q-%d", i)})
		}
		_ = json.NewEncoder(w).Encode(rows)
	default:
		http.NotFound(w, req)
	}
}

func fixtureJobID(i int) string   { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }
func fixtureRunID(n int32) string { return fmt.Sprintf("11111111-0000-4000-8000-%012d", n) }

func (f *openFixture) serveTrigger(w http.ResponseWriter, req *http.Request) {
	n := f.triggers.Add(1)
	body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<16))
	f.mu.Lock()
	f.triggerBodies = append(f.triggerBodies, string(body))
	f.mu.Unlock()
	if f.killConnection {
		if f.lateCommitAfterListCalls > 0 {
			// The server commits on a background context, so the write survives
			// the dead connection — it just becomes visible later.
			jobID := strings.Split(req.URL.Path, "/")[3]
			f.mu.Lock()
			f.pendingLate = append(f.pendingLate, jobID)
			f.mu.Unlock()
		}
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		<-req.Context().Done()
		return
	}
	if f.triggerDelay > 0 {
		select {
		case <-time.After(f.triggerDelay):
		case <-req.Context().Done():
			return
		}
	}
	if f.triggerStatus != 0 {
		w.WriteHeader(f.triggerStatus)
		fmt.Fprint(w, f.triggerBody)
		return
	}
	runID := fixtureRunID(n)
	jobID := strings.Split(req.URL.Path, "/")[3]
	f.mu.Lock()
	if f.runs == nil {
		f.runs, f.runJob = map[string]time.Time{}, map[string]string{}
	}
	f.runs[runID] = time.Now()
	f.runJob[runID] = jobID
	f.runOrder = append(f.runOrder, runID)
	f.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": runID})
}

// persistedEvents materializes the per-run event shape for every run admitted
// so far, exactly as the server's event store would hold it: nothing exists
// before the run does.
func (f *openFixture) persistedEvents() []serverEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []serverEvent
	var seq uint64
	for _, runID := range f.runOrder {
		admitted := f.runs[runID]
		for _, shape := range f.eventShape {
			if !shape.replay {
				seq++
			}
			out = append(out, serverEvent{
				Sequence: seq, Type: shape.typ, RunID: runID, TaskID: shape.taskID,
				Timestamp: admitted.Add(shape.offset).UTC(),
			})
		}
	}
	return out
}

// snapshotFor renders the public run read, using the fixture's own admission
// instant as the server-side clock.
func (f *openFixture) snapshotFor(runID string) runSnapshot {
	f.mu.Lock()
	admitted, ok := f.runs[runID]
	jobID := f.runJob[runID]
	f.mu.Unlock()
	if !ok {
		return runSnapshot{ID: runID, Status: "running"}
	}
	elapsed := time.Since(admitted)
	// Every timestamp the fixture REPORTS is on the server's clock, which the
	// skew knob can put ahead of or behind the driver's.
	admitted = admitted.Add(f.clockSkew)
	if elapsed < f.runDelay {
		return runSnapshot{ID: runID, JobID: jobID, Status: "running", CreatedAt: admitted, StartedAt: admitted, TotalTasks: 1}
	}
	status := f.runStatus
	if status == "" {
		status = "succeeded"
	}
	completed := admitted.Add(f.runDelay)
	snap := runSnapshot{ID: runID, JobID: jobID, Status: status, CreatedAt: admitted, StartedAt: admitted, CompletedAt: &completed, TotalTasks: 1}
	task := taskSnapshot{ID: "task-" + runID, Status: "succeeded", CreatedAt: admitted}
	if f.cacheHit {
		task.CacheHit = true
		snap.CacheHits = 1
	} else {
		started := admitted.Add(10 * time.Millisecond)
		task.StartedAt = &started
		task.CompletedAt = &completed
		snap.ExecutedTasks = 1
	}
	snap.Tasks = []taskSnapshot{task}
	return snap
}

func (f *openFixture) serveRunRead(w http.ResponseWriter, req *http.Request) {
	parts := strings.Split(req.URL.Path, "/")
	_ = json.NewEncoder(w).Encode(f.snapshotFor(parts[len(parts)-1]))
}

func (f *openFixture) serveRunList(w http.ResponseWriter, req *http.Request) {
	jobID := strings.Split(req.URL.Path, "/")[3]
	f.mu.Lock()
	f.listCalls++
	if f.lateCommitAfterListCalls > 0 && f.listCalls >= f.lateCommitAfterListCalls {
		for _, pendingJob := range f.pendingLate {
			runID := fixtureRunID(f.triggers.Add(1000))
			if f.runs == nil {
				f.runs, f.runJob = map[string]time.Time{}, map[string]string{}
			}
			f.runs[runID] = time.Now()
			f.runJob[runID] = pendingJob
			f.runOrder = append(f.runOrder, runID)
		}
		f.pendingLate = nil
	}
	f.mu.Unlock()
	f.mu.Lock()
	ids := make([]string, 0, len(f.runJob))
	for id, job := range f.runJob {
		if job == jobID {
			ids = append(ids, id)
		}
	}
	f.mu.Unlock()
	out := make([]runSnapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.snapshotFor(id))
	}
	_ = json.NewEncoder(w).Encode(out)
}

// serveEvents replays the declared backlog past the cursor, then holds the
// connection open the way the real SSE endpoint does.
func (f *openFixture) serveEvents(w http.ResponseWriter, req *http.Request) {
	if f.stallHeaders {
		// Connection accepted, no response ever written.
		<-req.Context().Done()
		return
	}
	var cursor uint64
	if raw := req.URL.Query().Get("cursor"); raw != "" {
		_, _ = fmt.Sscanf(raw, "%d", &cursor)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	fmt.Fprint(w, ": ping\n\n")
	if flusher != nil {
		flusher.Flush()
	}
	// Serve the persisted backlog, then keep pushing newly persisted events the
	// way the real live stream does, until the subscriber goes away.
	sent := map[uint64]int{}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		counts := map[uint64]int{}
		for _, evt := range f.persistedEvents() {
			counts[evt.Sequence]++
			if evt.Sequence <= cursor || counts[evt.Sequence] <= sent[evt.Sequence] {
				continue
			}
			sent[evt.Sequence]++
			data, _ := json.Marshal(evt)
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", evt.Sequence, evt.Type, data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		select {
		case <-req.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func openConfig(server string) config {
	return config{
		serverURL: server, jobCount: 2, fanOut: 1, depth: 1, taskDuration: time.Millisecond,
		concurrency: 1, sampleRate: 50 * time.Millisecond, timeout: 60 * time.Second,
		engine: "docker", image: "busybox:1.36.1",
		mode: modeOpen, rate: 20, arrivalWindow: 500 * time.Millisecond,
		maxInFlight: 16, lateBudget: time.Second, drainTimeout: 10 * time.Second,
		reconcileWorkers: 8, pollInterval: 20 * time.Millisecond,
		cacheMode: cacheOff, lifecycle: false, dockerHost: "unix:///var/run/docker.sock",
	}
}

func startOpenFixture(t *testing.T, f *openFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return srv
}

// offerSpread returns the interval between the first and last arrival the
// driver actually offered; in open mode a result's startedAt is its offer
// instant.
func offerSpread(r *report) (time.Duration, int) {
	var first, last time.Time
	offered := 0
	for _, rr := range r.results {
		if rr.startedAt.IsZero() {
			continue
		}
		offered++
		if first.IsZero() || rr.startedAt.Before(first) {
			first = rr.startedAt
		}
		if rr.startedAt.After(last) {
			last = rr.startedAt
		}
	}
	return last.Sub(first), offered
}

func assertLedgerBalances(t *testing.T, r *report) {
	t.Helper()
	attempted := r.runsObserved + r.runsQueued + r.runsRejected + r.runsUncertain
	if len(r.results) != r.runsDropped+attempted {
		t.Fatalf("offered=%d != dropped=%d + attempted=%d", len(r.results), r.runsDropped, attempted)
	}
	settled := r.runsSucceeded + r.runsFailed + r.runsCancelled + r.runsSkipped + r.runsUnreconciled
	if r.runsObserved != settled {
		t.Fatalf("admitted=%d != settled=%d", r.runsObserved, settled)
	}
}

// TestArrivalsDoNotWaitOnCompletion is the open-loop property itself: neither a
// stalled trigger response nor a long-running run may slow the arrival plan. A
// closed loop would need triggerDelay*N (or runDelay*N) to offer the same work.
func TestArrivalsDoNotWaitOnCompletion(t *testing.T) {
	cases := []struct {
		name         string
		triggerDelay time.Duration
		runDelay     time.Duration
	}{
		{name: "slow trigger response", triggerDelay: 300 * time.Millisecond},
		{name: "slow run completion", runDelay: 3 * time.Second},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &openFixture{triggerDelay: tt.triggerDelay, runDelay: tt.runDelay}
			srv := startOpenFixture(t, f)
			r, err := newHarness(openConfig(srv.URL)).run(context.Background())
			if err != nil {
				t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
			}
			spread, offered := offerSpread(r)
			if offered != 10 {
				t.Fatalf("offered %d arrivals, want the full plan of 10", offered)
			}
			// The plan is 10 arrivals at 20/s, so the last offer belongs ~450ms
			// after the first. A closed loop would need >=3s here.
			if spread > 1500*time.Millisecond {
				t.Fatalf("arrivals took %s to be offered: the scheduler waited on completion", spread)
			}
			if r.runsDropped != 0 || r.runsObserved != 10 || r.runsSucceeded != 10 {
				t.Fatalf("dropped=%d admitted=%d succeeded=%d", r.runsDropped, r.runsObserved, r.runsSucceeded)
			}
		})
	}
}

// TestDriverDropsBoundOverloadInsteadOfQueueing proves both driver-side drop
// reasons are counted rather than absorbed, and the ledger still balances.
func TestDriverDropsBoundOverloadInsteadOfQueueing(t *testing.T) {
	t.Run("in-flight cap", func(t *testing.T) {
		f := &openFixture{triggerDelay: 200 * time.Millisecond}
		srv := startOpenFixture(t, f)
		cfg := openConfig(srv.URL)
		cfg.maxInFlight = 1
		r, err := newHarness(cfg).run(context.Background())
		if err != nil {
			t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
		}
		if r.dropReasons["client_in_flight_cap"] == 0 {
			t.Fatalf("a full in-flight cap produced no drops: %v", r.dropReasons)
		}
		assertLedgerBalances(t, r)
	})
	t.Run("scheduler lag", func(t *testing.T) {
		srv := startOpenFixture(t, &openFixture{})
		cfg := openConfig(srv.URL)
		cfg.lateBudget = time.Nanosecond
		r, _ := newHarness(cfg).run(context.Background())
		if r.dropReasons["client_scheduler_lag"] == 0 {
			t.Fatalf("a driver that cannot keep to its own plan must drop, not burst: %v", r.dropReasons)
		}
		assertLedgerBalances(t, r)
	})
}

// TestAdmissionOutcomesAreNotConflated pins DT-ADMIT-01 and DT-QUORUM-01: a
// bare 202 is queued/skipped rather than an admission, a 4xx/5xx is a
// rejection, and a lost response is uncertain — never reported as a rejection.
func TestAdmissionOutcomesAreNotConflated(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		kill bool
		want string
	}{
		{name: "admitted", want: outcomeAdmitted},
		{name: "bare 202 is queued or skipped", code: 202, want: outcomeQueued},
		{name: "202 without an id is queued or skipped", code: 202, body: `{}`, want: outcomeQueued},
		{name: "202 with a non-uuid id is rejected", code: 202, body: `{"id":"not-a-uuid"}`, want: outcomeRejected},
		{name: "409 is rejected", code: 409, body: `{"message":"max concurrent runs reached"}`, want: outcomeRejected},
		{name: "503 is rejected", code: 503, want: outcomeRejected},
		{name: "lost response is uncertain", kill: true, want: outcomeUncertain},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			f := &openFixture{triggerStatus: tt.code, triggerBody: tt.body, killConnection: tt.kill}
			srv := startOpenFixture(t, f)
			r, err := newHarness(openConfig(srv.URL)).run(context.Background())
			counts := map[string]int{
				outcomeAdmitted: r.runsObserved, outcomeQueued: r.runsQueued,
				outcomeRejected: r.runsRejected, outcomeUncertain: r.runsUncertain,
			}
			if counts[tt.want] != 10 {
				t.Fatalf("want 10 %s, got %v (failure=%s %s)", tt.want, counts, r.failure, r.failureDetail)
			}
			assertLedgerBalances(t, r)
			if tt.want == outcomeAdmitted {
				if err != nil {
					t.Fatalf("an admitted-and-reconciled workload must pass: %v", err)
				}
				return
			}
			if r.runsObserved != 0 {
				t.Fatalf("nothing was admitted, yet admitted=%d", r.runsObserved)
			}
			if tt.want == outcomeUncertain {
				if r.runsRejected != 0 {
					t.Fatal("a lost response was reported as a server rejection")
				}
				for _, rr := range r.results {
					if !strings.Contains(rr.err.Error(), "admission unconfirmed") {
						t.Fatalf("uncertain arrival does not state the uncertainty: %v", rr.err)
					}
				}
			}
		})
	}
}

// TestReporterFailsOnUnreconciledAdmission: an acknowledged run that never
// reaches a terminal status inside the drain deadline is a failure, never a
// pass, however good the admission numbers look.
func TestReporterFailsOnUnreconciledAdmission(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{runDelay: time.Hour})
	cfg := openConfig(srv.URL)
	cfg.drainTimeout = 300 * time.Millisecond
	r, err := newHarness(cfg).run(context.Background())
	if err == nil || r.failure != "unreconciled_admission" {
		t.Fatalf("failure=%s err=%v", r.failure, err)
	}
	if r.runsUnreconciled != 10 || r.runsObserved != 10 {
		t.Fatalf("unreconciled=%d admitted=%d", r.runsUnreconciled, r.runsObserved)
	}
	if r.outcome() != "failed" {
		t.Fatal("outcome must not be passed while an admitted run is unreconciled")
	}
	assertLedgerBalances(t, r)
}

// TestReporterRejectsFasterAdmissionWithGrowingBacklog: admission outruns
// completion, every admitted run is eventually reconciled — and the result is
// STILL not a pass.
func TestReporterRejectsFasterAdmissionWithGrowingBacklog(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{runDelay: 1500 * time.Millisecond})
	cfg := openConfig(srv.URL)
	cfg.rate, cfg.arrivalWindow = 10, 2*time.Second
	cfg.sampleRate = 100 * time.Millisecond
	cfg.requireSustained = true
	cfg.drainTimeout = 30 * time.Second
	r, err := newHarness(cfg).run(context.Background())
	if r.runsUnreconciled != 0 {
		t.Fatalf("every admitted run should still reconcile: unreconciled=%d", r.runsUnreconciled)
	}
	if err == nil || r.failure != "backlog_growth" {
		t.Fatalf("failure=%s detail=%s slope=%.3f verdict=%s", r.failure, r.failureDetail, r.backlogSlope, r.sustainedVerdict)
	}
	if r.backlogSlope <= 0 {
		t.Fatalf("backlog slope %.3f does not show growth", r.backlogSlope)
	}
	if r.admittedPerSecond <= 0 {
		t.Fatal("the admission rate should still be reported alongside the refusal")
	}
}

// TestSustainableRateIsAcceptedAndDrains is the positive control: the same
// checker, a rate the fixture absorbs, and a backlog that returns to zero.
func TestSustainableRateIsAcceptedAndDrains(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{runDelay: 50 * time.Millisecond})
	cfg := openConfig(srv.URL)
	cfg.rate, cfg.arrivalWindow = 10, 2*time.Second
	cfg.sampleRate = 100 * time.Millisecond
	cfg.requireSustained = true
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s verdict=%s slope=%.3f", r.failure, r.failureDetail, r.sustainedVerdict, r.backlogSlope)
	}
	if r.sustainedVerdict != "sustained" {
		t.Fatalf("verdict=%s slope=%.3f", r.sustainedVerdict, r.backlogSlope)
	}
	if r.backlogFinal != 0 {
		t.Fatalf("drained workload left backlog=%d", r.backlogFinal)
	}
}

// ---------------------------------------------------------------------------
// The backlog verdict, series by series
//
// The verdict decides whether "we admitted more" is throughput or queueing, so
// it is tested directly on backlog series rather than only through a live
// workload. Each case names the regime it represents and the verdict that
// regime MUST get.
// ---------------------------------------------------------------------------

// verdictFor runs the reporter's verdict over a synthetic backlog series
// sampled every second at the given arrival rate.
func verdictFor(t *testing.T, rate float64, backlog []int64) *report {
	t.Helper()
	cfg := openConfig("http://127.0.0.1:1")
	cfg.rate = rate
	cfg.arrivalWindow = time.Duration(len(backlog)-1) * time.Second
	start := time.Now().Add(-time.Duration(len(backlog)) * time.Second)
	res := &openLoopResult{windowStart: start}
	for i, depth := range backlog {
		// No queue in these fixtures, so outstanding work == acknowledged backlog.
		res.backlog = append(res.backlog, backlogSample{at: start.Add(time.Duration(i) * time.Second), backlog: depth, total: depth})
	}
	res.windowEnd = start.Add(time.Duration(len(backlog)-1) * time.Second)
	res.drainEnd = res.windowEnd
	r := buildReport(cfg, nil, metricSample{}, metricSample{}, nil, 0)
	r.open = res
	r.deriveOpenLoop()
	return r
}

// ramp builds a linear backlog series of n one-second samples rising to peak.
func ramp(n int, peak float64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = int64(math.Round(peak * float64(i) / float64(n-1)))
	}
	return out
}

// TestBacklogVerdictSeries is the regression net for the verdict rule.
//
// History this pins down (see the PR evidence): the FIRST live run of
// open-tiny-sustained at 2 runs/s failed with backlog_growth, and that was a
// TRUE POSITIVE — the server offered 1.86 runs/s but completed only 1.27/s, so
// the queue really did accumulate at ~0.59 runs/s to a depth of 15. The rule
// was subsequently changed for a DIFFERENT reason: the old rule bounded a
// per-second slope by 0.2*rate, which at the retuned rate of 0.5 runs/s is
// 0.1 backlog/s — about what ONE unit of integer backlog jitter fits across a
// ten-sample half-window, so a perfectly stationary system was a coin flip.
// The replacement scales the allowance with the measurement window and adds a
// trend arm so late-onset growth cannot hide behind a half-window mean. Every
// case below must keep holding.
func TestBacklogVerdictSeries(t *testing.T) {
	cases := []struct {
		name    string
		rate    float64
		backlog []int64
		want    string
		why     string
	}{
		{
			name: "round-1 open-tiny-sustained regression fixture",
			rate: 2,
			// Reconstructed from the recorded round-1 summary: rate 2/s, a 20 s
			// window sampled every second, offered 1.86/s vs completed 1.27/s
			// (net ~0.59/s), peak 15, close 14, fitted second-half slope 0.573.
			backlog: []int64{0, 1, 1, 2, 2, 3, 4, 4, 5, 5, 6, 7, 7, 8, 8, 9, 10, 11, 12, 14, 15},
			want:    "backlog_growing",
			why:     "the original true positive: completion could not keep up and the queue grew to 15",
		},
		{
			name:    "retuned open-tiny-sustained: stationary at one run",
			rate:    0.5,
			backlog: []int64{0, 1, 0, 1, 1, 0, 1, 1, 0, 1, 1, 1, 0, 1, 1, 0, 1, 0, 1, 1, 0},
			want:    "sustained",
			why:     "this is the regime the live workload actually runs in (peak 1, slope -0.008)",
		},
		{
			name:    "noisy but stationary: plus or minus two runs of jitter",
			rate:    0.5,
			backlog: []int64{3, 5, 2, 4, 3, 5, 3, 2, 4, 5, 3, 4, 2, 5, 3, 4, 3, 5, 2, 4, 3},
			want:    "sustained",
			why:     "jitter around a flat mean is not growth; the old per-second slope bound could flip on this",
		},
		{
			name:    "slow steady growth a mean alone could smooth over",
			rate:    0.5,
			backlog: ramp(21, 8),
			want:    "backlog_growing",
			why:     "8 queued runs accumulated against ~10 offered: a third of the work never drained",
		},
		{
			name:    "growth confined to the final quarter",
			rate:    0.5,
			backlog: []int64{2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 4, 6, 8, 10, 12},
			want:    "backlog_growing",
			why:     "late-onset growth: the level arm alone halves it, so the trend arm must catch it",
		},
		{
			name:    "cold-start ramp that then holds flat",
			rate:    0.5,
			backlog: []int64{0, 1, 2, 3, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4},
			want:    "sustained",
			why:     "filling an empty pipeline to steady state is not a regression",
		},
		{
			name:    "backlog draining away",
			rate:    0.5,
			backlog: []int64{10, 9, 9, 8, 7, 7, 6, 5, 5, 4, 4, 3, 3, 2, 2, 1, 1, 1, 0, 0, 0},
			want:    "sustained",
			why:     "a shrinking queue is the opposite of the failure this guards",
		},
		{
			name:    "high rate hides nothing: growth scales with the allowance",
			rate:    20,
			backlog: ramp(21, 300),
			want:    "backlog_growing",
			why:     "300 queued against ~200 offered is runaway at any rate",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := verdictFor(t, tt.rate, tt.backlog)
			if r.sustainedVerdict != tt.want {
				t.Fatalf("verdict=%s want=%s (%s)\n  level arm: growth %.2f vs tolerance %.2f\n  trend arm: rise %.2f vs tolerance %.2f (slope %.3f/s)",
					r.sustainedVerdict, tt.want, tt.why,
					r.backlogMeanGrowth, r.backlogTolerance, r.backlogSlopeRise, r.backlogTolerance, r.backlogSlope)
			}
		})
	}
}

// TestBacklogVerdictArmsAreBothLoadBearing removes one arm at a time and shows
// a series that only the other arm catches. Without this, either arm could rot
// into decoration.
func TestBacklogVerdictArmsAreBothLoadBearing(t *testing.T) {
	t.Run("trend arm alone catches late-onset growth", func(t *testing.T) {
		// Flat for 36 of 41 samples, then a steep climb. Averaged over an
		// 21-sample second half the climb barely moves the MEAN, so the level
		// arm is satisfied — but the queue is plainly running away.
		backlog := make([]int64, 41)
		for i := range backlog {
			backlog[i] = 2
		}
		for i, depth := range []int64{6, 12, 18, 24, 30} {
			backlog[36+i] = depth
		}
		r := verdictFor(t, 2, backlog)
		if r.backlogMeanGrowth > r.backlogTolerance {
			t.Fatalf("this fixture is meant to slip past the level arm, but growth %.2f > tolerance %.2f",
				r.backlogMeanGrowth, r.backlogTolerance)
		}
		if r.sustainedVerdict != "backlog_growing" {
			t.Fatalf("late-onset growth passed: verdict=%s level=%.2f trend=%.2f tolerance=%.2f",
				r.sustainedVerdict, r.backlogMeanGrowth, r.backlogSlopeRise, r.backlogTolerance)
		}
	})
	t.Run("level arm alone catches a step that then holds", func(t *testing.T) {
		// A step up early in the second half, flat afterwards: the fitted slope
		// over the second half is small, but the LEVEL has moved for good.
		backlog := []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}
		r := verdictFor(t, 0.5, backlog)
		if r.backlogSlopeRise > r.backlogTolerance {
			t.Fatalf("this fixture is meant to slip past the trend arm, but rise %.2f > tolerance %.2f",
				r.backlogSlopeRise, r.backlogTolerance)
		}
		if r.sustainedVerdict != "backlog_growing" {
			t.Fatalf("a sustained step up passed: verdict=%s level=%.2f trend=%.2f tolerance=%.2f",
				r.sustainedVerdict, r.backlogMeanGrowth, r.backlogSlopeRise, r.backlogTolerance)
		}
	})
}

// TestBacklogToleranceScalesWithTheWindow is the defect the rule change fixed:
// the allowance must not depend on how long the workload happened to run, and
// it must never sit inside one unit of backlog quantisation.
func TestBacklogToleranceScalesWithTheWindow(t *testing.T) {
	flat := func(n int, depth int64) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = depth
		}
		return out
	}
	short := verdictFor(t, 0.5, flat(21, 3))
	long := verdictFor(t, 0.5, flat(61, 3))
	if short.backlogTolerance < 1 || long.backlogTolerance < 1 {
		t.Fatalf("tolerance fell below one whole run: short=%.2f long=%.2f", short.backlogTolerance, long.backlogTolerance)
	}
	if long.backlogTolerance <= short.backlogTolerance {
		t.Fatalf("a longer measurement must allow proportionally more absolute drift: short=%.2f long=%.2f",
			short.backlogTolerance, long.backlogTolerance)
	}
	// A single unit of jitter must never be able to decide the verdict.
	jitter := verdictFor(t, 0.5, []int64{3, 3, 3, 3, 3, 3, 3, 3, 3, 3, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4, 4})
	if jitter.sustainedVerdict != "sustained" {
		t.Fatalf("one queued run flipped the verdict to %s (growth %.2f, tolerance %.2f)",
			jitter.sustainedVerdict, jitter.backlogMeanGrowth, jitter.backlogTolerance)
	}
}

// TestInsufficientBacklogSamplesAreInconclusive: too little evidence must not
// read as success.
func TestInsufficientBacklogSamplesAreInconclusive(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{})
	cfg := openConfig(srv.URL)
	cfg.sampleRate = 10 * time.Second // no in-window samples at all
	cfg.requireSustained = true
	r, err := newHarness(cfg).run(context.Background())
	if err == nil || r.failure != "backlog_inconclusive" {
		t.Fatalf("failure=%s err=%v verdict=%s", r.failure, err, r.sustainedVerdict)
	}
}

// TestReporterChecksTheAccountingIdentity drives the verdict function with
// ledgers no correct driver can produce.
func TestReporterChecksTheAccountingIdentity(t *testing.T) {
	cfg := openConfig("http://127.0.0.1:1")
	cfg.rate, cfg.arrivalWindow = 1, 4*time.Second
	cases := []struct {
		name    string
		results []runResult
		want    string
	}{
		{
			name:    "admitted does not equal settled",
			results: []runResult{{status: "succeeded", runID: "r1"}, {status: outcomeAdmitted, runID: "r2"}, {status: outcomeDropped}},
			want:    "accounting_mismatch",
		},
		{
			name:    "duplicate run identity",
			results: []runResult{{status: "succeeded", runID: "dup"}, {status: "succeeded", runID: "dup"}, {status: outcomeDropped}},
			want:    "run_identity_invalid",
		},
		{
			name:    "unreconciled admission",
			results: []runResult{{status: "unreconciled", runID: "r1"}, {status: outcomeDropped}},
			want:    "unreconciled_admission",
		},
		{
			name:    "admitted run failed",
			results: []runResult{{status: "failed", runID: "r1"}, {status: outcomeDropped}},
			want:    "run_failure",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := buildReport(cfg, tt.results, metricSample{}, metricSample{}, nil, 0)
			r.deriveOpenLoop()
			class, err := r.judgeOpenLoop()
			if class != tt.want || err == nil {
				t.Fatalf("class=%s err=%v want=%s", class, err, tt.want)
			}
		})
	}
	t.Run("balanced ledger passes", func(t *testing.T) {
		results := []runResult{{status: "succeeded", runID: "r1"}, {status: outcomeDropped}, {status: outcomeQueued}, {status: outcomeRejected}}
		r := buildReport(cfg, results, metricSample{}, metricSample{}, nil, 0)
		r.deriveOpenLoop()
		if class, err := r.judgeOpenLoop(); class != "" || err != nil {
			t.Fatalf("class=%s err=%v", class, err)
		}
	})
}

// TestLifecycleIntervalsComeFromObservedEvents proves the intervals are the
// server's own timestamps, not client-side guesses.
func TestLifecycleIntervalsComeFromObservedEvents(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{eventShape: []fixtureEvent{
		{typ: "run_started"},
		{typ: "task_ready", taskID: "t1", offset: 100 * time.Millisecond},
		{typ: "task_started", taskID: "t1", offset: 400 * time.Millisecond},
		// At-least-once delivery replays a landmark; it must not double-count.
		{typ: "task_started", taskID: "t1", offset: 400 * time.Millisecond, replay: true},
		{typ: "task_succeeded", taskID: "t1", offset: 1900 * time.Millisecond},
		{typ: "run_completed", offset: 2000 * time.Millisecond},
	}})
	cfg := openConfig(srv.URL)
	cfg.rate, cfg.arrivalWindow, cfg.jobCount = 1, time.Second, 1
	cfg.lifecycle = true
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	life := r.open.lifecycle
	if life.eventsStatus != "ok" {
		t.Fatalf("event stream %s: %s", life.eventsStatus, life.eventsReason)
	}
	if life.duplicates == 0 {
		t.Fatal("the replayed landmark was not detected as a duplicate")
	}
	want := map[string]time.Duration{
		"admission_to_first_task_start": 400 * time.Millisecond,
		"task_queue_wait":               300 * time.Millisecond,
		"task_execution":                1500 * time.Millisecond,
		"run_terminal":                  2000 * time.Millisecond,
	}
	for name, expected := range want {
		stats := life.interval(name, "events")
		if len(stats.samples) != 1 {
			t.Fatalf("%s: %d samples, unavailable=%v", name, len(stats.samples), stats.unavailable)
		}
		if stats.samples[0] != expected {
			t.Fatalf("%s = %s, want %s (an interval taken from the client clock would not match)", name, stats.samples[0], expected)
		}
	}
}

// TestUnobservableIntervalsAreMarkedNotZeroed: a cache hit starts no container
// and a disabled stream observes nothing. Both must say so explicitly.
func TestUnobservableIntervalsAreMarkedNotZeroed(t *testing.T) {
	t.Run("cache hit has no task start", func(t *testing.T) {
		srv := startOpenFixture(t, &openFixture{cacheHit: true, eventShape: []fixtureEvent{
			{typ: "run_started"},
			{typ: "task_cached", taskID: "t1", offset: 20 * time.Millisecond},
			{typ: "run_completed", offset: 30 * time.Millisecond},
		}})
		cfg := openConfig(srv.URL)
		cfg.rate, cfg.arrivalWindow, cfg.jobCount = 1, time.Second, 1
		cfg.lifecycle = true
		r, err := newHarness(cfg).run(context.Background())
		if err != nil {
			t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
		}
		for _, name := range []string{"task_execution", "read_task_execution"} {
			stats := r.open.lifecycle.interval(name, "")
			if len(stats.samples) != 0 {
				t.Fatalf("%s reported %d samples for a cache hit", name, len(stats.samples))
			}
			if stats.unavailable["cache_hit_no_task_start"] == 0 {
				t.Fatalf("%s unavailable reasons=%v, want cache_hit_no_task_start", name, stats.unavailable)
			}
			encoded := stats.json()
			if encoded["status"] != "unavailable" || encoded["p50_seconds"] != nil {
				t.Fatalf("%s encoded as %v: an unmeasurable interval must not encode as a number", name, encoded)
			}
		}
	})
	t.Run("stream disabled", func(t *testing.T) {
		srv := startOpenFixture(t, &openFixture{})
		cfg := openConfig(srv.URL)
		cfg.lifecycle = false
		r, err := newHarness(cfg).run(context.Background())
		if err != nil {
			t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
		}
		stats := r.open.lifecycle.interval("run_terminal", "")
		if stats.unavailable["lifecycle_disabled"] != 10 {
			t.Fatalf("unavailable=%v", stats.unavailable)
		}
	})
}

// TestArrivalsCarryADistinctCacheIdentity: run parameters are part of a task's
// cache identity, so an open-loop workload that repeats the same job must vary
// them — otherwise a server with caching on short-circuits every repeat and the
// "load" test measures cache lookups instead of execution. cache=hit is the one
// mode that deliberately reuses the warm-up identity.
func TestArrivalsCarryADistinctCacheIdentity(t *testing.T) {
	t.Run("execution workloads vary the identity", func(t *testing.T) {
		f := &openFixture{}
		srv := startOpenFixture(t, f)
		cfg := openConfig(srv.URL)
		cfg.workload = "unit"
		if _, err := newHarness(cfg).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		bodies := append([]string(nil), f.triggerBodies...)
		f.mu.Unlock()
		if len(bodies) != 10 {
			t.Fatalf("%d trigger bodies", len(bodies))
		}
		seen := map[string]bool{}
		for _, body := range bodies {
			var decoded struct {
				Params map[string]string `json:"params"`
			}
			if err := json.Unmarshal([]byte(body), &decoded); err != nil {
				t.Fatalf("trigger body %q is not JSON: %v", body, err)
			}
			nonce := decoded.Params["caesium_load_arrival"]
			if nonce == "" {
				t.Fatalf("trigger body %q carries no cache-identity nonce", body)
			}
			if seen[nonce] {
				t.Fatalf("nonce %q repeated: repeats would cache-hit instead of executing", nonce)
			}
			seen[nonce] = true
		}
	})
	t.Run("cache hit reuses the warm-up identity", func(t *testing.T) {
		f := &openFixture{}
		srv := startOpenFixture(t, f)
		cfg := openConfig(srv.URL)
		cfg.cacheMode = cacheHit
		cfg.jobCount = 1
		if _, err := newHarness(cfg).run(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		bodies := append([]string(nil), f.triggerBodies...)
		f.mu.Unlock()
		if len(bodies) < 2 {
			t.Fatalf("expected a warm-up run plus arrivals, got %d triggers", len(bodies))
		}
		for _, body := range bodies {
			if strings.Contains(body, "caesium_load_arrival") {
				t.Fatalf("cache=hit sent a per-arrival nonce, which would guarantee a miss: %q", body)
			}
		}
	})
}

// TestBackgroundMixesRunInsideTheMeasuredInterval covers the API-read and SSE
// subscriber mixes: both must actually issue work, both must be counted, and
// neither may be reported as "ok" when it was never configured.
func TestBackgroundMixesRunInsideTheMeasuredInterval(t *testing.T) {
	f := &openFixture{eventShape: []fixtureEvent{
		{typ: "run_started"},
		{typ: "run_completed", offset: 10 * time.Millisecond},
	}}
	srv := startOpenFixture(t, f)
	cfg := openConfig(srv.URL)
	cfg.rate, cfg.arrivalWindow = 5, time.Second
	cfg.apiReadRate = 20
	cfg.subscribers = 3
	cfg.lifecycle = true
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	reads := apiReadJSON(&r.open.apiReads)
	if reads["status"] != "ok" {
		t.Fatalf("api_reads=%v", reads)
	}
	if ok, _ := reads["ok"].(int64); ok < 5 {
		t.Fatalf("read mix issued too little traffic: %v", reads)
	}
	if reads["p50_seconds"] == nil {
		t.Fatalf("read latency not reported: %v", reads)
	}
	if got := r.open.subscribers.connected.Load(); got != 3 {
		t.Fatalf("connected subscribers=%d, want 3", got)
	}
	if got := r.open.subscribers.events.Load(); got == 0 {
		t.Fatal("subscribers received nothing although the workload published events")
	}
}

// TestOpenLoopJSONIsVersionedAndComplete: the machine-readable contract E3/E4
// will read must carry the ledger, the verdict and the explicit unavailability
// markers on clean stdout, with the human report on stderr.
func TestOpenLoopJSONIsVersionedAndComplete(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{})
	var stdout, stderr bytes.Buffer
	code := runMain([]string{
		"-server", srv.URL, "-mode", "open", "-workload", "unit-open",
		"-jobs", "2", "-fan-out", "1", "-depth", "1", "-task-duration", "1ms",
		"-rate", "20", "-arrival-window", "500ms", "-sample-rate", "50ms",
		"-drain-timeout", "10s", "-poll-interval", "20ms", "-timeout", "60s",
		"-lifecycle=false", "-json-output", "-",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var decoded struct {
		Schema   int            `json:"schema_version"`
		Mode     string         `json:"mode"`
		Workload string         `json:"workload"`
		Counts   map[string]int `json:"counts"`
		Plan     struct {
			Total                 int  `json:"total_offered"`
			CompletionIndependent bool `json:"completion_independent"`
		} `json:"arrival_plan"`
		Accounting map[string]any `json:"accounting"`
		Throughput map[string]any `json:"throughput"`
		Backlog    map[string]any `json:"backlog"`
		Resources  map[string]any `json:"resources"`
		APIReads   map[string]any `json:"api_reads"`
		Split      map[string]any `json:"metric_work_split"`
		Drain      map[string]any `json:"drain"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("stdout is not clean JSON: %q: %v", stdout.String(), err)
	}
	if decoded.Schema != reportSchemaVersion || decoded.Mode != modeOpen || decoded.Workload != "unit-open" {
		t.Fatalf("identity fields wrong: %+v", decoded)
	}
	if decoded.Plan.Total != 10 || !decoded.Plan.CompletionIndependent {
		t.Fatalf("arrival plan=%+v", decoded.Plan)
	}
	for _, key := range []string{"offered", "dropped", "attempted", "admitted", "queued_or_skipped",
		"rejected", "transport_uncertain", "completed_ok", "completed_failed", "unreconciled",
		"offered_equals_dropped_plus_attempted", "admitted_equals_settled", "server_run_census"} {
		if _, ok := decoded.Accounting[key]; !ok {
			t.Fatalf("accounting is missing %q: %v", key, decoded.Accounting)
		}
	}
	if decoded.Accounting["offered_equals_dropped_plus_attempted"] != true || decoded.Accounting["admitted_equals_settled"] != true {
		t.Fatalf("identity not asserted in JSON: %v", decoded.Accounting)
	}
	if decoded.Throughput["verdict"] == nil || decoded.Backlog["slope_per_second"] == nil {
		t.Fatalf("throughput=%v backlog=%v", decoded.Throughput, decoded.Backlog)
	}
	if decoded.Resources["status"] != "unavailable" || !strings.Contains(fmt.Sprint(decoded.Resources["reason"]), "not_configured") {
		t.Fatalf("an unconfigured external observation must say so: %v", decoded.Resources)
	}
	if decoded.APIReads["status"] != "unavailable" {
		t.Fatalf("api_reads=%v", decoded.APIReads)
	}
	if decoded.Split["timer_driven"] == nil || decoded.Split["workload_driven"] == nil {
		t.Fatalf("metric work split must separate timer-driven background work: %v", decoded.Split)
	}
	if decoded.Drain["all_admitted_reconciled"] != true {
		t.Fatalf("drain=%v", decoded.Drain)
	}
	if decoded.Counts["dropped"]+decoded.Counts["observed"]+decoded.Counts["rejected"]+
		decoded.Counts["queued_or_skipped"]+decoded.Counts["transport_uncertain"] != 10 {
		t.Fatalf("schema 1 counts must stay meaningful in open mode: %v", decoded.Counts)
	}
	if !strings.Contains(stderr.String(), "Open-Loop Admission Ledger") {
		t.Fatal("human report lost the admission ledger")
	}
}

// TestOpenLoopConfigurationIsValidated keeps E1's fail-closed discipline over
// the new knobs.
func TestOpenLoopConfigurationIsValidated(t *testing.T) {
	cases := map[string]func(*config){
		"mode":                func(c *config) { c.mode = "half-open" },
		"zero rate":           func(c *config) { c.rate = 0 },
		"negative rate":       func(c *config) { c.rate = -1 },
		"absurd rate":         func(c *config) { c.rate = 1e9 },
		"zero window":         func(c *config) { c.arrivalWindow = 0 },
		"zero in-flight":      func(c *config) { c.maxInFlight = 0 },
		"huge in-flight":      func(c *config) { c.maxInFlight = 1 << 20 },
		"zero late budget":    func(c *config) { c.lateBudget = 0 },
		"zero drain":          func(c *config) { c.drainTimeout = 0 },
		"zero poll":           func(c *config) { c.pollInterval = 0 },
		"zero reconcilers":    func(c *config) { c.reconcileWorkers = 0 },
		"oversized plan":      func(c *config) { c.rate, c.arrivalWindow = 1000, time.Hour },
		"negative reads":      func(c *config) { c.apiReadRate = -1 },
		"too many subs":       func(c *config) { c.subscribers = 1000 },
		"cache mode":          func(c *config) { c.cacheMode = "maybe" },
		"strategy":            func(c *config) { c.concurrencyStrategy = "sometimes" },
		"strategy needs runs": func(c *config) { c.concurrencyStrategy = "queue"; c.maxRuns = 0 },
		"container needs host": func(c *config) {
			c.resourceContainer = "srv"
			c.dockerHost = " "
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := openConfig("http://127.0.0.1:1")
			mutate(&cfg)
			r, err := newHarness(cfg).run(context.Background())
			if err == nil || r.failure != "invalid_config" {
				t.Fatalf("accepted: report=%s err=%v", r.failure, err)
			}
		})
	}
	for key, value := range map[string]string{
		"CAESIUM_LOAD_RATE": "fast", "CAESIUM_LOAD_MAX_IN_FLIGHT": "many",
		"CAESIUM_LOAD_ARRIVAL_WINDOW": "soon", "CAESIUM_LOAD_MODE": "ajar",
		"CAESIUM_LOAD_SUBSCRIBERS": "lots", "CAESIUM_LOAD_CACHE": "perhaps",
	} {
		t.Run("env "+key, func(t *testing.T) {
			t.Setenv(key, value)
			if err := defaultConfig().validate(); err == nil {
				t.Fatalf("accepted %s=%s", key, value)
			}
		})
	}
}

// TestExternalResourceObservationFailsExplicitly: an unreachable runtime socket
// or a missing container is reported with a reason, never as zero usage.
func TestExternalResourceObservationFailsExplicitly(t *testing.T) {
	for _, host := range []string{"unix:///nonexistent/docker.sock", "tcp://127.0.0.1:1"} {
		t.Run(host, func(t *testing.T) {
			if _, err := newDockerStatsClient(host); err == nil {
				t.Fatal("unreachable runtime socket accepted")
			}
		})
	}
	srv := startOpenFixture(t, &openFixture{})
	cfg := openConfig(srv.URL)
	cfg.resourceContainer = "caesium-does-not-exist"
	cfg.dockerHost = "unix:///nonexistent/docker.sock"
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	encoded := resourceJSON(r.open.resources)
	if encoded["status"] != "unavailable" || encoded["cpu_percent"] != nil {
		t.Fatalf("resources=%v", encoded)
	}
	if fmt.Sprint(encoded["reason"]) == "" {
		t.Fatal("an unavailable external observation carries no reason")
	}
}

// ---------------------------------------------------------------------------
// Workload catalog
// ---------------------------------------------------------------------------

func catalogPath(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "performance", "workloads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("workload catalog missing: %v", err)
	}
	return path
}

// TestWorkloadCatalogIsValid parses the shipped catalog and resolves every
// entry through the SAME flag set the binary uses, so an entry can never name
// an unknown knob or produce a configuration the driver would reject at run
// time — and the declared shape coverage cannot quietly shrink.
func TestWorkloadCatalogIsValid(t *testing.T) {
	c, err := loadCatalog(catalogPath(t))
	if err != nil {
		t.Fatal(err)
	}
	shapes := map[string]bool{}
	tiers := map[string]int{}
	for _, entry := range c.Workloads {
		t.Run(entry.Name, func(t *testing.T) {
			cfg := defaultConfig()
			flags := newFlagSet(&cfg, io.Discard)
			if err := applyEntry(entry, flags, nil); err != nil {
				t.Fatal(err)
			}
			cfg.workload = entry.Name
			if err := cfg.validate(); err != nil {
				t.Fatalf("catalog entry produces an invalid configuration: %v", err)
			}
			if strings.TrimSpace(entry.Description) == "" {
				t.Fatal("entry has no description")
			}
			if entry.Tier != "smoke" && entry.Tier != "extended" {
				t.Fatalf("unknown tier %q", entry.Tier)
			}
			if _, ok := entry.Expect["exit_code"]; !ok {
				t.Fatal("entry declares no expected exit code")
			}
			if cfg.mode == modeOpen && cfg.totalArrivals() < 1 {
				t.Fatal("open workload plans no arrivals")
			}
			// Every entry must say why it does or does not gate on the
			// backlog verdict, so "only one workload gates" can never again be
			// an unexamined default.
			if strings.TrimSpace(entry.SustainedRationale) == "" {
				t.Fatal("entry does not record a require-sustained rationale")
			}
			gates := entry.Driver["require-sustained"] == true
			if gates && !strings.HasPrefix(entry.SustainedRationale, "ON") {
				t.Fatalf("entry gates on the verdict but its rationale reads %q", entry.SustainedRationale)
			}
			if !gates && !strings.HasPrefix(entry.SustainedRationale, "OFF") && !strings.HasPrefix(entry.SustainedRationale, "Not applicable") {
				t.Fatalf("entry does not gate but its rationale reads %q", entry.SustainedRationale)
			}
			if !strings.Contains(cfg.aliasFor(0), entry.Name) {
				t.Fatalf("workload %q does not namespace its job aliases: %s", entry.Name, cfg.aliasFor(0))
			}
		})
		tiers[entry.Tier]++
		if entry.Driver["mode"] == "closed" {
			shapes["closed"] = true
		}
		if entry.Driver["cache"] == "hit" {
			shapes["cache_hit"] = true
		}
		if entry.Driver["cache"] == "miss" {
			shapes["cache_miss"] = true
		}
		if entry.Driver["concurrency-strategy"] == "queue" {
			shapes["queue"] = true
		}
		if entry.Driver["require-sustained"] == true {
			shapes["sustained"] = true
		}
		if rate, ok := entry.Driver["rate"].(float64); ok && rate >= 20 {
			shapes["overload"] = true
		}
		if fanOut, ok := entry.Driver["fan-out"].(float64); ok && fanOut >= 6 {
			shapes["wide"] = true
		}
		if depth, ok := entry.Driver["depth"].(float64); ok && depth >= 5 {
			shapes["deep"] = true
		}
		if rate, ok := entry.Driver["api-read-rate"].(float64); ok && rate >= 10 {
			shapes["api_reads"] = true
		}
		if subs, ok := entry.Driver["subscribers"].(float64); ok && subs >= 4 {
			shapes["subscribers"] = true
		}
		if entry.Driver["drain-timeout"] != nil {
			shapes["drain"] = true
		}
	}
	for _, want := range []string{"closed", "sustained", "overload", "wide", "deep", "queue",
		"cache_hit", "cache_miss", "api_reads", "subscribers", "drain"} {
		if !shapes[want] {
			t.Errorf("workload catalog no longer covers %q", want)
		}
	}
	if gating := func() int {
		n := 0
		for _, e := range c.Workloads {
			if e.Driver["require-sustained"] == true {
				n++
			}
		}
		return n
	}(); gating < 2 {
		t.Errorf("only %d workload(s) gate on the backlog verdict", gating)
	}
	if tiers["smoke"] < 3 {
		t.Errorf("smoke tier has only %d entries", tiers["smoke"])
	}
}

// TestCatalogSelectionAppliesAndDefersToExplicitFlags pins the precedence rule
// the integration runner relies on.
func TestCatalogSelectionAppliesAndDefersToExplicitFlags(t *testing.T) {
	cfg := defaultConfig()
	flags := newFlagSet(&cfg, io.Discard)
	args := []string{"-catalog", catalogPath(t), "-catalog-workload", "open-tiny-sustained", "-rate", "7"}
	if err := flags.Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := applyCatalog(&cfg, flags); err != nil {
		t.Fatal(err)
	}
	if cfg.mode != modeOpen || cfg.workload != "open-tiny-sustained" {
		t.Fatalf("catalog not applied: mode=%s workload=%s", cfg.mode, cfg.workload)
	}
	if cfg.rate != 7 {
		t.Fatalf("an explicit -rate must beat the catalog, got %v", cfg.rate)
	}
	if !cfg.requireSustained {
		t.Fatal("catalog knob require-sustained was not applied")
	}
}

func TestCatalogSelectionRejectsBadInput(t *testing.T) {
	path := catalogPath(t)
	cases := []struct{ name, catalog, workload, want string }{
		{"missing workload flag", path, "", "must be given together"},
		{"missing catalog flag", "", "open-tiny-sustained", "must be given together"},
		{"unknown workload", path, "does-not-exist", "is not in"},
		{"unreadable catalog", filepath.Join(t.TempDir(), "nope.json"), "x", "read workload catalog"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.catalogFile, cfg.catalogWorkload = tt.catalog, tt.workload
			flags := newFlagSet(&cfg, io.Discard)
			err := applyCatalog(&cfg, flags)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want substring %q", err, tt.want)
			}
		})
	}
}

func TestCatalogLoaderRejectsMalformedFiles(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct{ name, body, want string }{
		{"not json", "{", "parse workload catalog"},
		{"wrong schema", `{"schema_version":99,"workloads":[]}`, "schema_version"},
		{"empty", `{"schema_version":1,"workloads":[]}`, "no workloads"},
		{"unnamed", `{"schema_version":1,"workloads":[{"driver":{"jobs":1},"expect":{"exit_code":0}}]}`, "without a name"},
		{"duplicate", `{"schema_version":1,"workloads":[{"name":"a","driver":{"jobs":1},"expect":{"exit_code":0}},{"name":"a","driver":{"jobs":1},"expect":{"exit_code":0}}]}`, "twice"},
		{"no driver", `{"schema_version":1,"workloads":[{"name":"a","expect":{"exit_code":0}}]}`, "no driver flags"},
		{"no expectations", `{"schema_version":1,"workloads":[{"name":"a","driver":{"jobs":1}}]}`, "no expectations"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadCatalog(write(tt.name+".json", tt.body)); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err=%v want substring %q", err, tt.want)
			}
		})
	}
	t.Run("unknown driver flag", func(t *testing.T) {
		cfg := defaultConfig()
		flags := newFlagSet(&cfg, io.Discard)
		err := applyEntry(catalogEntry{Name: "a", Driver: map[string]any{"warp-factor": 9.0}}, flags, nil)
		if err == nil || !strings.Contains(err.Error(), "unknown driver flag") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("unsupported value type", func(t *testing.T) {
		cfg := defaultConfig()
		flags := newFlagSet(&cfg, io.Discard)
		err := applyEntry(catalogEntry{Name: "a", Driver: map[string]any{"jobs": []any{1}}}, flags, nil)
		if err == nil || !strings.Contains(err.Error(), "unsupported catalog value") {
			t.Fatalf("err=%v", err)
		}
	})
}

// ===========================================================================
// Round-1 adversarial review regressions (PR #552)
// ===========================================================================

// censusReport builds a report whose ledger carries a census outcome, so the
// census pass conditions can be driven directly.
func censusReport(t *testing.T, results []runResult, status string, extra, settled, failed int, allowFailures bool) *report {
	t.Helper()
	cfg := openConfig("http://127.0.0.1:1")
	cfg.rate, cfg.arrivalWindow = 1, 4*time.Second
	cfg.allowRunFailures = allowFailures
	lg := newLedger(0)
	lg.censusStatus, lg.censusReason = status, "probe failed"
	lg.censusExtra, lg.censusTerminal, lg.censusFailed = extra, settled, failed
	r := buildReport(cfg, results, metricSample{}, metricSample{}, nil, 0)
	r.ledger = lg
	r.deriveOpenLoop()
	return r
}

// TestCensusOutcomesArePassConditions: arrivals that acknowledge no run
// identity are reconciled ONLY through the server's run census, so a census
// that could not be taken, a discovered run that never settled, or a
// discovered run that failed must each block the pass.
func TestCensusOutcomesArePassConditions(t *testing.T) {
	queued := []runResult{{status: outcomeQueued}, {status: outcomeDropped}}
	uncertain := []runResult{{status: outcomeUncertain}, {status: outcomeDropped}}
	cases := []struct {
		name                   string
		results                []runResult
		status                 string
		extra, settled, failed int
		allowFailures          bool
		want                   string
	}{
		{"census unavailable with queued arrivals", queued, "unavailable", 0, 0, 0, false, "census_unavailable"},
		{"census unavailable with uncertain arrivals", uncertain, "unavailable", 0, 0, 0, false, "census_unavailable"},
		{"census never collected", queued, "not_collected", 0, 0, 0, false, "census_unavailable"},
		{"discovered run never settled", queued, "ok", 3, 2, 0, false, "unreconciled_census_run"},
		{"discovered run failed", queued, "ok", 2, 2, 1, false, "run_failure"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := censusReport(t, tt.results, tt.status, tt.extra, tt.settled, tt.failed, tt.allowFailures)
			class, err := r.judgeOpenLoop()
			if class != tt.want || err == nil {
				t.Fatalf("class=%s err=%v want=%s", class, err, tt.want)
			}
		})
	}
	t.Run("fully settled census passes", func(t *testing.T) {
		if class, err := censusReport(t, queued, "ok", 2, 2, 0, false).judgeOpenLoop(); class != "" || err != nil {
			t.Fatalf("class=%s err=%v", class, err)
		}
	})
	t.Run("failed discovered run allowed when the workload says so", func(t *testing.T) {
		if class, err := censusReport(t, queued, "ok", 2, 2, 1, true).judgeOpenLoop(); class != "" || err != nil {
			t.Fatalf("class=%s err=%v", class, err)
		}
	})
	t.Run("census irrelevant when every arrival carries an identity", func(t *testing.T) {
		r := censusReport(t, []runResult{{status: "succeeded", runID: "r1"}, {status: outcomeDropped}}, "unavailable", 0, 0, 0, false)
		if class, err := r.judgeOpenLoop(); class != "" || err != nil {
			t.Fatalf("no identity-less arrival needs no census: class=%s err=%v", class, err)
		}
	})
}

// TestBacklogNeverGoesNegative: acknowledged admissions and census-discovered
// runs are separate populations. Counting a discovered run into the
// acknowledged terminal tally drove the live queue workload to 7-30 = -23.
func TestBacklogNeverGoesNegative(t *testing.T) {
	t.Run("reporter rejects a negative sample", func(t *testing.T) {
		r := verdictFor(t, 0.5, []int64{2, 1, 0, -1, 0, 1, 0, 1, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1})
		if !r.backlogNegative {
			t.Fatal("a negative backlog sample was not detected")
		}
		if class, err := r.judgeOpenLoop(); class != "accounting_mismatch" || err == nil {
			t.Fatalf("class=%s err=%v", class, err)
		}
	})
	t.Run("census settlement leaves the acknowledged backlog alone", func(t *testing.T) {
		lg := newLedger(2)
		lg.admitted.Add(1)
		newHarness(openConfig("http://127.0.0.1:1")).recordCensusTerminal(lg, "succeeded")
		if got := lg.terminal.Load(); got != 0 {
			t.Fatalf("census settlement bumped the acknowledged terminal tally to %d", got)
		}
		if lg.admitted.Load()-lg.terminal.Load() < 0 {
			t.Fatal("acknowledged backlog went negative")
		}
		if lg.censusTerminal != 1 {
			t.Fatalf("censusTerminal=%d", lg.censusTerminal)
		}
	})
}

// TestQueuedWorkCountsAsOutstanding: a single-slot queue can grow without
// bound while the acknowledged backlog sits at ~1, so the verdict must judge
// acknowledged backlog PLUS observed queue depth.
func TestQueuedWorkCountsAsOutstanding(t *testing.T) {
	cfg := openConfig("http://127.0.0.1:1")
	cfg.rate, cfg.arrivalWindow = 1, 20*time.Second
	cfg.requireSustained = true
	start := time.Now().Add(-21 * time.Second)
	res := &openLoopResult{windowStart: start}
	for i := 0; i < 21; i++ {
		res.backlog = append(res.backlog, backlogSample{
			at: start.Add(time.Duration(i) * time.Second), backlog: 1,
			queued: int64(i), total: 1 + int64(i),
		})
	}
	res.windowEnd, res.drainEnd = start.Add(20*time.Second), start.Add(20*time.Second)
	r := buildReport(cfg, nil, metricSample{}, metricSample{}, nil, 0)
	r.open = res
	r.deriveOpenLoop()
	if r.sustainedVerdict != "backlog_growing" {
		t.Fatalf("a growing queue read as %s (growth %.2f, tolerance %.2f)",
			r.sustainedVerdict, r.backlogMeanGrowth, r.backlogTolerance)
	}
	if class, err := r.judgeOpenLoop(); class != "backlog_growth" || err == nil {
		t.Fatalf("class=%s err=%v", class, err)
	}
}

// TestArrivalNonceIsUniquePerInvocation: label+index alone repeats across
// invocations, so a second run of a cache-miss workload inside the TTL would
// reproduce the first run's identities and be all hits.
func TestArrivalNonceIsUniquePerInvocation(t *testing.T) {
	cfg := openConfig("http://127.0.0.1:1")
	cfg.workload = "open-cache-miss"
	pull := func(v any) string {
		return v.(map[string]any)["params"].(map[string]string)["caesium_load_arrival"]
	}
	if a, b := pull(newHarness(cfg).triggerBody(0)), pull(newHarness(cfg).triggerBody(0)); a == b {
		t.Fatalf("two invocations produced identity %q: a repeat run would be all cache hits", a)
	}
	same := newHarness(cfg)
	if pull(same.triggerBody(0)) == pull(same.triggerBody(1)) {
		t.Fatal("arrivals within one invocation must still differ")
	}
}

// TestTruncatedAdmissionBodyIsUncertain: headers arrived, the body did not.
// The run may have been created, so it is DT-QUORUM-01 uncertainty — filing it
// as a bare-202 queue/skip would silently lose a possibly-committed run.
func TestTruncatedAdmissionBodyIsUncertain(t *testing.T) {
	base := &openFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/run") {
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"11111111-0000-4000-8000-0000`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		base.serve(w, req)
	}))
	defer srv.Close()
	r, _ := newHarness(openConfig(srv.URL)).run(context.Background())
	if r.runsUncertain != 10 {
		t.Fatalf("uncertain=%d queued=%d admitted=%d: a truncated body must not file as queue/skip",
			r.runsUncertain, r.runsQueued, r.runsObserved)
	}
	assertLedgerBalances(t, r)
}

// TestArrivalWindowUsesThePlannedEnd: a slow last response must not stretch the
// measured window and deflate the offered rate.
func TestArrivalWindowUsesThePlannedEnd(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{triggerDelay: 900 * time.Millisecond})
	cfg := openConfig(srv.URL)
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	planned := cfg.arrivalWindow.Seconds()
	if diff := r.windowSeconds - planned; diff > 0.15 || diff < -0.15 {
		t.Fatalf("measured window %.3fs but the plan was %.3fs: the window tracked the last response",
			r.windowSeconds, planned)
	}
	if r.open.lastResponseAt.Before(r.open.plannedEnd) {
		t.Fatal("fixture did not outlive the planned window; the assertion proves nothing")
	}
}

// ===========================================================================
// Round-2 adversarial review regressions (PR #552)
// ===========================================================================

// warmupMetricsBody renders a metrics page whose task_run_insert counter is a
// pure function of how many triggers the fixture has served, capped at the
// warm-up count. It therefore rises ONLY while the cache warm-up runs and is
// frozen for the whole measured window.
func warmupMetricsBody(inserts int) string {
	return fmt.Sprintf("# TYPE caesium_db_busy_retries_total counter\ncaesium_db_busy_retries_total 0\n"+
		"# TYPE caesium_db_writes_total counter\n"+
		"caesium_db_writes_total{category=\"task_run_insert\"} %d\ncaesium_db_writes_total{category=\"task_run_status\"} 5\n"+
		"# TYPE caesium_db_statements_total counter\n"+
		"caesium_db_statements_total{category=\"task_run_insert\"} %d\ncaesium_db_statements_total{category=\"task_run_status\"} 2\n",
		inserts, inserts)
}

// TestWarmupWorkIsExcludedFromMeasuredMetricDeltas: the cache warm-up executes
// real COLD runs before the arrival window opens. Differencing the measured
// window from the PRE-warm-up baseline charged those executions to the
// workload, so the cache-hit entry reported warm-up SQL alongside its measured
// arrivals. The warm-up's own delta is kept as separate evidence and the
// measured sample set restarts from a fresh baseline.
func TestWarmupWorkIsExcludedFromMeasuredMetricDeltas(t *testing.T) {
	const warmupRuns = 1
	const perWarmupRun = 7
	f := &openFixture{cacheHit: true}
	f.metrics = func() string {
		served := int(f.triggers.Load())
		if served > warmupRuns {
			served = warmupRuns
		}
		return warmupMetricsBody(served * perWarmupRun)
	}
	srv := startOpenFixture(t, f)
	cfg := openConfig(srv.URL)
	cfg.jobCount, cfg.cacheMode = warmupRuns, cacheHit
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	if r.open.warmupRuns != warmupRuns {
		t.Fatalf("warmup_runs=%d", r.open.warmupRuns)
	}
	// The counter rose only during warm-up, so the MEASURED delta must be zero.
	if r.deltaTaskRunInsert != 0 {
		t.Fatalf("measured task_run_insert delta = %v: the warm-up's cold executions were charged to the measured window",
			r.deltaTaskRunInsert)
	}
	split := r.workSplitJSON()
	driven := split["workload_driven"].(map[string]any)["rows"].(float64)
	if driven != 0 {
		t.Fatalf("metric_work_split.workload_driven.rows = %v, want 0", driven)
	}
	if r.peakTaskRunStatusPerSec != 0 {
		t.Fatalf("peak rates still cover the warm-up: %v", r.peakTaskRunStatusPerSec)
	}
	// ...and the warm-up evidence must be preserved, not discarded.
	if r.open.warmup == nil {
		t.Fatal("warm-up evidence was dropped instead of reported separately")
	}
	if got := r.open.warmup.rows["task_run_insert"]; got != warmupRuns*perWarmupRun {
		t.Fatalf("warmup rows task_run_insert=%v, want %d", got, warmupRuns*perWarmupRun)
	}
	cache := r.openLoopJSON()["cache"].(map[string]any)
	warm, ok := cache["warmup"].(map[string]any)
	if !ok || warm["excluded_from_measured_deltas"] != true {
		t.Fatalf("cache.warmup=%v", cache["warmup"])
	}
	// Every measured sample must sit at or after the fresh baseline.
	if len(r.samples) < 2 || r.samples[0].phase != "baseline" {
		t.Fatalf("%d measured samples, first phase %q", len(r.samples), r.samples[0].phase)
	}
	for _, s := range r.samples {
		if s.taskRunInsert != warmupRuns*perWarmupRun {
			t.Fatalf("a measured sample at %s still carries a pre-warm-up counter value %v", s.phase, s.taskRunInsert)
		}
	}
}

// TestEndToEndLatencyUsesTheDriverClock: the driver's offer instant and the
// server's completed_at are two different clocks. Pairing them made a server
// five minutes behind report ~-5 minute run latencies and silently dropped the
// status-poll delay the report claims to include.
func TestEndToEndLatencyUsesTheDriverClock(t *testing.T) {
	const skew = -5 * time.Minute
	f := &openFixture{clockSkew: skew, runDelay: 20 * time.Millisecond}
	srv := startOpenFixture(t, f)
	r, err := newHarness(openConfig(srv.URL)).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	if r.runsSucceeded == 0 {
		t.Fatal("no successful run to measure")
	}
	if r.endToEndP50 <= 0 || r.endToEndP99 <= 0 {
		t.Fatalf("p50=%s p99=%s: the server's clock offset leaked into end-to-end latency",
			r.endToEndP50, r.endToEndP99)
	}
	if r.endToEndP99 > 30*time.Second {
		t.Fatalf("p99=%s is not a plausible driver-clock latency", r.endToEndP99)
	}
	// Each reported terminal instant must be the driver's own observation, not
	// the skewed server timestamp the fixture served.
	cutoff := time.Now().Add(skew / 2)
	for _, rr := range r.results {
		if rr.status == "succeeded" && rr.finishedAt.Before(cutoff) {
			t.Fatalf("a run's finished_at %s is on the server clock, not the driver's", rr.finishedAt)
		}
	}
	// The run_read lifecycle intervals pair two SERVER timestamps, so the skew
	// cancels there and they stay available.
	if r.open.lifecycle == nil {
		t.Fatal("no lifecycle report")
	}
}

// TestSubscribersThatEndEarlyAreLostAndReconnected: streamEvents returns nil on
// an ordinary HTTP EOF, so a subscriber that received one frame and closed used
// to leave every counter unchanged. A min-events expectation was then satisfied
// while the rest of the window ran with no fan-out at all.
func TestSubscribersThatEndEarlyAreLostAndReconnected(t *testing.T) {
	base := &openFixture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/events" {
			// One valid frame, then an ordinary clean end of body.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "id: 1\nevent: run_started\ndata: {\"sequence\":1,\"type\":\"run_started\"}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			return
		}
		base.serve(w, req)
	}))
	defer srv.Close()
	cfg := openConfig(srv.URL)
	cfg.subscribers = 2
	r, err := newHarness(cfg).run(context.Background())
	if err == nil {
		t.Fatalf("a subscriber fan-out that was never applied passed; coverage=%.3f", r.subscriberCoverage)
	}
	if r.failure != "subscriber_coverage" {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	subs := &r.open.subscribers
	if subs.lost.Load() == 0 {
		t.Fatal("a clean EOF before the window closed was not counted as a lost subscriber")
	}
	if subs.attempts.Load() <= int64(cfg.subscribers) {
		t.Fatalf("attempts=%d: the driver did not reconnect the lost streams", subs.attempts.Load())
	}
	if subs.events.Load() < int64(cfg.subscribers) {
		t.Fatalf("events=%d: the fixture should still satisfy a naive min-events expectation", subs.events.Load())
	}
	if r.subscriberCoverage >= minSubscriberCoverage {
		t.Fatalf("coverage=%.3f", r.subscriberCoverage)
	}
}

// TestHealthySubscribersReachFullCoverage is the other side of the enforced
// check: a stream that stays open for the measured interval must not be
// reported as lost, and must clear the coverage floor.
func TestHealthySubscribersReachFullCoverage(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{eventShape: []fixtureEvent{{typ: "run_started"}}})
	cfg := openConfig(srv.URL)
	cfg.rate, cfg.arrivalWindow = 5, time.Second
	cfg.subscribers = 2
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s coverage=%.3f", r.failure, r.failureDetail, r.subscriberCoverage)
	}
	if got := r.open.subscribers.lost.Load(); got != 0 {
		t.Fatalf("lost=%d on a stream that stayed open", got)
	}
	if r.subscriberCoverage < minSubscriberCoverage {
		t.Fatalf("coverage=%.3f below the enforced floor", r.subscriberCoverage)
	}
}

// TestLifecycleAggregationSurvivesLateEventDelivery covers the observer race
// under -race: the live subscriber keeps calling observe() while the report is
// built, so aggregation must read an independent copy. Reading the shared
// pointer after unlocking raced, and iterating the shared task map while it
// grew could panic outright.
func TestLifecycleAggregationSurvivesLateEventDelivery(t *testing.T) {
	const runID = "11111111-0000-4000-8000-000000000001"
	obs := newEventObserver()
	obs.observe(serverEvent{Sequence: 1, Type: "run_started", RunID: runID, Timestamp: time.Now()})
	lg := newLedger(1)
	lg.set(0, func(a *arrival) {
		a.outcome, a.runID, a.jobID = outcomeAdmitted, runID, fixtureJobID(0)
		a.reconciled, a.terminalStatus = true, "succeeded"
	})
	cfg := openConfig("http://127.0.0.1:1")
	cfg.lifecycle = true
	h := newHarness(cfg)

	stop := make(chan struct{})
	var pump sync.WaitGroup
	pump.Add(1)
	go func() {
		defer pump.Done()
		for i := 2; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			obs.observe(serverEvent{Sequence: uint64(i), Type: "task_started", RunID: runID,
				TaskID: fmt.Sprintf("task-%d", i), Timestamp: time.Now()})
		}
	}()
	life := h.buildLifecycle(context.Background(), lg, obs, 0)
	close(stop)
	pump.Wait()
	if life == nil {
		t.Fatal("no lifecycle report")
	}
	if life.eventsStatus == "disabled" {
		t.Fatalf("aggregation never reached the observed-event path: %s", life.eventsStatus)
	}
}

// TestDrainDeadlineBoundsQueuePollingAndReconcilers: with a stalling queue
// endpoint, queue polling and the census used to run outside the deadline and
// let reconcilers keep accepting completions long past -drain-timeout.
func TestDrainDeadlineBoundsQueuePollingAndReconcilers(t *testing.T) {
	base := &openFixture{runDelay: time.Hour}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/queue") {
			select {
			case <-time.After(20 * time.Second):
			case <-req.Context().Done():
			}
			return
		}
		base.serve(w, req)
	}))
	defer srv.Close()
	cfg := openConfig(srv.URL)
	cfg.concurrencyStrategy, cfg.maxRuns = "queue", 1
	cfg.drainTimeout = 500 * time.Millisecond
	began := time.Now()
	r, err := newHarness(cfg).run(context.Background())
	elapsed := time.Since(began)
	if err == nil {
		t.Fatal("runs that never settle must not pass")
	}
	if r.failure != "unreconciled_admission" {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	// Arrival window + drain timeout + slack: a 20 s queue poll must not be
	// able to extend the drain.
	if budget := cfg.arrivalWindow + cfg.drainTimeout + 5*time.Second; elapsed > budget {
		t.Fatalf("drive took %s, over the %s budget: the deadline did not bound queue polling", elapsed, budget)
	}
	if drain := r.open.drainEnd.Sub(r.open.windowEnd); drain > cfg.drainTimeout+3*time.Second {
		t.Fatalf("drain ran %s against a %s deadline", drain, cfg.drainTimeout)
	}
}

// ===========================================================================
// Round-3 adversarial review regressions (PR #552)
// ===========================================================================

// queueConfig is an open-loop config whose jobs carry a one-slot queue, so the
// bare-202 / queue-observation paths are reachable.
func queueConfig(server string) config {
	cfg := openConfig(server)
	cfg.jobCount = 1
	cfg.concurrencyStrategy, cfg.maxRuns = "queue", 1
	cfg.drainTimeout = 2 * time.Second
	return cfg
}

// TestQueueObservationFailsClosed: a bare-202 arrival's work sits in the
// concurrency queue until the dequeuer starts it, so it is accounted for ONLY
// once a successful queue read returned 0. Previously `waitForQueueDrain`
// returned silently on a read error, `pollQueueDepth` republished its last good
// value, and `judgeOpenLoop` never consulted the queue at all — so with /queue
// erroring and /runs healthy the driver exited 0 with queue rows outstanding.
func TestQueueObservationFailsClosed(t *testing.T) {
	t.Run("unreadable queue with bare-202 arrivals fails", func(t *testing.T) {
		f := &openFixture{triggerStatus: http.StatusAccepted, triggerBody: "{}", queueStatus: 503}
		srv := startOpenFixture(t, f)
		r, err := newHarness(queueConfig(srv.URL)).run(context.Background())
		if err == nil {
			t.Fatal("an unobservable queue passed")
		}
		if r.failure != "queue_unobserved" {
			t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
		}
		if r.runsQueued == 0 {
			t.Fatal("fixture produced no bare-202 arrivals; the assertion proves nothing")
		}
	})
	t.Run("queue that never empties fails", func(t *testing.T) {
		f := &openFixture{triggerStatus: http.StatusAccepted, triggerBody: "{}", queueDepth: 3}
		srv := startOpenFixture(t, f)
		r, err := newHarness(queueConfig(srv.URL)).run(context.Background())
		if err == nil {
			t.Fatal("outstanding queue rows passed")
		}
		if r.failure != "queue_not_drained" {
			t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
		}
		if r.open.queueDrainVerified {
			t.Fatal("drainage was reported verified although no read ever returned 0")
		}
	})
	t.Run("verified drainage passes", func(t *testing.T) {
		f := &openFixture{triggerStatus: http.StatusAccepted, triggerBody: "{}"}
		srv := startOpenFixture(t, f)
		r, err := newHarness(queueConfig(srv.URL)).run(context.Background())
		if err != nil {
			t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
		}
		if !r.open.queueDrainVerified || r.open.queueStatus != "ok" {
			t.Fatalf("queue status=%s verified=%v", r.open.queueStatus, r.open.queueDrainVerified)
		}
		// The first sample is taken before the polling goroutine can have run,
		// so without a synchronous first read a HEALTHY queue reads as
		// unobserved for the whole window and every queue workload goes
		// permanently inconclusive.
		if r.open.queueObsMissing {
			t.Fatal("a healthy queue was reported as unobserved in-window")
		}
		if r.sustainedVerdict == "inconclusive_queue_unobserved" {
			t.Fatalf("verdict=%s on an observable queue", r.sustainedVerdict)
		}
	})
	t.Run("missing in-window observation makes sustained inconclusive", func(t *testing.T) {
		// Every arrival is admitted, so the bare-202 gate cannot fire and the
		// verdict path is what must refuse to decide.
		f := &openFixture{queueStatus: 503}
		srv := startOpenFixture(t, f)
		cfg := queueConfig(srv.URL)
		cfg.requireSustained = true
		r, err := newHarness(cfg).run(context.Background())
		if err == nil {
			t.Fatal("a workload whose queue was never observed reported sustained")
		}
		if r.failure != "backlog_inconclusive" || r.sustainedVerdict != "inconclusive_queue_unobserved" {
			t.Fatalf("failure=%s verdict=%s detail=%s", r.failure, r.sustainedVerdict, r.failureDetail)
		}
		if !r.open.queueObsMissing {
			t.Fatal("in-window samples were not flagged as lacking a queue observation")
		}
	})
}

// TestLateCommitIsCaughtByTheFinalCensus: the server admits on a background
// context, so a transport_uncertain offer whose connection died can still
// commit AFTER the first census. A single census therefore left it forever
// unreconciled.
func TestLateCommitIsCaughtByTheFinalCensus(t *testing.T) {
	// Call 1 is the identity baseline, call 2 the first census; the run only
	// becomes visible on call 3, the final census.
	f := &openFixture{killConnection: true, lateCommitAfterListCalls: 3}
	srv := startOpenFixture(t, f)
	cfg := openConfig(srv.URL)
	cfg.jobCount, cfg.rate, cfg.arrivalWindow = 1, 4, 500*time.Millisecond
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	if r.runsUncertain == 0 {
		t.Fatal("fixture produced no transport-uncertain arrivals")
	}
	if r.open.finalCensusStatus != "ok" {
		t.Fatalf("final census status=%s reason=%s", r.open.finalCensusStatus, r.open.finalCensusReason)
	}
	if r.open.finalCensusLate == 0 {
		t.Fatal("the final census found nothing; a run committed after the first census was not caught")
	}
	if r.censusExtra != r.censusTerminal {
		t.Fatalf("censusExtra=%d censusTerminal=%d: a late-committed run was left unsettled", r.censusExtra, r.censusTerminal)
	}
	assertLedgerBalances(t, r)
}

// TestStalledOffersAreBoundedByTheDrainDeadline: h.offer used the overall run
// context, so a POST that never answered was bounded only by the whole-run
// timeout — and offers.Wait() sat in front of the deadline select.
func TestStalledOffersAreBoundedByTheDrainDeadline(t *testing.T) {
	base := &openFixture{}
	stop := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/run") {
			select {
			case <-req.Context().Done():
			case <-stop:
			}
			return
		}
		base.serve(w, req)
	}))
	defer func() { close(stop); srv.Close() }()
	cfg := openConfig(srv.URL)
	cfg.timeout, cfg.drainTimeout = 60*time.Second, 500*time.Millisecond
	began := time.Now()
	r, _ := newHarness(cfg).run(context.Background())
	elapsed := time.Since(began)
	if budget := cfg.arrivalWindow + cfg.drainTimeout + 8*time.Second; elapsed > budget {
		t.Fatalf("stalled offers ran %s, over the %s budget: they were not bound by the drain deadline", elapsed, budget)
	}
	if r.runsUncertain == 0 {
		t.Fatalf("a cancelled offer must stay transport_uncertain: uncertain=%d dropped=%d rejected=%d",
			r.runsUncertain, r.runsDropped, r.runsRejected)
	}
	assertLedgerBalances(t, r)
}

// TestMeasurementRunsUntilThePlannedWindowCloses: windowEnd was assigned the
// planned end but nothing WAITED for it, so with fast responses sampling,
// subscribers and the read mix stopped early while the report claimed the full
// window — and drain duration could come out negative. The existing
// slow-response test cannot catch this direction.
func TestMeasurementRunsUntilThePlannedWindowCloses(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{})
	cfg := openConfig(srv.URL)
	// A sparse plan with instant responses: every arrival is offered in the
	// first fraction of a second, but the window is a full second.
	cfg.rate, cfg.arrivalWindow, cfg.sampleRate = 2, time.Second, 100*time.Millisecond
	cfg.subscribers = 1
	r, err := newHarness(cfg).run(context.Background())
	if err != nil {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	if r.open.windowTruncated {
		t.Fatal("the window was truncated")
	}
	if drain := r.open.drainEnd.Sub(r.open.windowEnd); drain < 0 {
		t.Fatalf("drain duration is negative (%s): measurement stopped before the planned window closed", drain)
	}
	last := r.open.backlog[len(r.open.backlog)-1].at
	if last.Before(r.open.plannedEnd.Add(-2 * cfg.sampleRate)) {
		t.Fatalf("last backlog sample at %s, planned end %s: sampling stopped early",
			last.Sub(r.open.windowStart), cfg.arrivalWindow)
	}
	if mix := time.Duration(r.open.subscribers.mixNanos); mix < cfg.arrivalWindow {
		t.Fatalf("the subscriber mix ran %s for a %s window", mix, cfg.arrivalWindow)
	}
}

// TestSubscriberCoverageCountsOnlyConfirmedStreams: coverage timed the whole
// streamEvents call, so a server that accepted the connection and never sent
// headers was credited with the full interval although it paid none of the
// fan-out cost.
func TestSubscriberCoverageCountsOnlyConfirmedStreams(t *testing.T) {
	srv := startOpenFixture(t, &openFixture{stallHeaders: true})
	cfg := openConfig(srv.URL)
	cfg.subscribers = 2
	r, err := newHarness(cfg).run(context.Background())
	if err == nil {
		t.Fatalf("a stream that never answered reported coverage %.3f", r.subscriberCoverage)
	}
	if r.failure != "subscriber_coverage" {
		t.Fatalf("failure=%s detail=%s", r.failure, r.failureDetail)
	}
	if got := r.open.subscribers.opened.Load(); got != 0 {
		t.Fatalf("streams_opened=%d although the server never answered", got)
	}
	if r.subscriberCoverage != 0 {
		t.Fatalf("coverage=%.3f, want 0", r.subscriberCoverage)
	}
}

// TestCensusMembershipIsIdentityNotClock: membership used
// r.CreatedAt.Before(windowStart) — a SERVER stamp against the DRIVER's clock
// with 5 s of slack. A server five minutes behind put every run of the window
// "before" it, so the census saw nothing and possibly-committed work went
// unaccounted.
func TestCensusMembershipIsIdentityNotClock(t *testing.T) {
	t.Run("skewed server clock does not break membership", func(t *testing.T) {
		f := &openFixture{clockSkew: -5 * time.Minute, triggerStatus: http.StatusAccepted, triggerBody: "{}"}
		srv := startOpenFixture(t, f)
		cfg := openConfig(srv.URL)
		cfg.jobCount = 1
		// Bare 202s carry no run identity, so only the census can account for
		// them — and the fixture also registers a real run per trigger.
		r, _ := newHarness(cfg).run(context.Background())
		if r.censusStatus != "ok" {
			t.Fatalf("census status=%s reason=%s", r.censusStatus, r.censusReason)
		}
		if r.censusExtra != r.censusTerminal {
			t.Fatalf("censusExtra=%d censusTerminal=%d under a skewed clock", r.censusExtra, r.censusTerminal)
		}
	})
	t.Run("unestablishable membership fails explicitly", func(t *testing.T) {
		lg := newLedger(1)
		lg.set(0, func(a *arrival) { a.outcome = outcomeQueued })
		lg.runBaselineOK = false
		res := &openLoopResult{windowStart: time.Now()}
		h := newHarness(openConfig("http://127.0.0.1:1"))
		h.collectCensus(context.Background(), []appliedJob{{id: fixtureJobID(0)}}, lg, res, make(chan reconcileItem, 1))
		if lg.censusStatus != "unavailable" || !strings.Contains(lg.censusReason, "baseline") {
			t.Fatalf("status=%s reason=%s", lg.censusStatus, lg.censusReason)
		}
	})
}

// TestBacklogCountersAreSampledCoherently: record() used to take three
// independent atomic Load()s. An arrival that is admitted AND settled between
// the admitted and terminal reads fabricates backlog = -1, which round 1 made a
// hard accounting_mismatch — i.e. a flake that FAILS a healthy workload.
func TestBacklogCountersAreSampledCoherently(t *testing.T) {
	lg := newLedger(0)
	stop := make(chan struct{})
	var writers sync.WaitGroup
	for w := 0; w < 4; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				lg.noteOffered()
				lg.noteAdmitted()
				lg.noteTerminal()
			}
		}()
	}
	deadline, reads, bad := time.Now().Add(400*time.Millisecond), 0, int64(0)
	for time.Now().Before(deadline) {
		_, admitted, terminal := lg.counts()
		reads++
		if admitted-terminal < 0 {
			bad = admitted - terminal
			break
		}
	}
	close(stop)
	writers.Wait()
	if bad < 0 {
		t.Fatalf("a backlog sample read %d after %d reads: the counters are not sampled coherently", bad, reads)
	}
	if reads < 1000 {
		t.Fatalf("only %d samples taken; the race window was barely exercised", reads)
	}
}

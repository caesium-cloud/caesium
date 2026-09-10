package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			if decoded.Schema != 1 || decoded.Counts["expected"] != 3 || decoded.Counts[tt.want] != 3 || len(decoded.Runs) != 3 {
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

//go:build integration

// Package performance drives the versioned workload catalog
// (test/performance/workloads.json) through the real load driver
// (test/load) against a LIVE Caesium server.
//
// It is deliberately not a unit test on the driver's internals: it builds the
// driver binary, runs it exactly as an operator would, captures stdout
// SEPARATELY from stderr, and asserts on the machine-readable result the
// driver printed. The driver's own scheduling and reporting logic is proven
// hermetically in test/load/harness_test.go.
//
// This package is NOT discovered by the precompiled ./test integration runner
// (that runner compiles only the ./test package), so it is compiled and run
// explicitly:
//
//	docker run --rm --platform linux/arm64 \
//	  -v "$PWD:/bld/caesium" -w /bld/caesium \
//	  -v /var/run/docker.sock:/var/run/docker.sock \
//	  -e DOCKER_HOST=unix:///var/run/docker.sock \
//	  -e CAESIUM_MANUAL_TRIGGER_API_KEY=integration-test-key \
//	  -e CAESIUM_PERF_SERVER_CONTAINER=caesium-server-test \
//	  --network=container:caesium-server-test \
//	  caesiumcloud/caesium-builder:latest-full \
//	  sh -c 'go test -tags=integration -count=1 -timeout=30m -v ./test/performance'
//
// Environment:
//
//	CAESIUM_LOAD_SERVER            server URL (default http://127.0.0.1:8080)
//	CAESIUM_MANUAL_TRIGGER_API_KEY API key when the server requires one
//	CAESIUM_PERF_WORKLOADS         "smoke" (default), "all", or a comma list
//	CAESIUM_PERF_SERVER_CONTAINER  server container name; enables the external
//	                               resource observation and the prerequisite probe
//	CAESIUM_PERF_DRIVER_BIN        prebuilt driver binary (skips `go build`)
package performance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const catalogFile = "workloads.json"

type catalog struct {
	SchemaVersion int            `json:"schema_version"`
	Workloads     []catalogEntry `json:"workloads"`
}

type catalogEntry struct {
	Name        string         `json:"name"`
	Tier        string         `json:"tier"`
	Description string         `json:"description"`
	Requires    requires       `json:"requires"`
	Driver      map[string]any `json:"driver"`
	Expect      map[string]any `json:"expect"`
}

type requires struct {
	ServerEnv []string `json:"server_env"`
	Reason    string   `json:"reason"`
}

func loadCatalog(t *testing.T) catalog {
	t.Helper()
	raw, err := os.ReadFile(catalogFile)
	if err != nil {
		t.Fatalf("read %s: %v", catalogFile, err)
	}
	var c catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("parse %s: %v", catalogFile, err)
	}
	if c.SchemaVersion != 1 {
		t.Fatalf("%s schema_version=%d, this runner understands 1", catalogFile, c.SchemaVersion)
	}
	if len(c.Workloads) == 0 {
		t.Fatalf("%s declares no workloads", catalogFile)
	}
	return c
}

// selected returns the workloads this invocation should run.
func selected(c catalog) ([]catalogEntry, string) {
	choice := strings.TrimSpace(os.Getenv("CAESIUM_PERF_WORKLOADS"))
	if choice == "" {
		choice = "smoke"
	}
	if choice == "all" {
		return c.Workloads, choice
	}
	wanted := map[string]bool{}
	for _, name := range strings.Split(choice, ",") {
		wanted[strings.TrimSpace(name)] = true
	}
	var out []catalogEntry
	for _, entry := range c.Workloads {
		if wanted[entry.Tier] || wanted[entry.Name] {
			out = append(out, entry)
		}
	}
	return out, choice
}

// ---------------------------------------------------------------------------
// Server prerequisites
// ---------------------------------------------------------------------------

// serverEnvProbe reads the environment of the server container through the
// container runtime's API. It is how a config-gated workload is reported
// blocked rather than silently passed: if the gate cannot be verified the
// workload is skipped WITH the reason, never run and called green.
type serverEnvProbe struct {
	env       map[string]string
	available bool
	reason    string
}

func probeServerEnv() serverEnvProbe {
	container := strings.TrimSpace(os.Getenv("CAESIUM_PERF_SERVER_CONTAINER"))
	if container == "" {
		return serverEnvProbe{reason: "CAESIUM_PERF_SERVER_CONTAINER is unset, so server feature gates cannot be verified"}
	}
	socket := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	if _, err := os.Stat(socket); err != nil {
		return serverEnvProbe{reason: fmt.Sprintf("container runtime socket %s is not reachable: %v", socket, err)}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://docker/containers/"+url.PathEscape(container)+"/json", nil)
	if err != nil {
		return serverEnvProbe{reason: fmt.Sprintf("inspect %s: %v", container, err)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return serverEnvProbe{reason: fmt.Sprintf("inspect %s: %v", container, err)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return serverEnvProbe{reason: fmt.Sprintf("inspect %s: HTTP %d", container, resp.StatusCode)}
	}
	var payload struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return serverEnvProbe{reason: fmt.Sprintf("parse inspect %s: %v", container, err)}
	}
	env := map[string]string{}
	for _, entry := range payload.Config.Env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	return serverEnvProbe{env: env, available: true}
}

// unmet returns the prerequisites the server under test does not satisfy.
func (p serverEnvProbe) unmet(entry catalogEntry) string {
	if len(entry.Requires.ServerEnv) == 0 {
		return ""
	}
	if !p.available {
		return fmt.Sprintf("prerequisites %v cannot be verified: %s", entry.Requires.ServerEnv, p.reason)
	}
	var missing []string
	for _, want := range entry.Requires.ServerEnv {
		key, value, ok := strings.Cut(want, "=")
		if !ok {
			missing = append(missing, want)
			continue
		}
		if !strings.EqualFold(p.env[key], value) {
			missing = append(missing, fmt.Sprintf("%s (server has %q, want %q)", key, p.env[key], value))
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf("server does not provide %s: %s", strings.Join(missing, ", "), entry.Requires.Reason)
}

// ---------------------------------------------------------------------------
// Driver invocation
// ---------------------------------------------------------------------------

// buildDriver compiles the real load driver once per run. Driving the compiled
// binary — rather than importing its internals — is what makes this an
// end-to-end check of the surface an operator actually uses.
func buildDriver(t *testing.T) string {
	t.Helper()
	if prebuilt := strings.TrimSpace(os.Getenv("CAESIUM_PERF_DRIVER_BIN")); prebuilt != "" {
		if _, err := os.Stat(prebuilt); err != nil {
			t.Fatalf("CAESIUM_PERF_DRIVER_BIN=%s is not usable: %v", prebuilt, err)
		}
		return prebuilt
	}
	out := filepath.Join(t.TempDir(), "caesium-load-driver")
	buildCtx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, "go", "build", "-o", out, "../load")
	cmd.Env = os.Environ()
	combined, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build load driver: %v\n%s", err, combined)
	}
	return out
}

// driverResult is one driver invocation: its exit code, the machine-readable
// JSON it wrote to STDOUT, and the human report it wrote to STDERR. The two
// streams are captured separately on purpose — a merged capture would hide a
// log line leaking into the machine-readable stream.
type driverResult struct {
	exitCode int
	stdout   []byte
	stderr   string
	report   map[string]any
	elapsed  time.Duration
}

func runDriver(t *testing.T, binary, catalogPath, workload string, extra ...string) driverResult {
	t.Helper()
	args := append([]string{
		"-catalog", catalogPath,
		"-catalog-workload", workload,
		"-server", serverURL(),
		"-json-output", "-",
	}, extra...)
	if container := strings.TrimSpace(os.Getenv("CAESIUM_PERF_SERVER_CONTAINER")); container != "" {
		args = append(args, "-resource-container", container)
	}
	// The driver enforces its own overall deadline; this outer one only stops a
	// wedged process from hanging the lane forever.
	runCtx, cancel := context.WithTimeout(t.Context(), 45*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(runCtx, binary, args...)
	cmd.Env = os.Environ()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	began := time.Now()
	err := cmd.Run()
	res := driverResult{stdout: []byte(stdout.String()), stderr: stderr.String(), elapsed: time.Since(began)}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.exitCode = exitErr.ExitCode()
	default:
		t.Fatalf("run driver for %s: %v\nstderr:\n%s", workload, err, res.stderr)
	}
	if err := json.Unmarshal(res.stdout, &res.report); err != nil {
		t.Fatalf("workload %s did not write clean JSON to stdout (a log line leaking onto stdout looks exactly like this): %v\nstdout:\n%s\nstderr:\n%s",
			workload, err, truncate(string(res.stdout), 2000), truncate(res.stderr, 4000))
	}
	return res
}

func serverURL() string {
	if v := strings.TrimSpace(os.Getenv("CAESIUM_LOAD_SERVER")); v != "" {
		return v
	}
	return "http://127.0.0.1:8080"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…truncated…"
}

// ---------------------------------------------------------------------------
// Expectation vocabulary
// ---------------------------------------------------------------------------

func lookup(m map[string]any, path ...string) (any, bool) {
	var current any = m
	for _, key := range path {
		asMap, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = asMap[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func number(t *testing.T, m map[string]any, path ...string) float64 {
	t.Helper()
	value, ok := lookup(m, path...)
	if !ok {
		t.Fatalf("result is missing %s", strings.Join(path, "."))
	}
	f, ok := value.(float64)
	if !ok {
		t.Fatalf("%s = %v, want a number", strings.Join(path, "."), value)
	}
	return f
}

// assertExpectations evaluates every declared expectation. An unknown key is a
// hard failure: a catalog entry must never carry an assertion nothing checks.
func assertExpectations(t *testing.T, entry catalogEntry, res driverResult) {
	t.Helper()
	report := res.report
	checked := 0
	for key, want := range entry.Expect {
		checked++
		switch key {
		case "exit_code":
			if got := float64(res.exitCode); got != want.(float64) {
				t.Errorf("exit code %d, want %v\nfailure=%v %v\nstderr:\n%s",
					res.exitCode, want, report["failure_class"], report["failure_detail"], truncate(res.stderr, 6000))
			}
		case "accounting_identity":
			if want != true {
				continue
			}
			for _, flag := range []string{"offered_equals_dropped_plus_attempted", "admitted_equals_settled"} {
				value, ok := lookup(report, "accounting", flag)
				if !ok || value != true {
					t.Errorf("accounting.%s = %v; the admission ledger does not balance: %v", flag, value, report["accounting"])
				}
			}
			offered := number(t, report, "accounting", "offered")
			sum := number(t, report, "accounting", "dropped") + number(t, report, "accounting", "admitted") +
				number(t, report, "accounting", "queued_or_skipped") + number(t, report, "accounting", "rejected") +
				number(t, report, "accounting", "transport_uncertain")
			if offered != sum {
				t.Errorf("offered=%v but the buckets sum to %v", offered, sum)
			}
		case "min_offered":
			if got := number(t, report, "accounting", "offered"); got < want.(float64) {
				t.Errorf("offered=%v, want >= %v", got, want)
			}
		case "min_admitted":
			if got := number(t, report, "accounting", "admitted"); got < want.(float64) {
				t.Errorf("admitted=%v, want >= %v", got, want)
			}
		case "min_succeeded":
			if got := number(t, report, "counts", "succeeded"); got < want.(float64) {
				t.Errorf("succeeded=%v, want >= %v", got, want)
			}
		case "max_unreconciled":
			if got := number(t, report, "accounting", "unreconciled"); got > want.(float64) {
				t.Errorf("unreconciled=%v, want <= %v; an admitted run that never reached a terminal status is never a pass", got, want)
			}
		case "min_overload_signal":
			signal := number(t, report, "accounting", "dropped") +
				number(t, report, "accounting", "rejected") +
				number(t, report, "accounting", "queued_or_skipped")
			if signal < want.(float64) {
				t.Errorf("overload produced only %v drop/reject/skip outcomes, want >= %v: overload that is not counted is overload that is not measured", signal, want)
			}
		case "sustained_verdict":
			value, _ := lookup(report, "throughput", "verdict")
			if value != want {
				t.Errorf("throughput.verdict=%v, want %v (backlog slope %v)", value, want, mustLookup(report, "backlog", "slope_per_second"))
			}
		case "max_backlog_final":
			if got := number(t, report, "backlog", "final"); got > want.(float64) {
				t.Errorf("backlog.final=%v, want <= %v", got, want)
			}
		case "require_drain_complete":
			if want != true {
				continue
			}
			value, _ := lookup(report, "drain", "all_admitted_reconciled")
			if value != true {
				t.Errorf("drain did not reconcile every admitted run: %v", report["drain"])
			}
		case "max_queue_depth_final":
			status, _ := lookup(report, "drain", "queue_status")
			if status != "ok" {
				t.Errorf("queue depth unavailable after drain (%v): %v", status, mustLookup(report, "drain", "queue_reason"))
				continue
			}
			if got := number(t, report, "drain", "queue_depth_final"); got > want.(float64) {
				t.Errorf("queue_depth_final=%v, want <= %v", got, want)
			}
		case "min_queued_or_skipped":
			if got := number(t, report, "accounting", "queued_or_skipped"); got < want.(float64) {
				t.Errorf("queued_or_skipped=%v, want >= %v", got, want)
			}
		case "min_cache_hit_ratio", "max_cache_hit_ratio":
			status, _ := lookup(report, "cache", "status")
			if status == "unavailable" {
				t.Errorf("cache ratio unavailable: %v", mustLookup(report, "cache", "reason"))
				continue
			}
			got := number(t, report, "cache", "hit_ratio")
			if key == "min_cache_hit_ratio" && got < want.(float64) {
				t.Errorf("cache hit ratio %v, want >= %v (%v)", got, want, report["cache"])
			}
			if key == "max_cache_hit_ratio" && got > want.(float64) {
				t.Errorf("cache hit ratio %v, want <= %v (%v)", got, want, report["cache"])
			}
		case "min_api_reads_ok":
			status, _ := lookup(report, "api_reads", "status")
			if status != "ok" {
				t.Errorf("api_reads unavailable: %v", report["api_reads"])
				continue
			}
			if got := number(t, report, "api_reads", "ok"); got < want.(float64) {
				t.Errorf("api_reads.ok=%v, want >= %v", got, want)
			}
		case "min_subscriber_events":
			if got := number(t, report, "subscribers", "events_received"); got < want.(float64) {
				t.Errorf("subscribers received %v events, want >= %v: %v", got, want, report["subscribers"])
			}
		case "min_subscriber_coverage":
			// events_received alone is satisfiable by one frame per stream
			// followed by a disconnect, which leaves the window running with
			// none of the fan-out the workload claims. Coverage is the check
			// that the subscriptions were actually held open.
			if got := number(t, report, "subscribers", "coverage_ratio"); got < want.(float64) {
				t.Errorf("subscribers held the event stream for only %.3f of the measured interval, want >= %v: %v",
					got, want, report["subscribers"])
			}
		case "require_lifecycle_ok":
			for _, raw := range want.([]any) {
				name := raw.(string)
				status, ok := lookup(report, "lifecycle", "intervals", name, "status")
				if !ok {
					t.Errorf("lifecycle interval %q is absent from the result", name)
					continue
				}
				if status != "ok" {
					reasons, _ := lookup(report, "lifecycle", "intervals", name, "unavailable_reasons")
					t.Errorf("lifecycle interval %q is %v: %v", name, status, reasons)
				}
			}
		case "require_unavailable_reason":
			for name, reason := range want.(map[string]any) {
				reasons, ok := lookup(report, "lifecycle", "intervals", name, "unavailable_reasons")
				if !ok {
					t.Errorf("lifecycle interval %q is absent from the result", name)
					continue
				}
				asMap, _ := reasons.(map[string]any)
				if asMap[reason.(string)] == nil {
					t.Errorf("lifecycle interval %q does not carry the %q marker: %v", name, reason, reasons)
				}
				if status, _ := lookup(report, "lifecycle", "intervals", name, "status"); status != "unavailable" {
					t.Errorf("lifecycle interval %q reports %v although it cannot be measured", name, status)
				}
			}
		case "max_duration_seconds":
			if res.elapsed.Seconds() > want.(float64) {
				t.Errorf("workload took %s, want <= %vs: a bounded driver must not hang under overload", res.elapsed, want)
			}
		default:
			t.Fatalf("workload %q declares expectation %q, which this runner does not implement", entry.Name, key)
		}
	}
	if checked == 0 {
		t.Fatalf("workload %q asserted nothing", entry.Name)
	}
}

func mustLookup(m map[string]any, path ...string) any {
	value, _ := lookup(m, path...)
	return value
}

// ---------------------------------------------------------------------------
// The lane
// ---------------------------------------------------------------------------

// TestLoadCatalogWorkloads runs the selected catalog entries against the live
// server and asserts each one's declared invariants on the driver's own
// machine-readable result.
func TestLoadCatalogWorkloads(t *testing.T) {
	c := loadCatalog(t)
	entries, choice := selected(c)
	if len(entries) == 0 {
		t.Fatalf("CAESIUM_PERF_WORKLOADS=%q selected none of the %d catalog workloads", choice, len(c.Workloads))
	}
	catalogPath, err := filepath.Abs(catalogFile)
	if err != nil {
		t.Fatal(err)
	}
	binary := buildDriver(t)
	probe := probeServerEnv()
	t.Logf("server=%s selection=%q workloads=%d prerequisite-probe-available=%t (%s)",
		serverURL(), choice, len(entries), probe.available, probe.reason)

	executed, blocked := 0, []string{}
	for _, entry := range entries {
		if reason := probe.unmet(entry); reason != "" {
			blocked = append(blocked, entry.Name+": "+reason)
			t.Run(entry.Name, func(t *testing.T) { t.Skipf("BLOCKED — %s", reason) })
			continue
		}
		executed++
		t.Run(entry.Name, func(t *testing.T) {
			res := runDriver(t, binary, catalogPath, entry.Name)
			t.Logf("%s: exit=%d elapsed=%s outcome=%v failure=%v",
				entry.Name, res.exitCode, res.elapsed.Round(time.Millisecond), res.report["outcome"], res.report["failure_class"])
			if accounting, ok := res.report["accounting"].(map[string]any); ok {
				t.Logf("%s ledger: offered=%v dropped=%v admitted=%v queued/skipped=%v rejected=%v uncertain=%v completed_ok=%v completed_failed=%v unreconciled=%v",
					entry.Name, accounting["offered"], accounting["dropped"], accounting["admitted"],
					accounting["queued_or_skipped"], accounting["rejected"], accounting["transport_uncertain"],
					accounting["completed_ok"], accounting["completed_failed"], accounting["unreconciled"])
				t.Logf("%s census: %v", entry.Name, accounting["server_run_census"])
			}
			if throughput, ok := res.report["throughput"].(map[string]any); ok {
				t.Logf("%s throughput: offered/s=%v admitted/s=%v completed/s=%v verdict=%v backlog_peak=%v slope=%v",
					entry.Name, throughput["offered_per_second"], throughput["admitted_per_second"],
					throughput["completed_per_second"], throughput["verdict"],
					mustLookup(res.report, "backlog", "peak"), mustLookup(res.report, "backlog", "slope_per_second"))
			}
			if intervals, ok := lookup(res.report, "lifecycle", "intervals"); ok {
				if asMap, ok := intervals.(map[string]any); ok {
					for name, data := range asMap {
						entryMap, _ := data.(map[string]any)
						t.Logf("%s lifecycle %s: status=%v n=%v p50=%v p99=%v unavailable=%v %v",
							entry.Name, name, entryMap["status"], entryMap["samples"],
							entryMap["p50_seconds"], entryMap["p99_seconds"],
							entryMap["unavailable"], entryMap["unavailable_reasons"])
					}
				}
			}
			t.Logf("%s resources: %v", entry.Name, res.report["resources"])
			t.Logf("%s sql work split: %v", entry.Name, res.report["metric_work_split"])
			assertExpectations(t, entry, res)
			if t.Failed() {
				t.Logf("%s human report:\n%s", entry.Name, truncate(res.stderr, 8000))
			}
		})
	}
	for _, reason := range blocked {
		t.Logf("BLOCKED (not a pass): %s", reason)
	}
	// Hollow-lane floor: a selection that executes nothing must fail rather
	// than exit green having proven nothing.
	if executed == 0 {
		t.Fatalf("every selected workload was blocked; nothing was executed:\n%s", strings.Join(blocked, "\n"))
	}
}

// TestDriverRejectsUnknownCatalogWorkload proves the catalog selection itself
// fails closed: a typo'd workload name must not quietly run the defaults.
func TestDriverRejectsUnknownCatalogWorkload(t *testing.T) {
	binary := buildDriver(t)
	catalogPath, err := filepath.Abs(catalogFile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-catalog", catalogPath, "-catalog-workload", "no-such-workload", "-server", serverURL())
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("unknown workload exited 0: %s", out)
	}
	if !strings.Contains(string(out), "is not in") {
		t.Fatalf("unhelpful error for an unknown workload: %s", out)
	}
}

// TestCacheMissWorkloadRepeatsAgainstAWarmServer runs the cache-miss workload
// TWICE against the SAME live server, inside the catalog's 1 h cache TTL.
//
// This is the live half of the per-invocation arrival nonce. The first run
// leaves the server's task cache populated with its own identities; if arrival
// params were only `workload-index` they would repeat, the second run would
// reproduce those identities exactly, and every task would be a cache HIT
// while the workload still claimed to be measuring first executions. A fresh
// server cannot show this — the cache has to be warm.
func TestCacheMissWorkloadRepeatsAgainstAWarmServer(t *testing.T) {
	const workload = "open-cache-miss"
	c := loadCatalog(t)
	var entry catalogEntry
	for _, e := range c.Workloads {
		if e.Name == workload {
			entry = e
		}
	}
	if entry.Name == "" {
		t.Fatalf("%s is not in the catalog", workload)
	}
	if reason := probeServerEnv().unmet(entry); reason != "" {
		t.Skipf("BLOCKED — %s", reason)
	}
	catalogPath, err := filepath.Abs(catalogFile)
	if err != nil {
		t.Fatal(err)
	}
	binary := buildDriver(t)
	for _, pass := range []string{"cold cache", "warm cache (same server)"} {
		res := runDriver(t, binary, catalogPath, workload)
		ratio, ok := lookup(res.report, "cache", "hit_ratio")
		t.Logf("%s: exit=%d hit_ratio=%v cache=%v", pass, res.exitCode, ratio, res.report["cache"])
		if res.exitCode != 0 {
			t.Fatalf("%s: exit=%d failure=%v %v", pass, res.exitCode,
				res.report["failure_class"], res.report["failure_detail"])
		}
		if !ok {
			t.Fatalf("%s: no cache hit ratio reported", pass)
		}
		if got, isNum := ratio.(float64); !isNum || got != 0 {
			t.Fatalf("%s: cache hit ratio %v, want 0 — the arrival identities repeated across invocations, so a repeat run silently measured cache lookups instead of executions",
				pass, ratio)
		}
	}
}

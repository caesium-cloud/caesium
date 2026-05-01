package harness

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/localrun"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"gorm.io/gorm"
)

// Result is the evaluated outcome of one scenario execution.
type Result struct {
	Scenario           ResolvedScenario
	Run                *localrun.RunResult
	ExecutionError     error
	MetricObservations []MetricObservation
	LineageEvents      []lineage.RunEvent
	Failures           []string
}

// MetricObservation is the measured value for one asserted metric.
type MetricObservation struct {
	Name   string
	Labels map[string]string
	Value  float64
	Delta  float64
}

// Passed reports whether the scenario met all expectations.
func (r *Result) Passed() bool {
	return len(r.Failures) == 0
}

// Execute runs one scenario and evaluates its expectations.
func Execute(ctx context.Context, scenario ResolvedScenario) (*Result, error) {
	def, err := scenario.Definition()
	if err != nil {
		return nil, err
	}

	state := placeholderState{
		JobAlias: def.Metadata.Alias,
		TaskIDs:  make(map[string]uuid.UUID),
	}
	obs := &observabilityCapture{}

	runner := localrun.New(localrun.Config{
		MaxParallel: scenario.Scenario.MaxParallel,
		TaskTimeout: scenario.Scenario.TaskTimeout,
		RunTimeout:  scenario.Scenario.RunTimeout,
		OnPrepared: func(store *run.Store, db *gorm.DB, jobModel *models.Job) error {
			state.JobID = jobModel.ID

			var err error
			obs.baselines, err = captureMetricBaselines(scenario.Scenario.Expect.Metrics, state)
			if err != nil {
				return err
			}

			if scenario.Scenario.Expect.Lineage == nil {
				return nil
			}

			lineage.RegisterMetrics()

			bus := event.New()
			store.SetBus(bus)

			transport := &recordingTransport{}
			subscriber := lineage.NewSubscriber(bus, transport, "caesium-harness", db)
			subscriber.SetTransportName("harness")

			lineageCtx, cancel := context.WithCancel(ctx)
			ready := make(chan struct{})
			errCh := make(chan error, 1)
			go func() {
				errCh <- subscriber.StartWithReady(lineageCtx, ready)
			}()
			<-ready

			obs.lineageTransport = transport
			obs.lineageCancel = cancel
			obs.lineageErrCh = errCh
			return nil
		},
	})

	runResult, execErr := runner.RunWithResult(ctx, def)

	if runResult != nil {
		state.JobID = runResult.JobID
		for _, task := range runResult.Tasks {
			if task.TaskID != uuid.Nil {
				state.TaskIDs[task.Name] = task.TaskID
			}
		}
	}

	lineageEvents, lineageErr := obs.finish(runResult)

	if execErr != nil && runResult == nil {
		return nil, execErr
	}

	metricObservations, err := collectMetricObservations(scenario.Scenario.Expect.Metrics, state, obs.baselines)
	if err != nil {
		return nil, err
	}

	result := &Result{
		Scenario:           scenario,
		Run:                runResult,
		ExecutionError:     execErr,
		MetricObservations: metricObservations,
		LineageEvents:      lineageEvents,
	}
	result.Failures = evaluateScenario(scenario, runResult, execErr, metricObservations, lineageEvents, lineageErr)
	return result, nil
}

type placeholderState struct {
	JobAlias string
	JobID    uuid.UUID
	TaskIDs  map[string]uuid.UUID
}

type observabilityCapture struct {
	baselines        map[int]float64
	lineageTransport *recordingTransport
	lineageCancel    context.CancelFunc
	lineageErrCh     chan error
}

func (o *observabilityCapture) finish(runResult *localrun.RunResult) ([]lineage.RunEvent, error) {
	if o.lineageTransport == nil {
		return nil, nil
	}

	waitTarget := expectedLineageEventCount(runResult)
	waitForLineageEvents(o.lineageTransport, waitTarget, 500*time.Millisecond)

	if o.lineageCancel != nil {
		o.lineageCancel()
	}

	var err error
	if o.lineageErrCh != nil {
		err = <-o.lineageErrCh
	}

	return o.lineageTransport.Events(), err
}

type recordingTransport struct {
	mu     sync.Mutex
	events []lineage.RunEvent
}

func (t *recordingTransport) Emit(_ context.Context, evt lineage.RunEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, evt)
	return nil
}

func (t *recordingTransport) Close() error {
	return nil
}

func (t *recordingTransport) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.events)
}

func (t *recordingTransport) Events() []lineage.RunEvent {
	t.mu.Lock()
	defer t.mu.Unlock()

	events := make([]lineage.RunEvent, len(t.events))
	copy(events, t.events)
	return events
}

func evaluateScenario(
	scenario ResolvedScenario,
	runResult *localrun.RunResult,
	execErr error,
	metricObservations []MetricObservation,
	lineageEvents []lineage.RunEvent,
	lineageErr error,
) []string {
	if runResult == nil {
		return []string{"scenario did not produce a run result"}
	}

	failures := make([]string, 0)
	expect := scenario.Scenario.Expect

	if expect.RunStatus != "" && runResult.Status != expect.RunStatus {
		failures = append(failures, fmt.Sprintf("run status: expected %s, got %s", expect.RunStatus, runResult.Status))
	}

	if expect.ErrorContains != "" {
		actualError := combinedError(runResult.Error, execErr)
		if !strings.Contains(actualError, expect.ErrorContains) {
			failures = append(failures, fmt.Sprintf("run error: expected substring %q, got %q", expect.ErrorContains, actualError))
		}
	}

	taskByName := make(map[string]localrun.TaskResult, len(runResult.Tasks))
	for _, task := range runResult.Tasks {
		taskByName[task.Name] = task
	}

	for _, expectedTask := range expect.Tasks {
		task, ok := taskByName[expectedTask.Name]
		if !ok {
			failures = append(failures, fmt.Sprintf("task %s: not found in run result", expectedTask.Name))
			continue
		}

		if expectedTask.Status != "" && task.Status != expectedTask.Status {
			failures = append(failures, fmt.Sprintf("task %s status: expected %s, got %s", expectedTask.Name, expectedTask.Status, task.Status))
		}
		if expectedTask.CacheHit != nil && task.CacheHit != *expectedTask.CacheHit {
			failures = append(failures, fmt.Sprintf("task %s cacheHit: expected %t, got %t", expectedTask.Name, *expectedTask.CacheHit, task.CacheHit))
		}
		if expectedTask.SchemaViolationCount != nil && len(task.SchemaViolations) != *expectedTask.SchemaViolationCount {
			failures = append(failures, fmt.Sprintf("task %s schemaViolationCount: expected %d, got %d", expectedTask.Name, *expectedTask.SchemaViolationCount, len(task.SchemaViolations)))
		}
		if expectedTask.ErrorContains != "" && !strings.Contains(task.Error, expectedTask.ErrorContains) {
			failures = append(failures, fmt.Sprintf("task %s error: expected substring %q, got %q", expectedTask.Name, expectedTask.ErrorContains, task.Error))
		}

		for key, want := range expectedTask.Output {
			got := task.Output[key]
			if got != want {
				failures = append(failures, fmt.Sprintf("task %s output[%s]: expected %q, got %q", expectedTask.Name, key, want, got))
			}
		}

		for _, fragment := range expectedTask.LogContains {
			if !strings.Contains(task.LogText, fragment) {
				failures = append(failures, fmt.Sprintf("task %s log: expected substring %q", expectedTask.Name, fragment))
			}
		}
	}

	for i, expectedMetric := range expect.Metrics {
		observation := metricObservations[i]
		if expectedMetric.Value != nil && !floatEquals(observation.Value, *expectedMetric.Value) {
			failures = append(failures, fmt.Sprintf(
				"metric %s %s value: expected %.6f, got %.6f",
				expectedMetric.Name,
				formatMetricLabels(observation.Labels),
				*expectedMetric.Value,
				observation.Value,
			))
		}
		if expectedMetric.Delta != nil && !floatEquals(observation.Delta, *expectedMetric.Delta) {
			failures = append(failures, fmt.Sprintf(
				"metric %s %s delta: expected %.6f, got %.6f",
				expectedMetric.Name,
				formatMetricLabels(observation.Labels),
				*expectedMetric.Delta,
				observation.Delta,
			))
		}
	}

	if expect.Lineage != nil {
		if lineageErr != nil {
			failures = append(failures, fmt.Sprintf("lineage subscriber: %v", lineageErr))
		}

		if expect.Lineage.TotalEvents != nil && len(lineageEvents) != *expect.Lineage.TotalEvents {
			failures = append(failures, fmt.Sprintf("lineage totalEvents: expected %d, got %d", *expect.Lineage.TotalEvents, len(lineageEvents)))
		}

		eventTypeCounts := make(map[string]int, len(lineageEvents))
		jobNames := make(map[string]struct{}, len(lineageEvents))
		for _, evt := range lineageEvents {
			eventTypeCounts[string(evt.EventType)]++
			jobNames[evt.Job.Name] = struct{}{}
		}

		for eventType, want := range expect.Lineage.EventTypes {
			got := eventTypeCounts[eventType]
			if got != want {
				failures = append(failures, fmt.Sprintf("lineage eventTypes[%s]: expected %d, got %d", eventType, want, got))
			}
		}

		for _, jobName := range expect.Lineage.JobNames {
			if _, ok := jobNames[jobName]; !ok {
				failures = append(failures, fmt.Sprintf("lineage jobNames: expected %q to appear", jobName))
			}
		}
	}

	return failures
}

func captureMetricBaselines(expectations []MetricExpectation, state placeholderState) (map[int]float64, error) {
	if len(expectations) == 0 {
		return nil, nil
	}

	baselines := make(map[int]float64, len(expectations))
	for i, expectation := range expectations {
		if expectation.Delta == nil {
			continue
		}

		labels, dynamic, err := resolveMetricLabels(expectation.Labels, state, true)
		if err != nil {
			return nil, fmt.Errorf("metric %s baseline: %w", expectation.Name, err)
		}
		if dynamic {
			baselines[i] = 0
			continue
		}

		value, err := readMetricValue(expectation.Name, labels)
		if err != nil {
			return nil, fmt.Errorf("metric %s baseline: %w", expectation.Name, err)
		}
		baselines[i] = value
	}

	return baselines, nil
}

func collectMetricObservations(expectations []MetricExpectation, state placeholderState, baselines map[int]float64) ([]MetricObservation, error) {
	if len(expectations) == 0 {
		return nil, nil
	}

	observations := make([]MetricObservation, 0, len(expectations))
	for i, expectation := range expectations {
		labels, _, err := resolveMetricLabels(expectation.Labels, state, false)
		if err != nil {
			return nil, fmt.Errorf("metric %s labels: %w", expectation.Name, err)
		}

		value, err := readMetricValue(expectation.Name, labels)
		if err != nil {
			return nil, fmt.Errorf("metric %s: %w", expectation.Name, err)
		}

		observation := MetricObservation{
			Name:   expectation.Name,
			Labels: cloneLabels(labels),
			Value:  value,
			Delta:  value - baselines[i],
		}
		observations = append(observations, observation)
	}

	return observations, nil
}

func resolveMetricLabels(labels map[string]string, state placeholderState, allowDynamic bool) (map[string]string, bool, error) {
	if len(labels) == 0 {
		return map[string]string{}, false, nil
	}

	resolved := make(map[string]string, len(labels))
	dynamic := false
	for key, raw := range labels {
		value, unresolved, err := resolveLabelValue(raw, state, allowDynamic)
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", key, err)
		}
		if unresolved {
			dynamic = true
			continue
		}
		resolved[key] = value
	}
	return resolved, dynamic, nil
}

func resolveLabelValue(raw string, state placeholderState, allowDynamic bool) (string, bool, error) {
	switch {
	case raw == "$job_id":
		if state.JobID == uuid.Nil {
			if allowDynamic {
				return "", true, nil
			}
			return "", false, fmt.Errorf("job ID is not available")
		}
		return state.JobID.String(), false, nil
	case raw == "$job_alias":
		if strings.TrimSpace(state.JobAlias) == "" {
			if allowDynamic {
				return "", true, nil
			}
			return "", false, fmt.Errorf("job alias is not available")
		}
		return state.JobAlias, false, nil
	case strings.HasPrefix(raw, "$task_id:"):
		taskName := strings.TrimSpace(strings.TrimPrefix(raw, "$task_id:"))
		taskID, ok := state.TaskIDs[taskName]
		if !ok || taskID == uuid.Nil {
			if allowDynamic {
				return "", true, nil
			}
			return "", false, fmt.Errorf("task %q ID is not available", taskName)
		}
		return taskID.String(), false, nil
	case strings.HasPrefix(raw, "$"):
		return "", false, fmt.Errorf("unsupported placeholder %q", raw)
	default:
		return raw, false, nil
	}
}

type metricSpec struct {
	labelNames []string
	read       func([]string) (float64, error)
}

func readMetricValue(name string, labels map[string]string) (float64, error) {
	spec, ok := harnessMetricSpecs[name]
	if !ok {
		names := make([]string, 0, len(harnessMetricSpecs))
		for metricName := range harnessMetricSpecs {
			names = append(names, metricName)
		}
		sort.Strings(names)
		return 0, fmt.Errorf("unsupported metric %q (supported: %s)", name, strings.Join(names, ", "))
	}

	if len(labels) != len(spec.labelNames) {
		return 0, fmt.Errorf("expected labels %v, got %v", spec.labelNames, sortedMapKeys(labels))
	}

	labelValues := make([]string, 0, len(spec.labelNames))
	for _, labelName := range spec.labelNames {
		value, ok := labels[labelName]
		if !ok {
			return 0, fmt.Errorf("missing label %q", labelName)
		}
		labelValues = append(labelValues, value)
	}

	return spec.read(labelValues)
}

var harnessMetricSpecs = map[string]metricSpec{
	"caesium_backfill_runs_total": {
		labelNames: []string{"job_alias", "status"},
		read:       readCounterVec(metrics.BackfillRunsTotal),
	},
	"caesium_backfills_active": {
		labelNames: []string{"job_alias"},
		read:       readGaugeVec(metrics.BackfillsActive),
	},
	"caesium_callback_runs_total": {
		labelNames: []string{"job_id", "status"},
		read:       readCounterVec(metrics.CallbackRunsTotal),
	},
	"caesium_db_busy_retries_total": {
		labelNames: nil,
		read:       readCounter(metrics.DBBusyRetriesTotal),
	},
	"caesium_job_runs_total": {
		labelNames: []string{"job_id", "status"},
		read:       readCounterVec(metrics.JobRunsTotal),
	},
	"caesium_jobs_active": {
		labelNames: []string{"job_id"},
		read:       readGaugeVec(metrics.JobsActive),
	},
	"caesium_lineage_events_emitted_total": {
		labelNames: []string{"event_type", "status"},
		read:       readCounterVec(lineage.LineageEventsEmitted),
	},
	"caesium_task_cache_entries": {
		labelNames: nil,
		read:       readGauge(metrics.TaskCacheEntries),
	},
	"caesium_task_cache_hits_total": {
		labelNames: []string{"job_alias", "task_name"},
		read:       readCounterVec(metrics.TaskCacheHitsTotal),
	},
	"caesium_task_cache_misses_total": {
		labelNames: []string{"job_alias", "task_name"},
		read:       readCounterVec(metrics.TaskCacheMissesTotal),
	},
	"caesium_task_retries_total": {
		labelNames: []string{"job_alias", "task_name", "attempt"},
		read:       readCounterVec(metrics.TaskRetriesTotal),
	},
	"caesium_task_runs_total": {
		labelNames: []string{"job_id", "task_id", "engine", "status"},
		read:       readCounterVec(metrics.TaskRunsTotal),
	},
	"caesium_trigger_fires_total": {
		labelNames: []string{"job_id", "trigger_type"},
		read:       readCounterVec(metrics.TriggerFiresTotal),
	},
	"caesium_worker_claim_contention_total": {
		labelNames: []string{"node_id"},
		read:       readCounterVec(metrics.WorkerClaimContentionTotal),
	},
	"caesium_worker_claims_total": {
		labelNames: []string{"node_id"},
		read:       readCounterVec(metrics.WorkerClaimsTotal),
	},
	"caesium_worker_lease_expirations_total": {
		labelNames: []string{"node_id"},
		read:       readCounterVec(metrics.WorkerLeaseExpirationsTotal),
	},
}

func readCounter(counter prometheus.Counter) func([]string) (float64, error) {
	return func(labelValues []string) (float64, error) {
		if len(labelValues) != 0 {
			return 0, fmt.Errorf("metric does not accept labels")
		}

		var metric dto.Metric
		if err := counter.(prometheus.Metric).Write(&metric); err != nil {
			return 0, err
		}
		return metric.GetCounter().GetValue(), nil
	}
}

func readCounterVec(vec *prometheus.CounterVec) func([]string) (float64, error) {
	return func(labelValues []string) (float64, error) {
		var metric dto.Metric
		counter, err := vec.GetMetricWithLabelValues(labelValues...)
		if err != nil {
			return 0, err
		}
		if err := counter.(prometheus.Metric).Write(&metric); err != nil {
			return 0, err
		}
		return metric.GetCounter().GetValue(), nil
	}
}

func readGaugeVec(vec *prometheus.GaugeVec) func([]string) (float64, error) {
	return func(labelValues []string) (float64, error) {
		var metric dto.Metric
		gauge, err := vec.GetMetricWithLabelValues(labelValues...)
		if err != nil {
			return 0, err
		}
		if err := gauge.(prometheus.Metric).Write(&metric); err != nil {
			return 0, err
		}
		return metric.GetGauge().GetValue(), nil
	}
}

func readGauge(gauge prometheus.Gauge) func([]string) (float64, error) {
	return func(labelValues []string) (float64, error) {
		if len(labelValues) != 0 {
			return 0, fmt.Errorf("metric does not accept labels")
		}

		var metric dto.Metric
		if err := gauge.(prometheus.Metric).Write(&metric); err != nil {
			return 0, err
		}
		return metric.GetGauge().GetValue(), nil
	}
}

func expectedLineageEventCount(runResult *localrun.RunResult) int {
	if runResult == nil {
		return 0
	}

	total := 2 // run start + terminal
	for _, task := range runResult.Tasks {
		switch task.Status {
		case "succeeded", "failed":
			total += 2
		case "skipped":
			total++
		}
	}
	return total
}

func waitForLineageEvents(transport *recordingTransport, target int, timeout time.Duration) {
	if transport == nil || target <= 0 {
		return
	}

	deadline := time.Now().Add(timeout)
	for transport.Count() < target && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

func floatEquals(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}

func formatMetricLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "{}"
	}

	keys := sortedMapKeys(labels)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, labels[key]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return map[string]string{}
	}

	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}

func combinedError(runError string, execErr error) string {
	parts := make([]string, 0, 2)
	if strings.TrimSpace(runError) != "" {
		parts = append(parts, runError)
	}
	if execErr != nil {
		parts = append(parts, execErr.Error())
	}
	return strings.Join(parts, " | ")
}

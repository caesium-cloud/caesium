package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Result labels for caesium_data_assertions_total. They are the applied
// disposition, not the declared one: a `hold` declaration counts as `warn`
// until Stream C turns it into a real DatasetHold.
const (
	assertionResultPass    = "pass"
	assertionResultSeeding = "seeding"
	assertionResultWarn    = "warn"
	assertionResultFail    = "fail"
)

// DataAssertionsEnabled reports the data circuit breaker's master gate. It is
// the single read point every seam in this package consults, so the feature
// cannot be half-on (arc convention 1).
func DataAssertionsEnabled() bool {
	return env.Variables().DataAssertionsEnabled
}

// EvaluateDataAssertions is the post-task data-quality seam, a sibling to
// ValidateTaskOutputSchema / ValidateTaskOutputSchemaInstance and called from
// exactly the same three places (the fanned and unfanned local executor paths
// in internal/job, and the distributed worker's runtime executor).
//
// It is the I/O SHELL around the pure core (EvaluateAssertions, in
// data_assertions_eval.go), and does five things in this order:
//
//  1. resolves the TaskRun instance these samples belong to, dropping
//     replay-quarantined runs entirely — a what-if must never move a baseline,
//     trip an assertion or (Stream C) open a hold;
//  2. loads the step's declared produced datasets and their assertion specs off
//     the registry (models.DatasetDeclaration), never by re-parsing the jobdef;
//  3. loads the rolling baseline for every metric an assertion reads — BEFORE
//     step 4, which is what keeps this run's own sample out of the baseline it
//     is judged against (see loadAssertionBaselines);
//  4. persists the emitted samples as DatasetMetric rows, INCLUDING metrics no
//     assertion names yet — free baseline history for an assertion added later;
//  5. evaluates the declared contract and dispatches the verdict.
//
// Dispatch mirrors schema validation exactly: `fail` returns an error the
// executors escalate into a red run; `warn` persists the violations onto the
// task run and publishes data_violation_recorded so the leader-gated incident
// subscriber can see a non-failing breach. `hold` behaves as `warn` here and
// logs that the breaker itself lands in Stream C.
//
// The signature is instance-aware on purpose: a fanned step has N TaskRun rows
// per trigger and each must own its samples AND its verdict. See the fan-out
// semantics note on resolveSampleDataset.
//
// Errors: only a genuine `fail`-disposition violation returns one. Every
// infrastructure problem — a lost row, a failed metric insert, a corrupt
// registry spec — is logged and swallowed, because losing an observation must
// not turn a successful run red.
func EvaluateDataAssertions(store *Store, runID, taskID, taskRunID uuid.UUID, samples []pkgtask.DatasetMetricSample) error {
	if !DataAssertionsEnabled() {
		return nil
	}
	if store == nil || store.db == nil {
		return nil
	}

	ref := taskRunID
	if ref == uuid.Nil {
		ref = taskID
	}
	row, err := loadTaskRunByIDOrUnique(store.db, runID, ref)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		log.Warn("failed to resolve task run for dataset metrics", "run_id", runID, "task_id", taskID, "error", err)
		return nil
	}

	// A replay-quarantined run is excluded completely: a what-if must never
	// move a baseline, trip an assertion, or clear a hold.
	if row.Quarantine {
		log.Info("quarantined task suppressed dataset metric capture", "run_id", runID, "task_id", taskID, "samples", len(samples))
		return nil
	}

	ctx := context.Background()
	declarations, err := producedDatasetDeclarations(ctx, store.db, row.TaskID)
	if err != nil {
		log.Warn("failed to load declared datasets for metric attribution", "task_id", row.TaskID, "error", err)
		return nil
	}
	contracts := declaredContracts(declarations)
	if len(samples) == 0 && len(contracts) == 0 {
		// Nothing emitted and nothing declared: the common case on every task
		// in a flag-on lane, and it costs one indexed read.
		return nil
	}

	names := declaredDatasetNames(declarations)
	observed := observedByDataset(samples, names)
	now := time.Now()

	// Baselines are read BEFORE this run's samples are inserted. That is what
	// makes "the current run never counts toward its own baseline" true on the
	// worker path too, where the row may already be marked succeeded by the
	// time a later attempt's samples land.
	baselines := loadAssertionBaselines(ctx, store.db, contracts, now)

	persistDatasetMetrics(ctx, store.db, row, runID, taskID, samples, names)

	return dispatchDataAssertions(store, runID, taskID, row, contracts, observed, baselines, now)
}

// declaredContract pairs a produced-dataset registry row with its decoded
// assertion spec. Only declarations that actually declare assertions become
// contracts — a dataset with an SLO but no assertions is not evaluated.
type declaredContract struct {
	name        string
	namespace   string
	assertions  *jobdef.DatasetAssertions
	onViolation string
}

// declaredContracts decodes the assertion spec off each produced declaration.
// A row whose AssertionsJSON is corrupt is logged and skipped rather than
// failing the task: the contract cannot be evaluated, but the run is not the
// place that broke.
func declaredContracts(declarations []models.DatasetDeclaration) []declaredContract {
	contracts := make([]declaredContract, 0, len(declarations))
	for i := range declarations {
		decl := &declarations[i]
		raw := strings.TrimSpace(decl.AssertionsJSON)
		if raw == "" {
			continue
		}
		var assertions jobdef.DatasetAssertions
		if err := json.Unmarshal([]byte(raw), &assertions); err != nil {
			log.Warn("failed to decode declared assertions; skipping the dataset's contract",
				"dataset", decl.Name, "job_id", decl.JobID, "error", err)
			continue
		}
		if assertions.IsEmpty() {
			continue
		}
		namespace := ""
		if decl.Namespace != nil {
			namespace = *decl.Namespace
		}
		contracts = append(contracts, declaredContract{
			name:        decl.Name,
			namespace:   namespace,
			assertions:  &assertions,
			onViolation: jobdef.EffectiveOnViolation(decl.OnViolation),
		})
	}
	return contracts
}

// loadAssertionBaselines reads the rolling baseline for every (dataset, metric)
// an assertion reads, as of `now`. A read error yields no baseline for that
// metric, which the pure core reads as "no history": a deltaFromBaseline
// assertion then produces no verdict at all rather than a fabricated breach.
func loadAssertionBaselines(ctx context.Context, conn *gorm.DB, contracts []declaredContract, now time.Time) map[string]map[string]*BaselineStats {
	if len(contracts) == 0 {
		return nil
	}
	window := BaselineWindow()
	baselines := make(map[string]map[string]*BaselineStats, len(contracts))
	for _, contract := range contracts {
		metricNames := AssertionMetrics(contract.assertions)
		if len(metricNames) == 0 {
			continue
		}
		perMetric := make(map[string]*BaselineStats, len(metricNames))
		for _, metric := range metricNames {
			stats, err := Baseline(ctx, conn, contract.namespace, contract.name, metric, window, now)
			if err != nil {
				log.Warn("failed to load baseline for a declared assertion; evaluating without history",
					"dataset", contract.name, "metric", metric, "error", err)
				continue
			}
			perMetric[metric] = stats
		}
		baselines[contract.name] = perMetric
	}
	return baselines
}

// observedByDataset groups the emitted samples by the dataset they attribute
// to, so each declared contract is evaluated against exactly its own metrics.
func observedByDataset(samples []pkgtask.DatasetMetricSample, declared []string) map[string]map[string]float64 {
	if len(samples) == 0 {
		return nil
	}
	observed := make(map[string]map[string]float64, len(samples))
	for _, sample := range samples {
		name, ok := resolveSampleDataset(sample, declared)
		if !ok {
			continue
		}
		if observed[name] == nil {
			observed[name] = make(map[string]float64, len(samples))
		}
		observed[name][sample.Metric] = sample.Value
	}
	return observed
}

// persistDatasetMetrics writes the emitted samples as DatasetMetric rows. A
// failure here is logged and swallowed — the observation is lost, the task is
// not, and evaluation still runs on the in-memory samples.
func persistDatasetMetrics(
	ctx context.Context,
	conn *gorm.DB,
	row *models.TaskRun,
	runID, taskID uuid.UUID,
	samples []pkgtask.DatasetMetricSample,
	declared []string,
) {
	if len(samples) == 0 {
		return
	}
	rows := make([]models.DatasetMetric, 0, len(samples))
	for _, sample := range samples {
		name, ok := resolveSampleDataset(sample, declared)
		if !ok {
			continue
		}
		rows = append(rows, models.DatasetMetric{
			ID:        uuid.New(),
			TaskRunID: row.ID,
			Namespace: "",
			Name:      name,
			Metric:    sample.Metric,
			Value:     sample.Value,
		})
	}
	if len(rows) == 0 {
		return
	}
	if err := InsertDatasetMetrics(ctx, conn, rows); err != nil {
		log.Warn("failed to persist dataset metrics", "run_id", runID, "task_id", taskID, "metrics", len(rows), "error", err)
		return
	}
	log.Info("recorded dataset metrics", "run_id", runID, "task_id", taskID, "task_run_id", row.ID, "metrics", len(rows))
}

// dispatchDataAssertions evaluates every declared contract through the pure
// core and applies the recorded verdicts: it persists them onto the task run,
// counts them, and either escalates (fail) or publishes the non-failing event
// (warn / hold / seeding).
func dispatchDataAssertions(
	store *Store,
	runID, taskID uuid.UUID,
	row *models.TaskRun,
	contracts []declaredContract,
	observed map[string]map[string]float64,
	baselines map[string]map[string]*BaselineStats,
	now time.Time,
) error {
	if len(contracts) == 0 {
		return nil
	}

	minSamples := BaselineMinSamples()
	var (
		recorded []DataViolation
		enforced []DataViolation
		datasets []string
	)

	for _, contract := range contracts {
		violations := EvaluateAssertions(contract.name, contract.assertions, observed[contract.name],
			baselines[contract.name], minSamples, now)
		if len(violations) == 0 {
			metrics.DataAssertionsTotal.WithLabelValues(assertionResultPass).Inc()
			continue
		}

		datasets = append(datasets, contract.name)
		for _, violation := range violations {
			violation.Namespace = contract.namespace
			escalates := violation.Enforceable() && contract.onViolation == jobdef.DatasetOnViolationFail
			switch {
			case !violation.Enforceable():
				metrics.DataAssertionsTotal.WithLabelValues(assertionResultSeeding).Inc()
			case escalates:
				metrics.DataAssertionsTotal.WithLabelValues(assertionResultFail).Inc()
			default:
				metrics.DataAssertionsTotal.WithLabelValues(assertionResultWarn).Inc()
			}
			recorded = append(recorded, violation)
			if escalates {
				enforced = append(enforced, violation)
			}
		}

		log.Warn("data assertion violations",
			"run_id", runID,
			"task_id", taskID,
			"task_run_id", row.ID,
			"dataset", contract.name,
			"on_violation", contract.onViolation,
			"violations", len(violations),
		)
		if contract.onViolation == jobdef.DatasetOnViolationHold {
			// Recorded honestly rather than silently downgraded: the breaker
			// (DatasetHold + the admission gate) is Stream C, and until it
			// lands a hold declaration behaves exactly like warn.
			log.Info("onViolation: hold recorded as a warning; the dataset hold itself lands with the circuit breaker",
				"run_id", runID, "task_id", taskID, "dataset", contract.name)
		}
	}

	if len(recorded) == 0 {
		return nil
	}

	if err := store.SaveDataViolations(runID, row.ID, recorded); err != nil {
		log.Warn("failed to persist data violations", "run_id", runID, "task_id", taskID, "error", err)
	}

	if len(enforced) > 0 {
		// Fail mode: the task fails and its task_failed event already carries
		// the violations, so no separate event is emitted — exactly the schema
		// `fail` contract.
		return fmt.Errorf("task %s violates its declared data assertions: %s (%d violation(s))",
			taskID, enforced[0].String(), len(enforced))
	}

	publishDataViolationEvent(store, runID, taskID, len(recorded), datasets)
	return nil
}

// publishDataViolationEvent emits data_violation_recorded for a non-failing
// violation, the data-quality twin of publishSchemaViolationEvent: the task did
// NOT fail, so nothing else would ever tell the leader-gated incident
// subscriber that a declared contract broke. Best-effort; a nil store is a
// no-op.
func publishDataViolationEvent(store *Store, runID, taskID uuid.UUID, count int, datasets []string) {
	if store == nil {
		return
	}
	payload, _ := json.Marshal(struct {
		Violations int      `json:"violations"`
		Datasets   []string `json:"datasets,omitempty"`
	}{Violations: count, Datasets: datasets})
	store.PublishEvents(event.Event{
		Type:      event.TypeDataViolationRecorded,
		RunID:     runID,
		TaskID:    taskID,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	})
}

// resolveSampleDataset decides which declared dataset a sample describes.
//
//   - An explicit `dataset` selector wins outright, declared or not: the emitter
//     named it, and recording an undeclared name costs one row and buys the
//     history an assertion declared later will need.
//   - An omitted selector resolves to the step's SOLE declared produced dataset.
//   - An omitted selector on a step that declares none, or more than one, is
//     ambiguous: the sample is dropped with a log line naming the ambiguity
//     rather than being attributed to an arbitrary dataset.
//
// Fan-out semantics (data-circuit-breaker Open Question 1, answered here):
// samples are recorded PER PARTITION. Each fanned instance owns its TaskRun id,
// so a fanned producer contributes N samples per trigger and the baseline window
// counts partitions, not triggers. Evaluation is likewise per-instance — the
// post-task pipeline has no group-completion seam, and C1's
// one-active-hold-per-dataset upsert already collapses N verdicts into one hold
// with an occurrence count. The accepted limitation: an assertion over a GROUP
// aggregate (total rowCount across partitions) is not expressible in v1.
func resolveSampleDataset(sample pkgtask.DatasetMetricSample, declared []string) (string, bool) {
	if name := strings.TrimSpace(sample.Dataset); name != "" {
		return name, true
	}
	switch len(declared) {
	case 1:
		return declared[0], true
	case 0:
		log.Warn("dataset metric omitted its dataset selector but the step declares no produced dataset; dropping sample",
			"metric", sample.Metric)
		return "", false
	default:
		log.Warn("dataset metric omitted its dataset selector but the step declares several produced datasets; dropping sample",
			"metric", sample.Metric, "datasets", strings.Join(declared, ","))
		return "", false
	}
}

// producedDatasetDeclarations returns the registry rows for the datasets the
// catalog task's step declares under datasets.produces — read off the declared
// registry rather than by re-parsing the stored job definition, so the
// evaluator and the lint read one spec. The catalog task is read unscoped so a
// step retired between dispatch and completion still attributes its metrics.
func producedDatasetDeclarations(ctx context.Context, conn *gorm.DB, taskID uuid.UUID) ([]models.DatasetDeclaration, error) {
	var task models.Task
	if err := conn.WithContext(ctx).Unscoped().
		Select("id", "job_id", "name").
		Where("id = ?", taskID).
		First(&task).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}

	var declarations []models.DatasetDeclaration
	if err := conn.WithContext(ctx).
		Where("job_id = ? AND step_name = ? AND direction = ?", task.JobID, task.Name, models.DatasetDirectionProduces).
		Order("name ASC").
		Find(&declarations).Error; err != nil {
		return nil, err
	}
	return declarations, nil
}

// declaredDatasetNames projects the registry rows onto the name list
// resolveSampleDataset attributes against.
func declaredDatasetNames(declarations []models.DatasetDeclaration) []string {
	names := make([]string, 0, len(declarations))
	for i := range declarations {
		names = append(names, declarations[i].Name)
	}
	return names
}

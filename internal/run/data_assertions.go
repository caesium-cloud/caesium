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
// disposition, not the declared one — a `hold` declaration whose verdict is
// still seeding counts as `seeding`, not `hold`.
const (
	assertionResultPass    = "pass"
	assertionResultSeeding = "seeding"
	assertionResultWarn    = "warn"
	assertionResultHold    = "hold"
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
//  3. loads the rolling baseline for every metric a deltaFromBaseline assertion
//     reads — BEFORE step 5, which is what keeps this run's own sample out of
//     the baseline it is judged against (see loadAssertionBaselines);
//  4. evaluates the declared contract through the pure core;
//  5. persists the emitted samples as DatasetMetric rows — INCLUDING metrics no
//     assertion names yet, free baseline history for an assertion added later —
//     marking the ones an enforced violation rejected so they never become the
//     baseline they broke, and, in the SAME transaction, releasing any hold this
//     job's earlier breach opened on a dataset whose contract just passed
//     cleanly (the clean-run release);
//  6. persists the verdicts and dispatches them.
//
// Dispatch mirrors schema validation for two of the three modes: `fail` returns
// an error the executors escalate into a red run; `warn` persists the
// violations onto the task run and publishes data_violation_recorded. `hold` is
// the circuit breaker: the task SUCCEEDS — the work is done, and failing it
// would only invite a retry of the same data — and the DATASET is held instead,
// so every downstream consumer is admitted straight to skipped until it is
// released. The hold-open is an idempotent upsert, so a repeat breach appends
// an occurrence instead of paging again.
//
// The signature is instance-aware on purpose: a fanned step has N TaskRun rows
// per trigger and each must own its samples AND its verdict. See the fan-out
// semantics note on resolveSampleDataset.
//
// Errors: only a genuine `fail`-disposition violation returns one. Every
// infrastructure problem — a lost row, a failed metric insert, a corrupt
// registry spec — is logged and swallowed, because losing an observation must
// not turn a successful run red.
//
// KNOWN LIMITATION (tracked, not fixed here): a truncated or unreadable marker
// stream is indistinguishable from a missing metric. Both executors tolerate a
// log-read or parse failure by passing nil samples, and the marker parser's own
// `MetricsTruncated` flag (pkg/task, MaxMetricsBytes) is not threaded into this
// seam — so a lost observation currently reads as "the step never emitted this
// metric" and, under onViolation: fail, turns a green run red for an
// infrastructure reason rather than a data one. Closing it means widening the
// three executor call sites to pass the flag, which is deliberately out of
// scope for B1.
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
	namespaces := declaredNamespaces(declarations)
	observed := observedByDataset(samples, names)
	now := time.Now()

	// Baselines are read BEFORE this run's samples are inserted, so a run can
	// never be part of the baseline it is judged against. (Baseline's own
	// `created_at < asOf` cut is the second guard; neither relies on the
	// TaskRun's status, which at this seam is still `running` on all three
	// executor paths.)
	baselines := loadAssertionBaselines(ctx, store.db, contracts, now)

	verdicts := evaluateContracts(contracts, observed, baselines, now)

	persistDatasetMetrics(ctx, store, row, runID, taskID, samples, names, namespaces,
		rejectedMetrics(verdicts), cleanContracts(verdicts, baselines))

	return dispatchDataAssertions(ctx, store, runID, taskID, row, verdicts)
}

// declaredContract pairs a produced-dataset registry row with its decoded
// assertion spec. Only declarations that actually declare assertions become
// contracts — a dataset with an SLO but no assertions is not evaluated.
type declaredContract struct {
	name        string
	namespace   string
	assertions  *jobdef.DatasetAssertions
	onViolation string
	// release is produces[].release with its default applied: "auto" (the next
	// clean producer run releases an active hold) or "manual" (the hold
	// survives a clean run and needs a human ack).
	release string
	// jobID / jobAlias / stepName identify the PRODUCER. They come off the
	// registry row that is already in hand, so opening a hold and deciding a
	// clean-run release cost no extra read.
	jobID    uuid.UUID
	jobAlias string
	stepName string
}

// datasetVerdict is one dataset's evaluated contract: the verdicts the pure
// core returned, kept beside the contract that produced them so both the
// sample-marking pass and the dispatch pass read the same result.
type datasetVerdict struct {
	contract   declaredContract
	violations []DataViolation
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
		contracts = append(contracts, declaredContract{
			name:        decl.Name,
			namespace:   declarationNamespace(decl),
			assertions:  &assertions,
			onViolation: jobdef.EffectiveOnViolation(decl.OnViolation),
			release:     (&jobdef.ProducedDataset{Release: decl.Release}).EffectiveRelease(),
			jobID:       decl.JobID,
			jobAlias:    decl.JobAlias,
			stepName:    decl.StepName,
		})
	}
	return contracts
}

// evaluateContracts runs the pure core once per declared contract. It performs
// no I/O of its own, so the shell's entire decision surface is one call away
// from the function Plan 3's backtest replays.
func evaluateContracts(
	contracts []declaredContract,
	observed map[string]map[string]float64,
	baselines map[string]map[string]*BaselineStats,
	now time.Time,
) []datasetVerdict {
	if len(contracts) == 0 {
		return nil
	}
	minSamples := BaselineMinSamples()
	verdicts := make([]datasetVerdict, 0, len(contracts))
	for _, contract := range contracts {
		violations := EvaluateAssertions(contract.name, contract.assertions, observed[contract.name],
			baselines[contract.name], minSamples, now)
		for i := range violations {
			violations[i].Namespace = contract.namespace
		}
		verdicts = append(verdicts, datasetVerdict{contract: contract, violations: violations})
	}
	return verdicts
}

// metricRef identifies one (dataset, metric) pair.
type metricRef struct {
	dataset string
	metric  string
}

// rejectedMetrics is the set of (dataset, metric) pairs an ENFORCED violation
// named. Their samples are recorded but flagged, so the value the breaker just
// rejected never becomes the baseline it is next compared against. A seeding
// verdict is deliberately absent: that value was compared against a baseline
// too short to trust, so flagging it would discard honest history.
func rejectedMetrics(verdicts []datasetVerdict) map[metricRef]struct{} {
	rejected := make(map[metricRef]struct{})
	for _, verdict := range verdicts {
		for _, violation := range verdict.violations {
			if !violation.Enforceable() {
				continue
			}
			rejected[metricRef{dataset: violation.Dataset, metric: violation.Metric}] = struct{}{}
		}
	}
	return rejected
}

// loadAssertionBaselines reads the rolling baseline for every (dataset, metric)
// a deltaFromBaseline assertion reads, as of `now`. Absolute bounds and the
// freshness assertion need no history, so no query is issued for their metrics.
// A read error yields no baseline for that metric, which the pure core reads as
// "no history": a deltaFromBaseline assertion then produces no verdict at all
// rather than a fabricated breach.
func loadAssertionBaselines(ctx context.Context, conn *gorm.DB, contracts []declaredContract, now time.Time) map[string]map[string]*BaselineStats {
	if len(contracts) == 0 {
		return nil
	}
	window := BaselineWindow()
	baselines := make(map[string]map[string]*BaselineStats, len(contracts))
	for _, contract := range contracts {
		metricNames := BaselineMetrics(contract.assertions)
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

// cleanContracts is the set of contracts that passed with NO recorded verdict
// at all — the clean runs that may release a hold.
//
// A "seeding" verdict deliberately does NOT count as clean: the contract did
// not hold, it merely was not enforced because the baseline was too short. A
// dataset does not get to leave the breaker on a verdict nobody trusted.
//
// Nor does an UNEVALUATED assertion. A `deltaFromBaseline` with no usable clean
// history yields NO verdict at all — not a seeding one — and a contract whose
// only assertion is starved would otherwise land here having proved nothing and
// release the hold. Starvation is reachable precisely while a dataset is held:
// the non-held predicate removes the hold window's samples, and the retention
// pruner eventually removes the rest. So a release requires every declared
// assertion to have actually been evaluated.
func cleanContracts(verdicts []datasetVerdict, baselines map[string]map[string]*BaselineStats) []declaredContract {
	clean := make([]declaredContract, 0, len(verdicts))
	for _, verdict := range verdicts {
		if len(verdict.violations) != 0 {
			continue
		}
		if !contractFullyEvaluated(verdict.contract, baselines[verdict.contract.name]) {
			log.Info("clean-run release skipped: the contract could not be fully evaluated",
				"dataset", verdict.contract.name)
			continue
		}
		clean = append(clean, verdict.contract)
	}
	return clean
}

// contractFullyEvaluated reports whether every declared assertion produced a
// real judgement on this run. Only the baseline-reading assertions can silently
// abstain — a missing absolute-bound metric is itself a `missing` violation, so
// it never reaches here — which makes "every deltaFromBaseline metric had a
// usable baseline" the whole test.
func contractFullyEvaluated(contract declaredContract, baselines map[string]*BaselineStats) bool {
	for _, metric := range BaselineMetrics(contract.assertions) {
		stats := baselines[metric]
		if stats == nil || stats.Samples == 0 {
			return false
		}
	}
	return true
}

// persistDatasetMetrics writes the emitted samples as DatasetMetric rows,
// flagging the ones an enforced violation rejected, and releases the holds a
// clean run just cleared — both in ONE transaction, because "the dataset
// recovered" and "here is the evidence it recovered" must not be separable.
//
// A failure here is logged and swallowed — the observation is lost, the task is
// not, and the verdict has already been computed from the in-memory samples.
func persistDatasetMetrics(
	ctx context.Context,
	store *Store,
	row *models.TaskRun,
	runID, taskID uuid.UUID,
	samples []pkgtask.DatasetMetricSample,
	declared []string,
	namespaces map[string]string,
	rejected map[metricRef]struct{},
	clean []declaredContract,
) {
	if len(samples) == 0 {
		// A contract can pass with no samples only when it declares nothing
		// that needs one; the release still has to happen.
		if err := releaseHoldsForCleanRun(ctx, store, nil, runID, clean); err != nil {
			log.Warn("failed to release dataset holds after a clean run", "run_id", runID, "task_id", taskID, "error", err)
		}
		return
	}
	// One row per (dataset, metric), last write wins — the SAME collapse
	// observedByDataset does when it builds the values the verdict is computed
	// from. The marker accumulator keys on the literal (selector, metric), so a
	// step that emits `{"dataset":"D","rowCount":5}` and `{"rowCount":7}` hands
	// this seam two samples that both resolve to D: writing both would give one
	// logical run two baseline samples for one metric, one of which no verdict
	// ever judged (and which would still carry the other's Violated flag).
	rows := make([]models.DatasetMetric, 0, len(samples))
	index := make(map[metricRef]int, len(samples))
	for _, sample := range samples {
		name, ok := resolveSampleDataset(sample, declared)
		if !ok {
			continue
		}
		ref := metricRef{dataset: name, metric: sample.Metric}
		_, violated := rejected[ref]
		if at, seen := index[ref]; seen {
			rows[at].Value = sample.Value
			rows[at].Violated = violated
			continue
		}
		index[ref] = len(rows)
		rows = append(rows, models.DatasetMetric{
			ID:        uuid.New(),
			TaskRunID: row.ID,
			Namespace: namespaces[name],
			Name:      name,
			Metric:    sample.Metric,
			Value:     sample.Value,
			Violated:  violated,
		})
	}
	if len(rows) == 0 {
		if err := releaseHoldsForCleanRun(ctx, store, nil, runID, clean); err != nil {
			log.Warn("failed to release dataset holds after a clean run", "run_id", runID, "task_id", taskID, "error", err)
		}
		return
	}
	if err := releaseHoldsForCleanRun(ctx, store, rows, runID, clean); err != nil {
		log.Warn("failed to persist dataset metrics", "run_id", runID, "task_id", taskID, "metrics", len(rows), "error", err)
		return
	}
	log.Info("recorded dataset metrics", "run_id", runID, "task_id", taskID, "task_run_id", row.ID, "metrics", len(rows))
}

// releaseHoldsForCleanRun inserts this task's samples and, in the SAME
// transaction, releases every active hold the clean contracts cleared.
//
// MULTI-PRODUCER (plan Open Question 6 / design open question 2, answered
// here): only the HOLDER's clean run releases. A hold records HeldByJobID, and
// a clean run releases it only when that job is the one running now. A second
// job that also writes the dataset has not re-produced the slice that broke —
// it has produced its own — so letting it clear somebody else's hold would
// reopen the circuit on evidence about a different partition of the data. The
// escape hatch for a genuinely multi-producer dataset is the human ack
// (POST /v1/datasets/holds/:id/release), which is authenticated and audited.
//
// RELEASE POLICY: only a dataset declared `release: auto` (the default) is
// auto-released. `release: manual` stays held until a human acks it, which is
// exactly what A3's field promised and would otherwise ship inert.
//
// THE EVIDENCE MUST POSTDATE THE BREACH (see releasableHold). "The holder job
// ran clean" is not on its own a reason to reopen the circuit: this evaluator
// is per-INSTANCE, so a fanned producer whose partition 2 breaks and whose
// partition 3 passes would otherwise have partition 3 release the hold
// partition 2 just opened, and which partition finished last would decide
// whether a broken dataset stayed held.
func releaseHoldsForCleanRun(
	ctx context.Context,
	store *Store,
	rows []models.DatasetMetric,
	runID uuid.UUID,
	clean []declaredContract,
) error {
	eligible := make([]declaredContract, 0, len(clean))
	names := make([]string, 0, len(clean))
	namespaces := make(map[string]string, len(clean))
	for _, contract := range clean {
		if contract.release != jobdef.DatasetReleaseAuto {
			continue
		}
		eligible = append(eligible, contract)
		names = append(names, contract.name)
		namespaces[contract.name] = contract.namespace
	}

	if len(rows) == 0 && len(eligible) == 0 {
		return nil
	}
	if len(eligible) == 0 {
		return InsertDatasetMetrics(ctx, store.db, rows)
	}

	var events []event.Event
	err := withStoreBusyRetryContext(ctx, func() error {
		events = nil
		return store.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if len(rows) > 0 {
				if err := tx.Create(&rows).Error; err != nil {
					return err
				}
			}
			holds, err := activeHoldsForDatasetsTx(tx, namespaces, names)
			if err != nil {
				return err
			}
			if len(holds) == 0 {
				return nil
			}
			startedAt, err := runStartedAtTx(tx, runID)
			if err != nil {
				return err
			}
			for _, contract := range eligible {
				hold, ok := holds[contract.name]
				if !ok || !releasableHold(hold, contract, runID, startedAt) {
					continue
				}
				var released models.DatasetHold
				ok, err := store.releaseDatasetHoldTx(tx, hold.ID, datasetHoldReleaseRequest{
					reason: models.DatasetHoldReleaseCleanRun,
					by:     "system",
					note:   "every declared assertion passed on a later run of the holding job",
					runID:  runID,
				}, &released)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				evt, err := store.appendDatasetHoldEventTx(tx, event.TypeDatasetReleased, &released, 0, nil)
				if err != nil {
					return err
				}
				if evt != nil {
					events = append(events, *evt)
				}
				log.Info("dataset hold released by a clean run",
					"dataset", contract.name, "hold_id", hold.ID, "run_id", runID)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if len(events) > 0 {
		store.syncDatasetHoldsActiveGauge(ctx)
		store.publishEvents(events...)
	}
	return nil
}

// dispatchDataAssertions applies the evaluated verdicts: it counts them,
// persists them onto the task run, and either escalates (fail) or publishes the
// non-failing event (warn / hold / seeding).
func dispatchDataAssertions(
	ctx context.Context,
	store *Store,
	runID, taskID uuid.UUID,
	row *models.TaskRun,
	verdicts []datasetVerdict,
) error {
	if len(verdicts) == 0 {
		return nil
	}

	var (
		recorded  []DataViolation
		enforced  []DataViolation
		datasets  []string
		holdError error
	)

	for _, verdict := range verdicts {
		contract := verdict.contract
		if len(verdict.violations) == 0 {
			metrics.DataAssertionsTotal.WithLabelValues(assertionResultPass).Inc()
			continue
		}

		datasets = append(datasets, contract.name)
		var breaking []DataViolation
		for _, violation := range verdict.violations {
			escalates := violation.Enforceable() && contract.onViolation == jobdef.DatasetOnViolationFail
			holds := violation.Enforceable() && contract.onViolation == jobdef.DatasetOnViolationHold
			switch {
			case !violation.Enforceable():
				// A seeding verdict never holds, whatever onViolation says: the
				// baseline it was judged against was too short to trust, and
				// breaking the circuit on it would stop a pipeline on a guess.
				metrics.DataAssertionsTotal.WithLabelValues(assertionResultSeeding).Inc()
			case escalates:
				metrics.DataAssertionsTotal.WithLabelValues(assertionResultFail).Inc()
			case holds:
				metrics.DataAssertionsTotal.WithLabelValues(assertionResultHold).Inc()
			default:
				metrics.DataAssertionsTotal.WithLabelValues(assertionResultWarn).Inc()
			}
			recorded = append(recorded, violation)
			if escalates {
				enforced = append(enforced, violation)
			}
			if holds {
				breaking = append(breaking, violation)
			}
		}

		log.Warn("data assertion violations",
			"run_id", runID,
			"task_id", taskID,
			"task_run_id", row.ID,
			"dataset", contract.name,
			"on_violation", contract.onViolation,
			"violations", len(verdict.violations),
		)
		if len(breaking) > 0 {
			if err := openHoldForContract(ctx, store, contract, row, runID, taskID, breaking); err != nil && holdError == nil {
				holdError = err
			}
		}
	}

	if len(recorded) == 0 {
		return nil
	}

	if err := store.SaveDataViolations(runID, row.ID, recorded); err != nil {
		log.Warn("failed to persist data violations", "run_id", runID, "task_id", taskID, "error", err)
	}

	// Fail-closed: the verdicts are persisted first, so the evidence outlives
	// the failure, and only then is the un-openable hold escalated. A breaker
	// that could not trip reddens the task rather than reporting success on
	// data it just rejected.
	if holdError != nil {
		return holdError
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

// openHoldForContract breaks the circuit for one dataset: it opens (or appends
// an occurrence to) the dataset's single active hold.
//
// The task is NOT failed. That is the whole point of the hold disposition — the
// work ran to completion and the output exists; what is wrong is the DATA, and
// a red task would only invite a retry that recomputes the same numbers. The
// consequence lands on the dataset's consumers instead, at their admission gate.
//
// FAIL-CLOSED. A write failure here is NOT swallowed: it is returned, and the
// caller escalates it into a red task. A breaker that cannot trip must not
// pretend it did — the alternative is a green run, no hold, and every
// downstream consumer admitted onto data the contract just rejected, which is
// the exact outcome this feature exists to prevent. The write already goes
// through the store's contention-retry budget, so reaching this branch means a
// durable failure, not a transient lock. The failed attempt is counted under
// caesium_dataset_holds_total{reason="open_failed"}, a page-worthy series in
// its own right.
func openHoldForContract(
	ctx context.Context,
	store *Store,
	contract declaredContract,
	row *models.TaskRun,
	runID, taskID uuid.UUID,
	violations []DataViolation,
) error {
	hold, opened, err := store.openOrAppendDatasetHold(ctx, datasetHoldRequest{
		namespace:  contract.namespace,
		name:       contract.name,
		reason:     violations[0].Assertion,
		jobID:      contract.jobID,
		jobAlias:   contract.jobAlias,
		runID:      runID,
		taskID:     taskID,
		taskRunID:  row.ID,
		stepName:   contract.stepName,
		violations: violations,
	})
	if err != nil {
		metrics.DatasetHoldsTotal.WithLabelValues(HoldReasonOpenFailed).Inc()
		log.Error("failed to open a dataset hold for a breached contract; failing the task so the breach is not silently ignored",
			"run_id", runID, "task_id", taskID, "dataset", contract.name, "error", err)
		return fmt.Errorf("task %s breached the data contract on dataset %s but the hold could not be opened: %w",
			taskID, contract.name, err)
	}
	if opened {
		log.Warn("dataset held: downstream consumers will be skipped until it is released",
			"run_id", runID, "task_id", taskID, "dataset", contract.name,
			"hold_id", hold.ID, "assertion", hold.Reason, "release", contract.release)
		return nil
	}
	// Alert-once: an already-held dataset breaking again is an occurrence, not
	// a second page.
	log.Info("dataset already held; recorded another occurrence",
		"run_id", runID, "task_id", taskID, "dataset", contract.name,
		"hold_id", hold.ID, "occurrences", hold.OccurrenceCount)
	return nil
}

// publishDataViolationEvent emits data_violation_recorded for a non-failing
// violation, the data-quality twin of publishSchemaViolationEvent: the task did
// NOT fail, so nothing else records that a declared contract broke.
//
// Consumers today are the persisted event store (and therefore `caesium why`,
// the run timeline and any NotificationPolicy matching on the type) — NOT the
// leader-gated incident subscriber, whose classifierFailureTypes set is
// unchanged by this item. The plan's incident entry point for data quality is
// F1, and it keys off C1's `dataset_held`, not off this event. Best-effort; a
// nil store is a no-op.
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

// declaredNamespaces maps each declared dataset name to the namespace its
// registry row carries, so a persisted sample is written under the SAME
// namespace the baseline read will later query it by. Getting these two out of
// step would silently return zero samples for every namespaced dataset the day
// namespaces are populated. An undeclared name (an explicit marker selector)
// has no row and resolves to the empty namespace.
func declaredNamespaces(declarations []models.DatasetDeclaration) map[string]string {
	namespaces := make(map[string]string, len(declarations))
	for i := range declarations {
		namespaces[declarations[i].Name] = declarationNamespace(&declarations[i])
	}
	return namespaces
}

// declarationNamespace reads the nullable namespace off a registry row. Nil is
// the v1 state and reads as the empty string, which is what both DatasetMetric
// and Baseline use for an unnamespaced dataset.
func declarationNamespace(decl *models.DatasetDeclaration) string {
	if decl == nil || decl.Namespace == nil {
		return ""
	}
	return *decl.Namespace
}

package run

import (
	"context"
	"errors"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
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
// Phase 0 (this item) persists what the step self-reported and nothing else:
// every emitted metric becomes a DatasetMetric row, INCLUDING metrics no
// assertion names yet — that is free baseline history for an assertion added
// later. Stream B fills in baseline loading, evaluation and warn/fail dispatch
// behind this same signature, and Stream C adds the hold disposition, so
// neither has to re-touch the executors.
//
// The signature is instance-aware on purpose: a fanned step has N TaskRun rows
// per trigger and each must own its samples. See the fan-out semantics note on
// resolveSampleDataset.
//
// Errors: Phase 0 never fails a task. A metric is an observation, and losing an
// observation must not turn a successful run red — persistence problems are
// logged, mirroring how SaveSchemaViolations treats its own write. The error
// return exists because Stream B's `fail` disposition needs it and the
// executors already escalate it.
func EvaluateDataAssertions(store *Store, runID, taskID, taskRunID uuid.UUID, samples []pkgtask.DatasetMetricSample) error {
	if !DataAssertionsEnabled() || len(samples) == 0 {
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
	declared, err := producedDatasetNames(ctx, store.db, row.TaskID)
	if err != nil {
		log.Warn("failed to load declared datasets for metric attribution", "task_id", row.TaskID, "error", err)
		return nil
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
		return nil
	}

	if err := InsertDatasetMetrics(ctx, store.db, rows); err != nil {
		log.Warn("failed to persist dataset metrics", "run_id", runID, "task_id", taskID, "metrics", len(rows), "error", err)
		return nil
	}
	log.Info("recorded dataset metrics", "run_id", runID, "task_id", taskID, "task_run_id", row.ID, "metrics", len(rows))
	return nil
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
// counts partitions, not triggers. Evaluation in Stream B is likewise
// per-instance — the post-task pipeline has no group-completion seam, and C1's
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

// producedDatasetNames returns the dataset names the catalog task's step
// declares under datasets.produces, read off the declared registry rather than
// by re-parsing the stored job definition. The catalog task is read unscoped so
// a step retired between dispatch and completion still attributes its metrics.
func producedDatasetNames(ctx context.Context, conn *gorm.DB, taskID uuid.UUID) ([]string, error) {
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

	var names []string
	if err := conn.WithContext(ctx).
		Model(&models.DatasetDeclaration{}).
		Where("job_id = ? AND step_name = ? AND direction = ?", task.JobID, task.Name, models.DatasetDirectionProduces).
		Order("name ASC").
		Pluck("name", &names).Error; err != nil {
		return nil, err
	}
	return names, nil
}

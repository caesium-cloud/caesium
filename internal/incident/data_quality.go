package incident

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// The data-quality view of a data_quality_hold incident (data-circuit-breaker
// F2). What broke comes from the incident's evidence, never from the hold row:
// evidence was shaped once for the incident's namespace, with foreign cone nodes
// collapsed and unowned baselines withheld, while the row's cone is unredacted and
// its verdicts are the latest breach's, which can be another tenant's. Only the
// status and the occurrence count are read live, and those and the metric series
// are withheld once the job has moved out of the incident's namespace. Every live
// read shares a snapshot with the job row that gates it: a check made earlier
// would let a move and a repeat breach land in between.

const (
	// bundleMetricSeriesLimit bounds the samples per metric in a bundle.
	bundleMetricSeriesLimit = 30
	// ContextMetricSeriesLimit bounds the samples per metric in context/metrics.
	ContextMetricSeriesLimit = 200
	// metricSeriesCap bounds the metrics a series is read for.
	metricSeriesCap = 10
)

// afterJobNamespaceRead, when set (tests only), runs inside a snapshot right after
// the job row that gates its live reads: where a concurrent move interleaves.
var afterJobNamespaceRead func()

// ErrNoDatasetHold is returned for a metric series on an incident that has no
// dataset hold to scope it to.
var ErrNoDatasetHold = errors.New("incident: incident has no dataset hold")

// BundleDataQuality is the hold behind a data_quality_hold incident.
type BundleDataQuality struct {
	HoldID uuid.UUID `json:"hold_id"`
	// DatasetNamespace is the dataset's data axis, unrelated to the tenant
	// namespace the incident belongs to.
	DatasetNamespace string `json:"dataset_namespace"`
	Dataset          string `json:"dataset"`
	Reason           string `json:"reason,omitempty"`
	// Violations are the verdicts the hold opened on: the violated assertion, the
	// observed value, the bound, and the baseline snapshot it was judged against.
	Violations json.RawMessage `json:"violations,omitempty"`
	// Impact is the cone as redacted for the incident's namespace.
	Impact           json.RawMessage `json:"impact,omitempty"`
	ImpactedDatasets int             `json:"impacted_datasets"`
	// HoldStatus and OccurrenceCount are the hold's live state.
	HoldStatus      string `json:"hold_status,omitempty"`
	OccurrenceCount int    `json:"occurrence_count,omitempty"`
	// MetricSeries holds the last samples of each violated metric.
	MetricSeries []BundleMetricSeries `json:"metric_series,omitempty"`
	// NamespaceMoved reports that the job has moved out of the incident's
	// namespace: the live hold state and the series are withheld.
	NamespaceMoved bool `json:"namespace_moved,omitempty"`
}

// BundleMetricSeries is the recent history of one metric of the held dataset,
// oldest first.
type BundleMetricSeries struct {
	Metric  string               `json:"metric"`
	Samples []BundleMetricSample `json:"samples"`
}

// BundleMetricSample is one observation. Violated marks a sample the contract
// rejected, which the baseline excludes.
type BundleMetricSample struct {
	Value      float64   `json:"value"`
	Violated   bool      `json:"violated"`
	RunID      uuid.UUID `json:"run_id"`
	RecordedAt time.Time `json:"recorded_at"`
}

// storedHoldEvidence is the part of buildHoldEvidence's document read back.
type storedHoldEvidence struct {
	DatasetNamespace string          `json:"dataset_namespace"`
	Dataset          string          `json:"dataset"`
	Reason           string          `json:"reason"`
	ImpactedDatasets int             `json:"impacted_datasets"`
	Violations       json.RawMessage `json:"violations"`
	Impact           json.RawMessage `json:"impact"`
}

func decodeHoldEvidence(inc *models.Incident) storedHoldEvidence {
	var evidence storedHoldEvidence
	if len(inc.Evidence) == 0 {
		return evidence
	}
	if err := json.Unmarshal(inc.Evidence, &evidence); err != nil {
		log.Warn("incident: could not decode a hold incident's evidence", "incident_id", inc.ID, "error", err)
		return storedHoldEvidence{}
	}
	return evidence
}

// violatedMetrics lists the metrics the evidence's verdicts name, in order.
func (e storedHoldEvidence) violatedMetrics() []string {
	var verdicts []struct {
		Metric string `json:"metric"`
	}
	if len(e.Violations) == 0 || json.Unmarshal(e.Violations, &verdicts) != nil {
		return nil
	}
	metrics := make([]string, 0, len(verdicts))
	for i := range verdicts {
		if verdicts[i].Metric != "" && !slices.Contains(metrics, verdicts[i].Metric) {
			metrics = append(metrics, verdicts[i].Metric)
		}
	}
	if len(metrics) > metricSeriesCap {
		metrics = metrics[:metricSeriesCap]
	}
	return metrics
}

// loadDataQuality builds the bundle section. moved must come from a job row read
// through db, which must be the same snapshot. It returns nil for an incident
// whose dedupe key names no hold, which only a row written outside the subscriber
// has.
func loadDataQuality(ctx context.Context, db *gorm.DB, inc *models.Incident, moved bool) (*BundleDataQuality, error) {
	holdID, ok := holdIDFromDedupeKey(inc.DedupeKey)
	if !ok {
		return nil, nil
	}
	evidence := decodeHoldEvidence(inc)
	section := &BundleDataQuality{
		HoldID:           holdID,
		DatasetNamespace: evidence.DatasetNamespace,
		Dataset:          evidence.Dataset,
		Reason:           evidence.Reason,
		Violations:       evidence.Violations,
		Impact:           evidence.Impact,
		ImpactedDatasets: evidence.ImpactedDatasets,
		NamespaceMoved:   moved,
	}
	if moved {
		return section, nil
	}

	var hold models.DatasetHold
	err := db.WithContext(ctx).Select("id", "status", "occurrence_count").
		First(&hold, "id = ? AND held_by_job_id = ?", holdID, inc.JobID).Error
	switch {
	case err == nil:
		section.HoldStatus = hold.Status
		section.OccurrenceCount = hold.OccurrenceCount
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return nil, err
	}

	section.MetricSeries, err = metricSeries(ctx, db, inc, evidence, evidence.violatedMetrics(), bundleMetricSeriesLimit)
	if err != nil {
		return nil, err
	}
	return section, nil
}

// HoldMetricSeries returns the recent samples of one metric of the incident's
// held dataset, or of every violated metric when metric is empty. The dataset is
// the one in the incident's evidence: a caller can narrow the metric and can never
// name a dataset. Samples are those of runs recorded in the incident's namespace,
// which is a tenant axis and not the dataset's namespace.
func HoldMetricSeries(ctx context.Context, db *gorm.DB, inc *models.Incident, metric string, limit int) ([]BundleMetricSeries, error) {
	if FailureClass(inc.Class) != ClassDataQualityHold {
		return nil, ErrNoDatasetHold
	}
	evidence := decodeHoldEvidence(inc)
	if evidence.Dataset == "" {
		return nil, ErrNoDatasetHold
	}
	metrics := evidence.violatedMetrics()
	if metric != "" {
		metrics = []string{metric}
	}
	var series []BundleMetricSeries
	err := readSnapshot(ctx, db, func(tx *gorm.DB) error {
		var job models.Job
		err := tx.WithContext(ctx).Unscoped().Select("id", "namespace").First(&job, "id = ?", inc.JobID).Error
		switch {
		case err == nil:
			if movedOutOfIncident(inc, &job) {
				return ErrNamespaceMoved
			}
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return err
		}
		if afterJobNamespaceRead != nil {
			afterJobNamespaceRead()
		}
		series, err = metricSeries(ctx, tx, inc, evidence, metrics, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	return series, nil
}

func metricSeries(ctx context.Context, db *gorm.DB, inc *models.Incident, evidence storedHoldEvidence, metrics []string, limit int) ([]BundleMetricSeries, error) {
	if evidence.Dataset == "" {
		return nil, nil
	}
	scoped := HistoryContext(ctx, inc)
	out := make([]BundleMetricSeries, 0, len(metrics))
	for _, metric := range metrics {
		var rows []struct {
			Value     float64
			Violated  bool
			RunID     uuid.UUID
			CreatedAt time.Time
		}
		err := authz.NamespaceFilter(db.WithContext(scoped).
			Model(&models.DatasetMetric{}).
			Joins("JOIN task_runs ON task_runs.id = dataset_metrics.task_run_id").
			Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id"), "job_runs.namespace").
			Select("dataset_metrics.value, dataset_metrics.violated, task_runs.job_run_id AS run_id, dataset_metrics.created_at").
			Where("dataset_metrics.namespace = ? AND dataset_metrics.name = ? AND dataset_metrics.metric = ?",
				evidence.DatasetNamespace, evidence.Dataset, metric).
			Order("dataset_metrics.created_at DESC").Order("dataset_metrics.id DESC").
			Limit(limit).
			Find(&rows).Error
		if err != nil {
			return nil, err
		}
		samples := make([]BundleMetricSample, len(rows))
		for i := range rows {
			samples[len(rows)-1-i] = BundleMetricSample{
				Value: rows[i].Value, Violated: rows[i].Violated, RunID: rows[i].RunID, RecordedAt: rows[i].CreatedAt,
			}
		}
		out = append(out, BundleMetricSeries{Metric: metric, Samples: samples})
	}
	return out, nil
}

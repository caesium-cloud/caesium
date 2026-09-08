package run

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"gorm.io/gorm"
)

// DefaultBaselineWindow is the fallback sample window when neither the caller
// nor CAESIUM_BASELINE_WINDOW supplies one.
const DefaultBaselineWindow = 20

// BaselineStats summarises the recent clean history of one (dataset, metric).
// It is computed on read — there is no materialized baseline table, because the
// window is a handful of small rows per metric (design-data-circuit-breaker.md
// "Data model").
type BaselineStats struct {
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Metric    string    `json:"metric"`
	Samples   int       `json:"samples"`
	Median    float64   `json:"median"`
	P10       float64   `json:"p10"`
	P90       float64   `json:"p90"`
	Values    []float64 `json:"values,omitempty"`
	// AsOf echoes the cut the window was computed against, so a backtest
	// verdict can be reproduced from its recorded baseline.
	AsOf time.Time `json:"as_of"`
}

// Baseline computes the rolling baseline for one (namespace, name, metric) over
// the most recent `window` clean samples that PRECEDE asOf.
//
// The explicit asOf cut is the primary signature, not an afterthought: an
// assertion backtest (backtesting.md Stream F item F3) replays historical
// samples and must see the baseline "as it would have been at that run", with
// no later sample leaking in. The live evaluator simply passes time.Now().
//
// It takes a *gorm.DB rather than a *Store and touches no run state, so it is
// callable with no executor and no live run.
//
// "Clean" today means the emitting task run neither was replay-quarantined nor
// ended in anything but success, AND the sample itself did not break the
// contract it was judged against (models.DatasetMetric.Violated) — a what-if, a
// failed attempt or a rejected value must not move a baseline. The design's
// remaining predicate, "non-held", joins in with the DatasetHold model (Stream
// C1), which does not exist yet; cleanSampleQuery is the single seam where that
// filter lands.
func Baseline(ctx context.Context, conn *gorm.DB, namespace, name, metric string, window int, asOf time.Time) (*BaselineStats, error) {
	stats := &BaselineStats{
		Namespace: namespace,
		Name:      name,
		Metric:    metric,
		AsOf:      asOf.UTC(),
	}
	if conn == nil || name == "" || metric == "" {
		return stats, nil
	}
	if window <= 0 {
		window = DefaultBaselineWindow
	}

	var rows []models.DatasetMetric
	err := cleanSampleQuery(ctx, conn, namespace, name, metric, asOf).
		Order("dataset_metrics.created_at DESC").
		Limit(window).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return stats, nil
	}

	// Rows arrive newest-first (that is what "the last N" means); the caller
	// sees them oldest-first, which is the order a sparkline renders.
	values := make([]float64, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		values = append(values, rows[i].Value)
	}

	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	stats.Samples = len(sorted)
	stats.Median = percentile(sorted, 0.5)
	stats.P10 = percentile(sorted, 0.1)
	stats.P90 = percentile(sorted, 0.9)
	stats.Values = values
	return stats, nil
}

// BaselineWindow is the configured sample window, falling back to
// DefaultBaselineWindow when the environment carries a nonsensical value.
func BaselineWindow() int {
	if window := env.Variables().BaselineWindow; window > 0 {
		return window
	}
	return DefaultBaselineWindow
}

// cleanSampleQuery scopes DatasetMetric rows to the clean history of one
// (namespace, name, metric) strictly before asOf.
//
// "Clean" is four predicates, all of them load-bearing:
//
//   - non-quarantined and succeeded — a what-if replay and a failed attempt
//     never describe the dataset that exists;
//   - non-violated — the value an ENFORCED assertion rejected is kept (Plan 3
//     replays it, `caesium why` shows it) but must never become the normal it
//     next gets compared against, or three anomalies in a row would move the
//     median far enough to silence the assertion mid-incident;
//   - NOT HELD — a sample observed while the dataset was inside an active
//     DatasetHold window. The design's third predicate, added with the breaker
//     itself. While a dataset is held it is by declaration known-bad, so its
//     observations are not "normal" even when a particular metric happened to
//     satisfy its own bound; letting a hold window drift the baseline is how a
//     dataset comes back from an incident with its assertions re-centred on the
//     incident.
//
// The releasing run is exempted (release_run_id): a clean-run release records
// its samples and closes the hold in ONE transaction, so those samples are
// technically inside the window while being exactly the evidence that the
// dataset recovered. Under `release: manual` there is no exempt run, so every
// sample taken during the hold stays out of the baseline until a human acks —
// deliberate, and the starvation fallback B1 documented applies: with no clean
// history a deltaFromBaseline assertion yields no verdict rather than a
// fabricated one, while the absolute bound that opened the hold keeps firing.
func cleanSampleQuery(ctx context.Context, conn *gorm.DB, namespace, name, metric string, asOf time.Time) *gorm.DB {
	return conn.WithContext(ctx).
		Model(&models.DatasetMetric{}).
		Joins("JOIN task_runs ON task_runs.id = dataset_metrics.task_run_id").
		Where("dataset_metrics.namespace = ?", namespace).
		Where("dataset_metrics.name = ?", name).
		Where("dataset_metrics.metric = ?", metric).
		Where("dataset_metrics.created_at < ?", asOf.UTC()).
		Where("dataset_metrics.violated = ?", false).
		Where("task_runs.quarantine = ?", false).
		Where("task_runs.status = ?", string(TaskStatusSucceeded)).
		Where(`NOT EXISTS (
	SELECT 1 FROM dataset_holds
	WHERE dataset_holds.namespace = dataset_metrics.namespace
		AND dataset_holds.name = dataset_metrics.name
		AND dataset_holds.opened_at <= dataset_metrics.created_at
		AND (dataset_holds.released_at IS NULL OR dataset_holds.released_at > dataset_metrics.created_at)
		AND (dataset_holds.release_run_id IS NULL OR dataset_holds.release_run_id <> task_runs.job_run_id)
)`)
}

// percentile returns the linearly-interpolated percentile of an ascending
// slice. Interpolation (rather than nearest-rank) keeps p10/p90 meaningful on
// the small windows this feature uses — with 20 samples, nearest-rank p10 and
// p90 would both quantise onto single observations.
func percentile(sorted []float64, p float64) float64 {
	switch len(sorted) {
	case 0:
		return 0
	case 1:
		return sorted[0]
	}
	pos := p * float64(len(sorted)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if lower == upper {
		return sorted[lower]
	}
	weight := pos - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}

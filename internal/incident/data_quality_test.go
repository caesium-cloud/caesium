package incident

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// metricSample records one rowCount sample of dataset from a new run in namespace.
func (w *holdWorld) metricSample(namespace, dataset, metric string, value float64, violated bool, at time.Time) uuid.UUID {
	w.t.Helper()
	runID, taskRunID := w.greenRun(namespace)
	require.NoError(w.t, w.db.Create(&models.DatasetMetric{
		ID: uuid.New(), TaskRunID: taskRunID, Name: dataset, Metric: metric, Value: value, Violated: violated, CreatedAt: at,
	}).Error)
	return runID
}

func bundleOf(t *testing.T, w *holdWorld, id uuid.UUID, reader ReaderConfinement) *Bundle {
	t.Helper()
	b, err := BuildBundleFor(context.Background(), w.db, id, nil, reader)
	require.NoError(t, err)
	return b
}

func TestBundleDataQualitySection(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, storedCone(t, coneNode("finance", "own.rollup")))
	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", id).Update("occurrence_count", 4).Error)

	base := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		w.metricSample("finance", releaseDataset, "rowCount", 5000+float64(i), false, base.Add(time.Duration(i)*time.Minute))
	}
	breach := w.metricSample("finance", releaseDataset, "rowCount", 12, true, base.Add(10*time.Minute))
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))
	inc := w.only()

	for _, reader := range []ReaderConfinement{ReaderUnrestricted, ReaderConfined} {
		b := bundleOf(t, w, inc.ID, reader)
		section := b.DataQuality
		require.NotNil(t, section)
		require.Equal(t, id, section.HoldID)
		require.Equal(t, releaseDataset, section.Dataset)
		require.Equal(t, "", section.DatasetNamespace)
		require.Equal(t, "min", section.Reason)
		require.Equal(t, models.DatasetHoldStatusActive, section.HoldStatus)
		require.Equal(t, 4, section.OccurrenceCount)
		require.False(t, section.NamespaceMoved)
		require.Contains(t, string(section.Impact), "own.rollup")

		var verdicts []struct {
			Assertion       string  `json:"assertion"`
			Observed        float64 `json:"observed"`
			Bound           float64 `json:"bound"`
			BaselineMedian  float64 `json:"baseline_median"`
			BaselineSamples int     `json:"baseline_samples"`
		}
		require.NoError(t, json.Unmarshal(section.Violations, &verdicts))
		require.Len(t, verdicts, 1)
		require.Equal(t, "min", verdicts[0].Assertion)
		require.InDelta(t, 12, verdicts[0].Observed, 0.001)
		require.InDelta(t, 1000, verdicts[0].Bound, 0.001)
		require.InDelta(t, 5000, verdicts[0].BaselineMedian, 0.001)
		require.Equal(t, 9, verdicts[0].BaselineSamples)

		require.Len(t, section.MetricSeries, 1)
		series := section.MetricSeries[0]
		require.Equal(t, "rowCount", series.Metric)
		require.Len(t, series.Samples, 4)
		require.InDelta(t, 5000, series.Samples[0].Value, 0.001, "oldest first")
		last := series.Samples[3]
		require.InDelta(t, 12, last.Value, 0.001)
		require.True(t, last.Violated)
		require.Equal(t, breach, last.RunID)

		// The failure semantics are untouched: the task succeeded.
		require.Empty(t, b.Failure.LogTail)
		require.Empty(t, b.Failure.Result)
		require.Nil(t, b.Failure.ExitCode)
		require.NotEmpty(t, b.Failure.Error)
	}
}

// The hold row holds an unredacted cone and the latest breach's verdicts, which can
// be another tenant's. Neither may reach the section.
func TestBundleDataQualityReadsEvidenceNotTheHoldRow(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, storedCone(t, coneNode("finance", "own.rollup"), coneNode("marketing", "secret.rollup")))
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))
	inc := w.only()

	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", id).
		Update("violations", datatypes.JSON(`[{"metric":"marketingRevenue","assertion":"min","observed":424242}]`)).Error)

	raw, err := json.Marshal(bundleOf(t, w, inc.ID, ReaderUnrestricted).DataQuality)
	require.NoError(t, err)
	require.Contains(t, string(raw), "own.rollup")
	require.NotContains(t, string(raw), "secret.rollup")
	require.NotContains(t, string(raw), "424242")
	require.NotContains(t, string(raw), "marketingRevenue")
}

func TestBundleDataQualitySeriesStaysInTheIncidentsNamespaceAndDataset(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, nil)
	now := time.Now().UTC().Add(-time.Hour)
	own := w.metricSample("finance", releaseDataset, "rowCount", 1, false, now)
	w.metricSample("marketing", releaseDataset, "rowCount", 2, false, now.Add(time.Minute))
	w.metricSample("finance", "another.dataset", "rowCount", 3, false, now.Add(2*time.Minute))
	w.metricSample("finance", releaseDataset, "nullRate", 4, false, now.Add(3*time.Minute))
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))
	inc := w.only()

	series, err := HoldMetricSeries(ctx, w.db, &inc, "", ContextMetricSeriesLimit)
	require.NoError(t, err)
	require.Len(t, series, 1, "with no metric named, the violated ones")
	require.Len(t, series[0].Samples, 1)
	require.Equal(t, own, series[0].Samples[0].RunID)

	series, err = HoldMetricSeries(ctx, w.db, &inc, "nullRate", ContextMetricSeriesLimit)
	require.NoError(t, err)
	require.Len(t, series, 1)
	require.Len(t, series[0].Samples, 1)
	require.InDelta(t, 4, series[0].Samples[0].Value, 0.001)

	series, err = HoldMetricSeries(ctx, w.db, &inc, "absent", ContextMetricSeriesLimit)
	require.NoError(t, err)
	require.Empty(t, series[0].Samples)
}

func TestBundleDataQualitySeriesIsBounded(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, nil)
	base := time.Now().UTC().Add(-time.Hour)
	for i := range bundleMetricSeriesLimit + 5 {
		w.metricSample("finance", releaseDataset, "rowCount", float64(i), false, base.Add(time.Duration(i)*time.Second))
	}
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))

	samples := bundleOf(t, w, w.only().ID, ReaderConfined).DataQuality.MetricSeries[0].Samples
	require.Len(t, samples, bundleMetricSeriesLimit)
	require.InDelta(t, float64(bundleMetricSeriesLimit+4), samples[len(samples)-1].Value, 0.001, "the newest samples survive")
}

func TestViolatedMetricsAreDedupedAndCapped(t *testing.T) {
	verdicts := make([]map[string]string, 0, metricSeriesCap+3)
	verdicts = append(verdicts, map[string]string{"metric": "a"}, map[string]string{"metric": "a"}, map[string]string{"metric": ""})
	for i := range metricSeriesCap + 3 {
		verdicts = append(verdicts, map[string]string{"metric": string(rune('b' + i))})
	}
	raw, err := json.Marshal(verdicts)
	require.NoError(t, err)
	metrics := storedHoldEvidence{Violations: raw}.violatedMetrics()
	require.Len(t, metrics, metricSeriesCap)
	require.Equal(t, "a", metrics[0])
	require.Nil(t, storedHoldEvidence{Violations: json.RawMessage(`{`)}.violatedMetrics())
}

// A job that moved out takes the live hold state and the series with it, as it
// takes its DAG; what the incident recorded at open stays.
func TestBundleDataQualityWithheldWhenTheJobMoved(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, nil)
	w.metricSample("finance", releaseDataset, "rowCount", 1, false, time.Now().UTC().Add(-time.Hour))
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))
	inc := w.only()
	require.NoError(t, w.db.Model(&models.Job{}).Where("id = ?", w.jobID).Update("namespace", "marketing").Error)

	b := bundleOf(t, w, inc.ID, ReaderConfined)
	require.True(t, b.Job.NamespaceMoved)
	section := b.DataQuality
	require.True(t, section.NamespaceMoved)
	require.Empty(t, section.MetricSeries)
	require.Empty(t, section.HoldStatus)
	require.Zero(t, section.OccurrenceCount)
	require.Equal(t, releaseDataset, section.Dataset)
	require.NotEmpty(t, section.Violations)

	_, err := HoldMetricSeries(ctx, w.db, &inc, "", ContextMetricSeriesLimit)
	require.ErrorIs(t, err, ErrNamespaceMoved)
}

// openConcurrentDB is a file database in WAL mode with more than one connection,
// so a write can commit while a read snapshot is open, which the shared in-memory
// test database cannot do.
func openConcurrentDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "catalog.db") + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { testutil.CloseDB(db) })
	require.NoError(t, db.AutoMigrate(models.All...))
	return db
}

func interleaveAfterJobNamespaceRead(t *testing.T, fn func()) {
	t.Helper()
	fired := false
	afterJobNamespaceRead = func() {
		if !fired {
			fired = true
			fn()
		}
	}
	t.Cleanup(func() { afterJobNamespaceRead = nil })
}

// A repeat breach keeps the hold's job, so only the namespace in the same snapshot
// can tell the new owner's occurrence from the incident's. Here the job moves and
// breaches again after the bundle read the job row: the bundle must describe the
// world that row belongs to, not the new owner's hold.
func TestBundleDataQualityLiveStateSharesTheJobSnapshot(t *testing.T) {
	w := newHoldWorldOn(t, openConcurrentDB(t), "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, nil)
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))
	inc := w.only()

	interleaveAfterJobNamespaceRead(t, func() {
		require.NoError(t, w.db.Model(&models.Job{}).Where("id = ?", w.jobID).Update("namespace", "marketing").Error)
		require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", id).Update("occurrence_count", 5).Error)
	})

	b := bundleOf(t, w, inc.ID, ReaderConfined)
	require.False(t, b.Job.NamespaceMoved, "the snapshot predates the move")
	require.False(t, b.DataQuality.NamespaceMoved, "the section and the job agree")
	require.Equal(t, 1, b.DataQuality.OccurrenceCount, "the new owner's breach must not reach the old namespace")

	again := bundleOf(t, w, inc.ID, ReaderConfined)
	require.True(t, again.Job.NamespaceMoved)
	require.True(t, again.DataQuality.NamespaceMoved)
	require.Zero(t, again.DataQuality.OccurrenceCount)
}

// The series reads inside the snapshot of its gate too. Its rows are filtered by
// run namespace, so no foreign sample could reach it; this pins the agreement.
func TestHoldMetricSeriesSharesTheJobSnapshot(t *testing.T) {
	w := newHoldWorldOn(t, openConcurrentDB(t), "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, nil)
	base := time.Now().UTC().Add(-time.Hour)
	w.metricSample("finance", releaseDataset, "rowCount", 1, false, base)
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))
	inc := w.only()

	interleaveAfterJobNamespaceRead(t, func() {
		require.NoError(t, w.db.Model(&models.Job{}).Where("id = ?", w.jobID).Update("namespace", "marketing").Error)
		w.metricSample("finance", releaseDataset, "rowCount", 2, false, base.Add(time.Minute))
	})

	series, err := HoldMetricSeries(ctx, w.db, &inc, "", ContextMetricSeriesLimit)
	require.NoError(t, err)
	require.Len(t, series[0].Samples, 1, "nothing committed after the gate's snapshot")

	_, err = HoldMetricSeries(ctx, w.db, &inc, "", ContextMetricSeriesLimit)
	require.ErrorIs(t, err, ErrNamespaceMoved)
}

// A soft-deleted job moved nowhere: the agent keeps its own history.
func TestBundleDataQualityForASoftDeletedJob(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, nil)
	w.metricSample("finance", releaseDataset, "rowCount", 1, false, time.Now().UTC().Add(-time.Hour))
	w.sub.handle(ctx, w.heldEvent(id, releaseDataset))
	inc := w.only()
	require.NoError(t, w.db.Where("id = ?", w.jobID).Delete(&models.Job{}).Error)

	b := bundleOf(t, w, inc.ID, ReaderConfined)
	require.Empty(t, b.Job.Tasks)
	require.False(t, b.DataQuality.NamespaceMoved)
	require.Len(t, b.DataQuality.MetricSeries[0].Samples, 1)

	series, err := HoldMetricSeries(ctx, w.db, &inc, "", ContextMetricSeriesLimit)
	require.NoError(t, err)
	require.Len(t, series[0].Samples, 1)
}

// An incident the sweep opened holds no verdicts when the row's latest breach is
// not attributable to it, and a run-less one has no run: the section still names
// the hold, and the agent still has a target.
func TestBundleDataQualityWithoutVerdicts(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	id := w.hold(releaseDataset, nil)
	held := holdEvent{HoldID: id, Dataset: releaseDataset, Reason: "min"}
	w.sub.openHoldIncident(ctx, w.jobID, uuid.Nil, failureContext{jobID: w.jobID, namespace: "finance"}, held, nil, time.Now().UTC())
	inc := w.only()
	require.Nil(t, inc.RunID)

	b := bundleOf(t, w, inc.ID, ReaderConfined)
	require.NotNil(t, b.DataQuality)
	require.Equal(t, id, b.DataQuality.HoldID)
	require.Empty(t, b.DataQuality.Violations)
	require.Empty(t, b.DataQuality.MetricSeries)
}

func TestBundleDataQualityAbsentForOtherClasses(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := context.Background()
	inc, _, err := NewStore(w.db).OpenOrAppend(ctx, OpenParams{Namespace: "finance", JobID: w.jobID, TaskName: "load", Class: ClassTransientInfra})
	require.NoError(t, err)

	require.Nil(t, bundleOf(t, w, inc.ID, ReaderConfined).DataQuality)
	_, err = HoldMetricSeries(ctx, w.db, inc, "", ContextMetricSeriesLimit)
	require.ErrorIs(t, err, ErrNoDatasetHold)
}

func TestHoldMetricSeriesNeedsEvidence(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	id := w.hold(releaseDataset, nil)
	w.sub.handle(context.Background(), w.heldEvent(id, releaseDataset))
	inc := w.only()
	inc.Evidence = datatypes.JSON(`{`)
	_, err := HoldMetricSeries(context.Background(), w.db, &inc, "rowCount", ContextMetricSeriesLimit)
	require.ErrorIs(t, err, ErrNoDatasetHold)
}

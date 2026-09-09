package dataset

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestDatasetHoldFieldsAreFlagGatedAndPreserveFreshness(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	now := time.Now().UTC()
	state := models.DatasetState{Namespace: "", Name: "warehouse/orders", Status: models.DatasetStatusFresh, Watermark: "2026-09-08", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, conn.Create(&state).Error)
	hold := models.DatasetHold{ID: uuid.New(), Name: state.Name, Status: models.DatasetHoldStatusActive, Reason: "min", OccurrenceCount: 3,
		HeldByJobID: uuid.New(), OpenedAt: now, CreatedAt: now, UpdatedAt: now,
		Violations: datatypes.JSON(`[{"assertion":"min","observed":12}]`), Impact: datatypes.JSON(`{"downstream":[{"name":"reports/orders"}]}`)}
	require.NoError(t, conn.Create(&hold).Error)
	var holdQueries []string
	require.NoError(t, conn.Callback().Query().After("gorm:query").Register("test:capture_hold_query", func(tx *gorm.DB) {
		if tx.Statement.Table == "dataset_holds" {
			holdQueries = append(holdQueries, tx.Statement.SQL.String())
		}
	}))

	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() { t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false"); require.NoError(t, env.Process()) })
	list, err := svc.List(ListParams{Status: models.DatasetStatusFresh})
	require.NoError(t, err)
	require.Len(t, list.Datasets, 1)
	require.Equal(t, models.DatasetStatusFresh, list.Datasets[0].Status)
	require.Equal(t, hold.ID, list.Datasets[0].Hold.ID)
	require.Len(t, holdQueries, 1)
	require.Contains(t, holdQueries[0], "SELECT `namespace`,`name`,`id`,`status`,`reason`,`opened_at`,`occurrence_count` FROM `dataset_holds`")
	encoded, err := json.Marshal(list.Datasets[0].Hold)
	require.NoError(t, err)
	var summary map[string]any
	require.NoError(t, json.Unmarshal(encoded, &summary))
	require.Len(t, summary, 5)
	require.Equal(t, "active", summary["status"])
	require.Equal(t, "min", summary["reason"])
	require.EqualValues(t, 3, summary["occurrence_count"])
	detail, err := svc.Get("", state.Name)
	require.NoError(t, err)
	require.Equal(t, "active", detail.HoldStatus)
	require.Equal(t, hold.Violations, detail.Hold.Violations)
	require.Equal(t, hold.Impact, detail.Hold.Impact)
	feed, err := svc.Holds(HoldsParams{})
	require.NoError(t, err)
	require.Len(t, feed.Holds, 1)
	require.Equal(t, hold.Violations, feed.Holds[0].Violations)
	require.Equal(t, hold.Impact, feed.Holds[0].Impact)

	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
	require.NoError(t, env.Process())
	// An off deployment must never query holds, including one whose catalog
	// predates this feature. Dropping the table makes a stray query fail.
	require.NoError(t, conn.Migrator().DropTable(&models.DatasetHold{}))
	list, err = svc.List(ListParams{})
	require.NoError(t, err)
	require.Len(t, list.Datasets, 1)
	require.Nil(t, list.Datasets[0].Hold)
	detail, err = svc.Get("", state.Name)
	require.NoError(t, err)
	for _, result := range []any{list, detail} {
		body, err := json.Marshal(result)
		require.NoError(t, err)
		require.NotContains(t, string(body), `"hold"`)
		require.NotContains(t, string(body), `"hold_status"`)
	}
}

func TestDatasetDetailCanOmitHoldQueryWhileAssertionsEnabled(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	now := time.Now().UTC()
	state := models.DatasetState{Name: "warehouse/orders", Status: models.DatasetStatusFresh, Watermark: "2026-09-08", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, conn.Create(&state).Error)
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	// The opt-out must suppress the query itself, not just remove its JSON.
	require.NoError(t, conn.Migrator().DropTable(&models.DatasetHold{}))
	detail, err := svc.GetWithOptions("", state.Name, GetOptions{IncludeHold: false})
	require.NoError(t, err)
	require.Equal(t, state.Name, detail.State.Name)
	require.Equal(t, state.Watermark, detail.State.Watermark)
	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"hold"`)
	require.NotContains(t, string(encoded), `"hold_status"`)
	_, err = svc.Get("", state.Name)
	require.Error(t, err, "the default detail path still reads the full active hold")
}

func TestMetricsPagesRawHistoryAndMarksActualBaselineMembership(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	t.Cleanup(func() { require.NoError(t, env.Process()) })
	t.Setenv("CAESIUM_BASELINE_WINDOW", "2")
	t.Setenv("CAESIUM_BASELINE_MIN_SAMPLES", "3")
	require.NoError(t, env.Process())
	cleanRun := seedMetricTaskRun(t, conn, "succeeded", false)
	failedRun := seedMetricTaskRun(t, conn, "failed", false)
	quarantinedRun := seedMetricTaskRun(t, conn, "succeeded", true)
	base := time.Now().UTC().Add(-time.Hour)
	rows := []models.DatasetMetric{
		{TaskRunID: cleanRun, Value: 10, CreatedAt: base},
		{TaskRunID: cleanRun, Value: 20, CreatedAt: base.Add(time.Minute)},
		{TaskRunID: cleanRun, Value: 30, CreatedAt: base.Add(time.Minute)},
		{TaskRunID: cleanRun, Value: 40, CreatedAt: base.Add(time.Minute)},
		{TaskRunID: failedRun, Value: 50, CreatedAt: base.Add(2 * time.Minute)},
		{TaskRunID: quarantinedRun, Value: 60, CreatedAt: base.Add(3 * time.Minute)},
		{TaskRunID: cleanRun, Value: 70, Violated: true, CreatedAt: base.Add(4 * time.Minute)},
		{TaskRunID: cleanRun, Value: 80, CreatedAt: base.Add(6 * time.Minute)},
	}
	for i := range rows {
		rows[i].ID = uuid.MustParse(fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1))
		rows[i].Name, rows[i].Metric = "warehouse/orders", "dedup_ratio"
	}
	require.NoError(t, conn.Create(&rows).Error)
	hold := models.DatasetHold{ID: uuid.New(), Name: "warehouse/orders", Status: "active", HeldByJobID: uuid.New(),
		OpenedAt: base.Add(5 * time.Minute), CreatedAt: base, UpdatedAt: base}
	require.NoError(t, conn.Create(&hold).Error)
	// These observations must not enter the selected identity/metric or as-of cut.
	for _, row := range []models.DatasetMetric{
		{ID: uuid.New(), TaskRunID: cleanRun, Name: "warehouse/orders", Metric: "dedup_ratio", CreatedAt: time.Now().UTC().Add(time.Hour)},
		{ID: uuid.New(), TaskRunID: cleanRun, Namespace: "tenant", Name: "warehouse/orders", Metric: "dedup_ratio", CreatedAt: base},
		{ID: uuid.New(), TaskRunID: cleanRun, Name: "warehouse/orders", Metric: "rowCount", CreatedAt: base},
	} {
		require.NoError(t, conn.Create(&row).Error)
	}

	all, err := svc.Metrics("", "warehouse/orders", "dedup_ratio", MetricsParams{})
	require.NoError(t, err)
	require.EqualValues(t, 8, all.Total)
	require.Equal(t, 50, all.Limit)
	require.Zero(t, all.Offset)
	require.Len(t, all.Series, 8)
	require.Equal(t, []float64{30, 40}, all.Baseline.Values)
	require.Equal(t, 2, all.Baseline.Samples)
	require.Equal(t, 2, all.Window)
	require.Equal(t, 3, all.MinSamples)
	require.True(t, all.Seeding)
	for i, sample := range all.Series {
		require.Equal(t, rows[i].ID, sample.ID, "raw history uses timestamp then ID order")
		require.Equal(t, i == 2 || i == 3, sample.InBaseline, "only the selected clean window belongs to this baseline: sample %d", i)
	}
	// Failed, quarantined and held observations are visibly outside the baseline
	// even though none broke this particular metric's assertion.
	for _, i := range []int{4, 5, 7} {
		require.False(t, all.Series[i].Violated)
		require.False(t, all.Series[i].InBaseline)
	}

	for _, page := range []struct {
		offset int
		ids    []uuid.UUID
	}{
		{2, []uuid.UUID{rows[3].ID, rows[4].ID, rows[5].ID}},
		{5, []uuid.UUID{rows[0].ID, rows[1].ID, rows[2].ID}},
	} {
		result, err := svc.Metrics("", "warehouse/orders", "dedup_ratio", MetricsParams{Limit: 3, Offset: page.offset})
		require.NoError(t, err)
		require.EqualValues(t, 8, result.Total)
		require.Equal(t, 3, result.Limit)
		require.Equal(t, page.offset, result.Offset)
		require.Equal(t, all.Baseline.Values, result.Baseline.Values, "raw pagination never pages the clean baseline")
		require.Len(t, result.Series, len(page.ids))
		for i, sample := range result.Series {
			require.Equal(t, page.ids[i], sample.ID)
			require.Equal(t, sample.ID == rows[2].ID || sample.ID == rows[3].ID, sample.InBaseline)
		}
	}
}

func TestMetricsPaginationDefaultsAndBounds(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	taskRunID := seedMetricTaskRun(t, conn, "succeeded", false)
	base := time.Now().UTC().Add(-time.Hour)
	rows := make([]models.DatasetMetric, 205)
	for i := range rows {
		rows[i] = models.DatasetMetric{ID: uuid.New(), TaskRunID: taskRunID, Name: "orders", Metric: "rowCount", Value: float64(i), CreatedAt: base.Add(time.Duration(i) * time.Second)}
	}
	require.NoError(t, conn.Create(&rows).Error)
	first, err := svc.Metrics("", "orders", "rowCount", MetricsParams{})
	require.NoError(t, err)
	require.EqualValues(t, 205, first.Total)
	require.Equal(t, 50, first.Limit)
	require.Len(t, first.Series, 50)
	require.EqualValues(t, 155, first.Series[0].Value)
	require.EqualValues(t, 204, first.Series[49].Value)
	capped, err := svc.Metrics("", "orders", "rowCount", MetricsParams{Limit: 500, Offset: -2})
	require.NoError(t, err)
	require.Equal(t, 200, capped.Limit)
	require.Zero(t, capped.Offset)
	require.Len(t, capped.Series, 200)
	require.EqualValues(t, 5, capped.Series[0].Value)
	last, err := svc.Metrics("", "orders", "rowCount", MetricsParams{Limit: 50, Offset: 200})
	require.NoError(t, err)
	require.EqualValues(t, 205, last.Total)
	require.Len(t, last.Series, 5)
	require.EqualValues(t, 0, last.Series[0].Value)
	require.EqualValues(t, 4, last.Series[4].Value)
	empty, err := svc.Metrics("", "orders", "rowCount", MetricsParams{Offset: 205})
	require.NoError(t, err)
	require.EqualValues(t, 205, empty.Total)
	require.NotNil(t, empty.Series)
	require.Empty(t, empty.Series)
}

func seedMetricTaskRun(t *testing.T, conn *gorm.DB, status string, quarantine bool) uuid.UUID {
	t.Helper()
	trigger := models.Trigger{ID: uuid.New(), Type: models.TriggerTypeCron}
	require.NoError(t, conn.Create(&trigger).Error)
	job := models.Job{ID: uuid.New(), Alias: "metrics-" + uuid.NewString(), TriggerID: trigger.ID}
	require.NoError(t, conn.Create(&job).Error)
	atom := models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23"}
	require.NoError(t, conn.Create(&atom).Error)
	task := models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "load"}
	require.NoError(t, conn.Create(&task).Error)
	jobRun := models.JobRun{ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, Status: status, StartedAt: time.Now().UTC()}
	require.NoError(t, conn.Create(&jobRun).Error)
	taskRun := models.TaskRun{ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atom.ID, Engine: atom.Engine,
		Image: atom.Image, Command: "[]", Status: status, Quarantine: quarantine}
	require.NoError(t, conn.Create(&taskRun).Error)
	return taskRun.ID
}

func TestHoldsFiltersExactIdentityAndPaginate(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	svc := &Service{ctx: context.Background(), db: conn}
	now := time.Now().UTC()
	for i, row := range []models.DatasetHold{
		{Namespace: "", Name: "warehouse/orders", Status: "active"},
		{Namespace: "warehouse", Name: "orders", Status: "active"},
		{Namespace: "other", Name: "warehouse/orders", Status: "active"},
		{Namespace: "", Name: "warehouse/orders", Status: "released"},
	} {
		row.ID, row.HeldByJobID = uuid.New(), uuid.New()
		row.OpenedAt = now.Add(time.Duration(i) * time.Second)
		row.CreatedAt, row.UpdatedAt = row.OpenedAt, row.OpenedAt
		require.NoError(t, conn.Create(&row).Error)
	}
	namespace := ""
	result, err := svc.Holds(HoldsParams{Namespace: &namespace, Name: "warehouse/orders"})
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Total)
	require.Len(t, result.Holds, 1)
	require.Empty(t, result.Holds[0].Namespace)
	require.Equal(t, "warehouse/orders", result.Holds[0].Name)
	result, err = svc.Holds(HoldsParams{Status: "all", Limit: 1, Offset: 1})
	require.NoError(t, err)
	require.EqualValues(t, 4, result.Total)
	require.Len(t, result.Holds, 1)
	require.Equal(t, "other", result.Holds[0].Namespace)
	_, err = svc.Holds(HoldsParams{Status: "bogus"})
	require.ErrorIs(t, err, ErrHoldStatus)
}

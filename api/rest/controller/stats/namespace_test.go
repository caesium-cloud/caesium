package stats

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	svc "github.com/caesium-cloud/caesium/api/rest/service/stats"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestStatsCollectionGuard serves the real GET /v1/stats and
// /v1/stats/summary handlers as a confined principal: every total, rate, top
// list and trend point is computed over its namespaces only, so no other
// namespace's job, alias or run count leaks through an aggregate (B4).
func TestStatsCollectionGuard(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	prev := statsService
	statsService = func(ctx context.Context) *svc.Service { return svc.NewWithDatabase(ctx, db) }
	t.Cleanup(func() { statsService = prev })

	mktJob := seedStatsJob(t, db, "stats-mkt", "marketing", []string{"succeeded", "failed"})
	finJob := seedStatsJob(t, db, "stats-fin-secret", "finance", []string{"failed", "failed", "failed"})

	type statsBody struct {
		Jobs struct {
			Total       int64   `json:"total"`
			RecentRuns  int64   `json:"recent_runs"`
			SuccessRate float64 `json:"success_rate"`
		} `json:"jobs"`
		TopFailing []struct {
			JobID string `json:"job_id"`
		} `json:"top_failing"`
		TopFailingAtoms []struct {
			JobID string `json:"job_id"`
		} `json:"top_failing_atoms"`
		SlowestJobs []struct {
			JobID string `json:"job_id"`
		} `json:"slowest_jobs"`
		Trend []struct {
			RunCount int64 `json:"run_count"`
		} `json:"success_rate_trend"`
	}
	for _, route := range []struct {
		path, target string
		handler      echo.HandlerFunc
	}{
		{"/v1/stats", "/v1/stats", Get},
		{"/v1/stats/summary", "/v1/stats/summary?window=24h", Summary},
	} {
		read := func(key string) (statsBody, string) {
			rec := serveCollectionGuarded(t, db, key, route.path, route.target, route.handler)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var body statsBody
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			return body, rec.Body.String()
		}
		trendRuns := func(body statsBody) (n int64) {
			for _, point := range body.Trend {
				n += point.RunCount
			}
			return n
		}

		confined, raw := read(guardViewerKey(t, db, `["marketing"]`))
		require.EqualValues(t, 1, confined.Jobs.Total, route.path)
		require.EqualValues(t, 2, confined.Jobs.RecentRuns, route.path)
		require.InDelta(t, 0.5, confined.Jobs.SuccessRate, 1e-9, route.path)
		require.EqualValues(t, 2, trendRuns(confined), route.path)
		for _, list := range [][]struct {
			JobID string `json:"job_id"`
		}{confined.TopFailing, confined.TopFailingAtoms, confined.SlowestJobs} {
			for _, entry := range list {
				require.Equal(t, mktJob.String(), entry.JobID, route.path)
			}
		}
		require.NotContains(t, raw, finJob.String(), route.path)
		require.NotContains(t, raw, "stats-fin-secret", route.path)

		cluster, raw := read(guardViewerKey(t, db, ""))
		require.EqualValues(t, 2, cluster.Jobs.Total, route.path)
		require.EqualValues(t, 5, trendRuns(cluster), route.path)
		require.Contains(t, raw, "stats-fin-secret", "an unrestricted principal sees every job")
	}
}

// seedStatsJob creates a job in namespace with one completed run per status
// (each failed run with a failed task run), all started within the last hour.
func seedStatsJob(t *testing.T, db *gorm.DB, alias, namespace string, statuses []string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	job := &models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: uuid.New(), Name: alias + "-task", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(task).Error)
	for i, status := range statuses {
		started := now.Add(-time.Duration(i+1) * time.Minute)
		completed := started.Add(time.Duration(i+1) * time.Second)
		run := &models.JobRun{ID: uuid.New(), JobID: job.ID, Namespace: namespace, Status: status, StartedAt: started, CompletedAt: &completed, CreatedAt: started, UpdatedAt: started}
		require.NoError(t, db.Create(run).Error)
		if status == "failed" {
			require.NoError(t, db.Create(&models.TaskRun{ID: uuid.New(), JobRunID: run.ID, TaskID: task.ID, AtomID: task.AtomID, Engine: "docker",
				Image: "busybox:1.36.1", Command: "false", Status: "failed"}).Error)
		}
	}
	return job.ID
}

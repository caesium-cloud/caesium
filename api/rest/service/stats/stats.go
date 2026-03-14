package stats

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"gorm.io/gorm"
)

// StatsResponse is the top-level statistics payload.
type StatsResponse struct {
	Jobs             JobStats     `json:"jobs"`
	TopFailing       []FailingJob `json:"top_failing"`
	SlowestJobs      []SlowestJob `json:"slowest_jobs"`
	SuccessRateTrend []DailyStats `json:"success_rate_trend"`
}

// DailyStats describes success rate for a specific day.
type DailyStats struct {
	Date        string  `json:"date"`
	SuccessRate float64 `json:"success_rate"`
}

// JobStats contains aggregate job statistics.
type JobStats struct {
	Total              int64   `json:"total"`
	RecentRuns         int64   `json:"recent_runs"`
	SuccessRate        float64 `json:"success_rate"`
	AvgDurationSeconds float64 `json:"avg_duration_seconds"`
}

// FailingJob describes a frequently failing job.
type FailingJob struct {
	JobID        string     `json:"job_id"`
	Alias        string     `json:"alias"`
	FailureCount int64      `json:"failure_count"`
	LastFailure  *time.Time `json:"last_failure"`
}

// SlowestJob describes a job with high average duration.
type SlowestJob struct {
	JobID              string  `json:"job_id"`
	Alias              string  `json:"alias"`
	AvgDurationSeconds float64 `json:"avg_duration_seconds"`
}

// Service provides statistics queries.
type Service struct {
	ctx context.Context
	db  *gorm.DB
}

// New creates a Service with the default DB connection.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

// durationExpr returns a SQL expression computing the difference in seconds
// between completed_at and started_at. The expression is dialect-aware:
// Postgres uses EXTRACT(EPOCH FROM ...), SQLite/DQLite uses JULIANDAY arithmetic.
func (s *Service) durationExpr() string {
	if s.db.Name() == "postgres" {
		return "EXTRACT(EPOCH FROM (completed_at - started_at))"
	}
	return "(JULIANDAY(completed_at) - JULIANDAY(started_at)) * 86400"
}

// Get computes aggregate statistics from job_runs and task_runs.
func (s *Service) Get() (*StatsResponse, error) {
	resp := &StatsResponse{}
	durExpr := s.durationExpr()

	// Total distinct jobs
	s.db.WithContext(s.ctx).Model(&models.Job{}).Count(&resp.Jobs.Total)

	// Recent runs (last 24 hours)
	since := time.Now().UTC().Add(-24 * time.Hour)
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Where("started_at >= ?", since).
		Count(&resp.Jobs.RecentRuns)

	// Success rate across all completed runs
	var totalCompleted int64
	var totalSucceeded int64
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Where("status IN ?", []string{"succeeded", "failed"}).
		Count(&totalCompleted)
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Where("status = ?", "succeeded").
		Count(&totalSucceeded)
	if totalCompleted > 0 {
		resp.Jobs.SuccessRate = float64(totalSucceeded) / float64(totalCompleted)
	}

	// Average duration of completed runs
	var avgResult struct{ Avg float64 }
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select("AVG(" + durExpr + ") as avg").
		Where("completed_at IS NOT NULL").
		Scan(&avgResult)
	resp.Jobs.AvgDurationSeconds = avgResult.Avg

	// Top failing jobs (up to 5)
	type failRow struct {
		JobID        string
		FailureCount int64
		LastFailure  *time.Time
	}
	var failRows []failRow
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select("job_id, COUNT(*) as failure_count, MAX(completed_at) as last_failure").
		Where("status = ?", "failed").
		Group("job_id").
		Order("failure_count DESC").
		Limit(5).
		Scan(&failRows)

	resp.TopFailing = make([]FailingJob, 0, len(failRows))
	for _, row := range failRows {
		alias := s.lookupAlias(row.JobID)
		resp.TopFailing = append(resp.TopFailing, FailingJob{
			JobID:        row.JobID,
			Alias:        alias,
			FailureCount: row.FailureCount,
			LastFailure:  row.LastFailure,
		})
	}

	// Slowest jobs (up to 5)
	type slowRow struct {
		JobID string
		Avg   float64
	}
	var slowRows []slowRow
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select("job_id, AVG(" + durExpr + ") as avg").
		Where("completed_at IS NOT NULL").
		Group("job_id").
		Order("avg DESC").
		Limit(5).
		Scan(&slowRows)

	resp.SlowestJobs = make([]SlowestJob, 0, len(slowRows))
	for _, row := range slowRows {
		alias := s.lookupAlias(row.JobID)
		resp.SlowestJobs = append(resp.SlowestJobs, SlowestJob{
			JobID:              row.JobID,
			Alias:              alias,
			AvgDurationSeconds: row.Avg,
		})
	}

	// Success rate trend (last 7 days)
	var trendData []struct {
		Day   string
		Total int64
		Succ  int64
	}
	dayExpr := "strftime('%Y-%m-%d', started_at)"
	if s.db.Name() == "postgres" {
		dayExpr = "TO_CHAR(started_at, 'YYYY-MM-DD')"
	}

	sevenDaysAgo := time.Now().UTC().Add(-7 * 24 * time.Hour)
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select(dayExpr+" as day, COUNT(*) as total, SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END) as succ").
		Where("started_at >= ? AND status IN ?", sevenDaysAgo, []string{"succeeded", "failed"}).
		Group("day").
		Order("day ASC").
		Scan(&trendData)

	resp.SuccessRateTrend = make([]DailyStats, 0, len(trendData))
	for _, d := range trendData {
		rate := 0.0
		if d.Total > 0 {
			rate = float64(d.Succ) / float64(d.Total)
		}
		resp.SuccessRateTrend = append(resp.SuccessRateTrend, DailyStats{
			Date:        d.Day,
			SuccessRate: rate,
		})
	}

	return resp, nil
}

func (s *Service) lookupAlias(jobID string) string {
	var job models.Job
	if err := s.db.WithContext(s.ctx).Select("alias").First(&job, "id = ?", jobID).Error; err != nil {
		return ""
	}
	return job.Alias
}

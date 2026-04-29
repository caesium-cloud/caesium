package stats

import (
	"context"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"gorm.io/gorm"
)

// StatsResponse is the top-level statistics payload.
type StatsResponse struct {
	Jobs             JobStats      `json:"jobs"`
	TopFailing       []FailingJob  `json:"top_failing"`
	TopFailingAtoms  []FailingAtom `json:"top_failing_atoms"`
	SlowestJobs      []SlowestJob  `json:"slowest_jobs"`
	SuccessRateTrend []DailyStats  `json:"success_rate_trend"`
}

// DailyStats describes success rate for a specific day or hour.
type DailyStats struct {
	Date        string  `json:"date"`
	RunCount    int64   `json:"run_count"`
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

// FailingAtom describes a frequently failing atom.
type FailingAtom struct {
	JobID        string `json:"job_id"`
	Alias        string `json:"alias"`
	AtomName     string `json:"atom_name"`
	FailureCount int64  `json:"failure_count"`
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
// between completed_at and started_at.
func (s *Service) durationExpr() string {
	if s.db.Name() == "postgres" {
		return "EXTRACT(EPOCH FROM (completed_at - started_at))"
	}
	return "(JULIANDAY(completed_at) - JULIANDAY(started_at)) * 86400"
}

// Get is a legacy wrapper for Summary("7d").
func (s *Service) Get() (*StatsResponse, error) {
	return s.Summary("7d")
}

// Summary computes aggregate statistics for the given window.
func (s *Service) Summary(window string) (*StatsResponse, error) {
	resp := &StatsResponse{}
	durExpr := s.durationExpr()

	var since time.Time
	var days int
	var hourly bool

	switch window {
	case "24h":
		since = time.Now().UTC().Add(-24 * time.Hour)
		days = 24
		hourly = true
	case "30d":
		since = time.Now().UTC().Add(-30 * 24 * time.Hour)
		days = 30
	default: // 7d
		since = time.Now().UTC().Add(-7 * 24 * time.Hour)
		days = 7
	}

	// Total distinct jobs (not windowed)
	s.db.WithContext(s.ctx).Model(&models.Job{}).Count(&resp.Jobs.Total)

	// Recent runs (always 24h as per spec KPI)
	recentSince := time.Now().UTC().Add(-24 * time.Hour)
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Where("started_at >= ?", recentSince).
		Count(&resp.Jobs.RecentRuns)

	// Success rate in window
	var totalCompleted int64
	var totalSucceeded int64
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Where("status IN ? AND started_at >= ?", []string{"succeeded", "failed"}, since).
		Count(&totalCompleted)
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Where("status = ? AND started_at >= ?", "succeeded", since).
		Count(&totalSucceeded)
	if totalCompleted > 0 {
		resp.Jobs.SuccessRate = float64(totalSucceeded) / float64(totalCompleted)
	}

	// Average duration in window
	var avgResult struct{ Avg float64 }
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select("AVG(" + durExpr + ") as avg").
		Where("completed_at IS NOT NULL AND started_at >= ?", since).
		Scan(&avgResult)
	resp.Jobs.AvgDurationSeconds = avgResult.Avg

	// Top failing jobs (up to 5) in window
	type failRow struct {
		JobID        string
		FailureCount int64
		LastFailure  *time.Time
	}
	var failRows []failRow
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select("job_id, COUNT(*) as failure_count, MAX(completed_at) as last_failure").
		Where("status = ? AND started_at >= ?", "failed", since).
		Group("job_id").
		Order("failure_count DESC").
		Limit(5).
		Scan(&failRows)

	resp.TopFailing = make([]FailingJob, 0, len(failRows))
	for _, row := range failRows {
		resp.TopFailing = append(resp.TopFailing, FailingJob{
			JobID:        row.JobID,
			Alias:        s.lookupAlias(row.JobID),
			FailureCount: row.FailureCount,
			LastFailure:  row.LastFailure,
		})
	}

	// Top failing atoms (up to 5) in window
	var atomRows []struct {
		JobID        string
		AtomName     string
		FailureCount int64
	}
	s.db.WithContext(s.ctx).Model(&models.TaskRun{}).
		Select("job_runs.job_id, tasks.name as atom_name, COUNT(*) as failure_count").
		Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id").
		Joins("JOIN tasks ON tasks.id = task_runs.task_id").
		Where("task_runs.status = ? AND job_runs.started_at >= ?", "failed", since).
		Group("job_runs.job_id, tasks.name").
		Order("failure_count DESC").
		Limit(5).
		Scan(&atomRows)

	resp.TopFailingAtoms = make([]FailingAtom, 0, len(atomRows))
	for _, row := range atomRows {
		resp.TopFailingAtoms = append(resp.TopFailingAtoms, FailingAtom{
			JobID:        row.JobID,
			Alias:        s.lookupAlias(row.JobID),
			AtomName:     row.AtomName,
			FailureCount: row.FailureCount,
		})
	}

	// Slowest jobs (up to 5) in window
	type slowRow struct {
		JobID string
		Avg   float64
	}
	var slowRows []slowRow
	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select("job_id, AVG(" + durExpr + ") as avg").
		Where("completed_at IS NOT NULL AND started_at >= ?", since).
		Group("job_id").
		Order("avg DESC").
		Limit(5).
		Scan(&slowRows)

	resp.SlowestJobs = make([]SlowestJob, 0, len(slowRows))
	for _, row := range slowRows {
		resp.SlowestJobs = append(resp.SlowestJobs, SlowestJob{
			JobID:              row.JobID,
			Alias:              s.lookupAlias(row.JobID),
			AvgDurationSeconds: row.Avg,
		})
	}

	// Trend data
	var trendData []struct {
		Point    string
		Succ     int64
		RunCount int64
	}

	pointExpr := "strftime('%Y-%m-%d', started_at, 'utc')"
	if hourly {
		pointExpr = "strftime('%Y-%m-%dT%H:00:00Z', started_at, 'utc')"
	}

	if s.db.Name() == "postgres" {
		if hourly {
			pointExpr = "TO_CHAR(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:00:00\"Z\"')"
		} else {
			pointExpr = "TO_CHAR(started_at AT TIME ZONE 'UTC', 'YYYY-MM-DD')"
		}
	}

	s.db.WithContext(s.ctx).Model(&models.JobRun{}).
		Select(pointExpr+" as point, COUNT(*) as run_count, SUM(CASE WHEN status = 'succeeded' THEN 1 ELSE 0 END) as succ").
		Where("started_at >= ? AND status IN ?", since, []string{"succeeded", "failed"}).
		Group("point").
		Order("point ASC").
		Scan(&trendData)

	trendMap := make(map[string]DailyStats, len(trendData))
	for _, d := range trendData {
		rate := 0.0
		if d.RunCount > 0 {
			rate = float64(d.Succ) / float64(d.RunCount)
		}
		trendMap[d.Point] = DailyStats{
			Date:        d.Point,
			RunCount:    d.RunCount,
			SuccessRate: rate,
		}
	}

	resp.SuccessRateTrend = make([]DailyStats, 0, days)
	if hourly {
		now := time.Now().UTC()
		startHour := now.Add(-23 * time.Hour).Truncate(time.Hour)
		for i := 0; i < 24; i++ {
			hour := startHour.Add(time.Duration(i) * time.Hour)
			hourStr := hour.Format("2006-01-02T15:04:05Z")
			point := trendMap[hourStr]
			if point.Date == "" {
				point.Date = hourStr
			}
			resp.SuccessRateTrend = append(resp.SuccessRateTrend, point)
		}
	} else {
		now := time.Now().UTC()
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		startDay := today.Add(time.Duration(-(days - 1)) * 24 * time.Hour)
		for i := 0; i < days; i++ {
			day := startDay.Add(time.Duration(i) * 24 * time.Hour)
			dayStr := day.Format("2006-01-02")
			point := trendMap[dayStr]
			if point.Date == "" {
				point.Date = dayStr
			}
			resp.SuccessRateTrend = append(resp.SuccessRateTrend, point)
		}
	}

	return resp, nil
}

func (s *Service) lookupAlias(jobID string) string {
	var job models.Job
	if err := s.db.WithContext(s.ctx).Unscoped().Select("alias").First(&job, "id = ?", jobID).Error; err != nil {
		return ""
	}
	return job.Alias
}

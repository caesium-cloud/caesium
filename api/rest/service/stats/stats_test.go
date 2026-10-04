package stats

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type StatsSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestStatsSuite(t *testing.T) {
	suite.Run(t, new(StatsSuite))
}

func (s *StatsSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *StatsSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *StatsSuite) TestEmptyDatabaseReturnsZeros() {
	svc := &Service{ctx: context.Background(), db: s.db}
	resp, err := svc.Get()
	s.Require().NoError(err)
	s.Require().NotNil(resp)

	s.Equal(int64(0), resp.Jobs.Total)
	s.Equal(int64(0), resp.Jobs.RecentRuns)
	s.Equal(float64(0), resp.Jobs.SuccessRate)
	s.Empty(resp.TopFailing)
	s.Empty(resp.SlowestJobs)
	s.Len(resp.SuccessRateTrend, 7)
	for _, day := range resp.SuccessRateTrend {
		s.Equal(int64(0), day.RunCount)
		s.Equal(float64(0), day.SuccessRate)
	}
}

func (s *StatsSuite) TestSummaryReturnsCountError() {
	want := errors.New("job count failed")
	s.failQuery("stats_test_count_error", func(tx *gorm.DB) bool {
		_, isCount := tx.Statement.Dest.(*int64)
		return tx.Statement.Table == "jobs" && isCount
	}, want)

	resp, err := (&Service{ctx: context.Background(), db: s.db}).Summary("7d")
	s.Nil(resp)
	s.ErrorIs(err, want)
}

func (s *StatsSuite) TestSummaryReturnsScanError() {
	want := errors.New("duration scan failed")
	s.failQuery("stats_test_scan_error", func(tx *gorm.DB) bool {
		if tx.Statement.Table != "job_runs" {
			return false
		}
		for _, selection := range tx.Statement.Selects {
			if strings.Contains(strings.ToUpper(selection), "AVG(") {
				return true
			}
		}
		return false
	}, want)

	resp, err := (&Service{ctx: context.Background(), db: s.db}).Summary("7d")
	s.Nil(resp)
	s.ErrorIs(err, want)
}

func (s *StatsSuite) TestSummaryReturnsAliasLookupError() {
	jobID := s.createJob("alias-error")
	now := time.Now().UTC()
	completed := now.Add(-time.Minute)
	s.createJobRun(jobID, "failed", now.Add(-2*time.Minute), &completed)

	want := errors.New("alias lookup failed")
	s.failQuery("stats_test_alias_error", func(tx *gorm.DB) bool {
		_, isJobLookup := tx.Statement.Dest.(*models.Job)
		return tx.Statement.Table == "jobs" && isJobLookup
	}, want)

	resp, err := (&Service{ctx: context.Background(), db: s.db}).Summary("7d")
	s.Nil(resp)
	s.ErrorIs(err, want)
}

func (s *StatsSuite) TestLookupAliasTreatsMissingJobAsEmpty() {
	alias, err := (&Service{ctx: context.Background(), db: s.db}).lookupAlias(uuid.NewString())
	s.NoError(err)
	s.Empty(alias)
}

func (s *StatsSuite) failQuery(name string, matches func(*gorm.DB) bool, want error) {
	callback := func(tx *gorm.DB) {
		if matches(tx) {
			tx.AddError(want)
		}
	}
	s.Require().NoError(s.db.Callback().Query().Before("gorm:query").Register(name, callback))
	s.Require().NoError(s.db.Callback().Row().Before("gorm:row").Register(name, callback))
}

func (s *StatsSuite) TestSuccessRateComputedCorrectly() {
	jobID := s.createJob("rate-test")

	// 3 succeeded, 1 failed = 75%
	now := time.Now().UTC()
	for i := range 3 {
		completed := now.Add(-time.Duration(i) * time.Minute)
		s.createJobRun(jobID, "succeeded", now.Add(-time.Duration(i+1)*time.Minute), &completed)
	}
	completed := now.Add(-5 * time.Minute)
	s.createJobRun(jobID, "failed", now.Add(-6*time.Minute), &completed)

	svc := &Service{ctx: context.Background(), db: s.db}
	resp, err := svc.Get()
	s.Require().NoError(err)
	s.Equal(int64(1), resp.Jobs.Total)
	s.Equal(0.75, resp.Jobs.SuccessRate)
}

func (s *StatsSuite) TestRecentRunsCountsLast24Hours() {
	jobID := s.createJob("recent-test")
	now := time.Now().UTC()

	// One recent run
	completed := now.Add(-1 * time.Hour)
	s.createJobRun(jobID, "succeeded", now.Add(-2*time.Hour), &completed)

	// One old run
	oldComplete := now.Add(-48 * time.Hour)
	s.createJobRun(jobID, "succeeded", now.Add(-49*time.Hour), &oldComplete)

	svc := &Service{ctx: context.Background(), db: s.db}
	resp, err := svc.Get()
	s.Require().NoError(err)
	s.Equal(int64(1), resp.Jobs.RecentRuns)
}

func (s *StatsSuite) TestTopFailingJobsRanked() {
	jobA := s.createJob("failing-a")
	jobB := s.createJob("failing-b")
	now := time.Now().UTC()

	// jobA fails 3 times
	for i := range 3 {
		completed := now.Add(-time.Duration(i) * time.Minute)
		s.createJobRun(jobA, "failed", now.Add(-time.Duration(i+1)*time.Minute), &completed)
	}
	// jobB fails 1 time
	completed := now.Add(-1 * time.Minute)
	s.createJobRun(jobB, "failed", now.Add(-2*time.Minute), &completed)

	svc := &Service{ctx: context.Background(), db: s.db}
	resp, err := svc.Get()
	s.Require().NoError(err)
	s.Require().Len(resp.TopFailing, 2)
	s.Equal(jobA.String(), resp.TopFailing[0].JobID)
	s.Equal("failing-a", resp.TopFailing[0].Alias)
	s.Equal(int64(3), resp.TopFailing[0].FailureCount)
	s.NotNil(resp.TopFailing[0].LastFailure)
	s.Equal(now.Unix(), resp.TopFailing[0].LastFailure.Unix())
	s.Equal(time.UTC, resp.TopFailing[0].LastFailure.Location())
	s.Equal(int64(1), resp.TopFailing[1].FailureCount)
}

func (s *StatsSuite) TestTopFailingJobAllowsNullLatestFailure() {
	jobID := s.createJob("failed-without-completion")
	s.createJobRun(jobID, "failed", time.Now().UTC().Add(-time.Minute), nil)

	resp, err := (&Service{ctx: context.Background(), db: s.db}).Summary("7d")
	s.Require().NoError(err)
	s.Require().Len(resp.TopFailing, 1)
	s.Equal(int64(1), resp.TopFailing[0].FailureCount)
	s.Nil(resp.TopFailing[0].LastFailure)
}

func (s *StatsSuite) TestSummaryReturnsMalformedLatestFailureTimestampError() {
	jobID := s.createJob("bad-latest-failure")
	completed := time.Now().UTC().Truncate(time.Second)
	s.createJobRun(jobID, "failed", completed.Add(-time.Minute), &completed)
	s.Require().NoError(s.db.Exec("UPDATE job_runs SET completed_at = ? WHERE job_id = ?", "not-a-timestamp", jobID.String()).Error)

	resp, err := (&Service{ctx: context.Background(), db: s.db}).Summary("7d")
	s.Nil(resp)
	s.ErrorContains(err, "parse latest failure timestamp")
	s.ErrorContains(err, "invalid timestamp")
}

func TestParseAggregateTime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value sql.NullString
		want  time.Time
		bad   bool
	}{
		{name: "null"},
		{
			name:  "sqlite dqlite naive UTC",
			value: sql.NullString{String: "2025-01-02 03:04:05.123456789", Valid: true},
			want:  time.Date(2025, time.January, 2, 3, 4, 5, 123456789, time.UTC),
		},
		{
			name:  "postgres offset",
			value: sql.NullString{String: "2025-01-02 03:04:05.123456-07:00", Valid: true},
			want:  time.Date(2025, time.January, 2, 10, 4, 5, 123456000, time.UTC),
		},
		{
			name:  "rfc3339",
			value: sql.NullString{String: "2025-01-02T03:04:05.123456789+02:00", Valid: true},
			want:  time.Date(2025, time.January, 2, 1, 4, 5, 123456789, time.UTC),
		},
		{name: "malformed", value: sql.NullString{String: "not-a-timestamp", Valid: true}, bad: true},
		{name: "empty but non-null", value: sql.NullString{String: " ", Valid: true}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAggregateTime(tc.value)
			if tc.bad {
				if err == nil || got != nil {
					t.Fatalf("parseAggregateTime(%+v) = %v, %v; want error", tc.value, got, err)
				}
				return
			}
			if tc.value.Valid != (got != nil) {
				t.Fatalf("parseAggregateTime(%+v) = %v, %v", tc.value, got, err)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != nil && (!got.Equal(tc.want) || got.Location() != time.UTC) {
				t.Fatalf("timestamp = %s (%s), want %s (UTC)", got, got.Location(), tc.want)
			}
		})
	}
}

func (s *StatsSuite) TestSlowestJobsRanked() {
	jobA := s.createJob("slow")
	jobB := s.createJob("fast")
	now := time.Now().UTC()

	// jobA: 100 second run
	completedA := now.Add(-1 * time.Minute)
	startedA := completedA.Add(-100 * time.Second)
	s.createJobRun(jobA, "succeeded", startedA, &completedA)

	// jobB: 10 second run
	completedB := now.Add(-2 * time.Minute)
	startedB := completedB.Add(-10 * time.Second)
	s.createJobRun(jobB, "succeeded", startedB, &completedB)

	svc := &Service{ctx: context.Background(), db: s.db}
	resp, err := svc.Get()
	s.Require().NoError(err)
	s.Require().Len(resp.SlowestJobs, 2)
	s.Equal(jobA.String(), resp.SlowestJobs[0].JobID)
	s.Equal("slow", resp.SlowestJobs[0].Alias)
	s.Greater(resp.SlowestJobs[0].AvgDurationSeconds, resp.SlowestJobs[1].AvgDurationSeconds)
}

func (s *StatsSuite) TestAvgDurationComputed() {
	jobID := s.createJob("avg-test")
	now := time.Now().UTC()

	// Two runs: 60s and 120s = avg 90s
	c1 := now.Add(-1 * time.Minute)
	s.createJobRun(jobID, "succeeded", c1.Add(-60*time.Second), &c1)
	c2 := now.Add(-5 * time.Minute)
	s.createJobRun(jobID, "succeeded", c2.Add(-120*time.Second), &c2)

	svc := &Service{ctx: context.Background(), db: s.db}
	resp, err := svc.Get()
	s.Require().NoError(err)
	s.InDelta(90.0, resp.Jobs.AvgDurationSeconds, 1.0)
}

func (s *StatsSuite) TestSuccessRateTrendUsesFullCalendarWindow() {
	jobID := s.createJob("trend-test")
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	completedToday := today.Add(2 * time.Hour)
	s.createJobRun(jobID, "succeeded", today.Add(time.Hour), &completedToday)

	twoDaysAgo := today.Add(-2 * 24 * time.Hour)
	completedTwoDaysAgo := twoDaysAgo.Add(3 * time.Hour)
	s.createJobRun(jobID, "failed", twoDaysAgo.Add(2*time.Hour), &completedTwoDaysAgo)

	svc := &Service{ctx: context.Background(), db: s.db}
	resp, err := svc.Get()
	s.Require().NoError(err)
	s.Require().Len(resp.SuccessRateTrend, 7)

	byDate := make(map[string]DailyStats, len(resp.SuccessRateTrend))
	for _, day := range resp.SuccessRateTrend {
		byDate[day.Date] = day
	}

	s.Equal(today.Add(-6*24*time.Hour).Format("2006-01-02"), resp.SuccessRateTrend[0].Date)
	s.Equal(today.Format("2006-01-02"), resp.SuccessRateTrend[6].Date)
	s.Equal(int64(1), byDate[today.Format("2006-01-02")].RunCount)
	s.Equal(1.0, byDate[today.Format("2006-01-02")].SuccessRate)
	s.Equal(int64(1), byDate[twoDaysAgo.Format("2006-01-02")].RunCount)
	s.Equal(0.0, byDate[twoDaysAgo.Format("2006-01-02")].SuccessRate)
	s.Equal(int64(0), byDate[today.Add(-1*24*time.Hour).Format("2006-01-02")].RunCount)
	s.Equal(0.0, byDate[today.Add(-1*24*time.Hour).Format("2006-01-02")].SuccessRate)
}

func (s *StatsSuite) createJob(alias string) uuid.UUID {
	id := uuid.New()
	triggerID := uuid.New()
	s.Require().NoError(s.db.Create(&models.Trigger{
		ID:   triggerID,
		Type: models.TriggerTypeCron,
	}).Error)
	s.Require().NoError(s.db.Create(&models.Job{
		ID:        id,
		Alias:     alias,
		TriggerID: triggerID,
	}).Error)
	return id
}

func (s *StatsSuite) createJobRun(jobID uuid.UUID, status string, started time.Time, completed *time.Time) {
	run := &models.JobRun{
		ID:          uuid.New(),
		JobID:       jobID,
		Status:      status,
		StartedAt:   started,
		CompletedAt: completed,
	}
	s.Require().NoError(s.db.Create(run).Error)
}

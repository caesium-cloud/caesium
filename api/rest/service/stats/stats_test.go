package stats

import (
	"context"
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
}

func (s *StatsSuite) TestSuccessRateComputedCorrectly() {
	jobID := s.createJob("rate-test")

	// 3 succeeded, 1 failed = 75%
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
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
	for i := 0; i < 3; i++ {
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
	s.Equal(int64(1), resp.TopFailing[1].FailureCount)
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

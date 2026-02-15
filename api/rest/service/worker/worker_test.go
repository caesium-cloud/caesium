package worker

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

type WorkerStatusSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestWorkerStatusSuite(t *testing.T) {
	suite.Run(t, new(WorkerStatusSuite))
}

func (s *WorkerStatusSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *WorkerStatusSuite) TearDownTest() {
	if s.db == nil {
		return
	}
	sqlDB, _ := s.db.DB()
	if sqlDB != nil {
		_ = sqlDB.Close()
	}
}

func (s *WorkerStatusSuite) TestStatusWithNoClaimsReturnsEmpty() {
	svc := New(context.Background()).WithDatabase(s.db)

	resp, err := svc.Status("node-a")
	s.Require().NoError(err)
	s.Require().NotNil(resp)

	s.Equal("node-a", resp.Address)
	s.Equal(int64(0), resp.TotalClaimedTasks)
	s.Equal(int64(0), resp.RunningClaims)
	s.Equal(int64(0), resp.ExpiredLeases)
	s.Equal(int64(0), resp.TotalClaimAttempts)
	s.Nil(resp.LastActivityAt)
	s.Empty(resp.ActiveClaims)
	s.Empty(resp.ClaimedByStatus)
}

func (s *WorkerStatusSuite) TestStatusAggregatesClaimsAndExpirations() {
	now := time.Now().UTC()

	s.seedTaskRun(taskRunSeed{
		claimedBy:      "node-a",
		status:         "running",
		claimAttempt:   3,
		claimExpiresAt: ptrTime(now.Add(2 * time.Minute)),
		updatedAt:      now.Add(-5 * time.Second),
	})
	s.seedTaskRun(taskRunSeed{
		claimedBy:      "node-a",
		status:         "running",
		claimAttempt:   2,
		claimExpiresAt: ptrTime(now.Add(-2 * time.Minute)),
		updatedAt:      now.Add(-10 * time.Second),
	})
	s.seedTaskRun(taskRunSeed{
		claimedBy:    "node-a",
		status:       "succeeded",
		claimAttempt: 1,
		updatedAt:    now.Add(-20 * time.Second),
	})
	s.seedTaskRun(taskRunSeed{
		claimedBy:    "node-a",
		status:       "failed",
		claimAttempt: 4,
		updatedAt:    now.Add(-30 * time.Second),
	})
	s.seedTaskRun(taskRunSeed{
		claimedBy:    "node-b",
		status:       "running",
		claimAttempt: 9,
		updatedAt:    now,
	})

	svc := New(context.Background()).WithDatabase(s.db)
	resp, err := svc.Status("node-a")
	s.Require().NoError(err)

	s.Equal(int64(4), resp.TotalClaimedTasks)
	s.Equal(int64(2), resp.RunningClaims)
	s.Equal(int64(1), resp.ExpiredLeases)
	s.Equal(int64(10), resp.TotalClaimAttempts)
	s.Require().NotNil(resp.LastActivityAt)
	s.WithinDuration(now.Add(-5*time.Second), *resp.LastActivityAt, time.Second)

	s.Equal(int64(2), resp.ClaimedByStatus["running"])
	s.Equal(int64(1), resp.ClaimedByStatus["succeeded"])
	s.Equal(int64(1), resp.ClaimedByStatus["failed"])

	s.Require().Len(resp.ActiveClaims, 2)
	s.Equal("running", resp.ActiveClaims[0].Status)
	s.True(resp.ActiveClaims[0].UpdatedAt.After(resp.ActiveClaims[1].UpdatedAt) || resp.ActiveClaims[0].UpdatedAt.Equal(resp.ActiveClaims[1].UpdatedAt))
}

type taskRunSeed struct {
	claimedBy      string
	status         string
	claimAttempt   int
	claimExpiresAt *time.Time
	updatedAt      time.Time
}

func (s *WorkerStatusSuite) seedTaskRun(in taskRunSeed) {
	if in.updatedAt.IsZero() {
		in.updatedAt = time.Now().UTC()
	}

	taskRun := &models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                uuid.New(),
		TaskID:                  uuid.New(),
		AtomID:                  uuid.New(),
		Engine:                  models.AtomEngineDocker,
		Image:                   "alpine:3.20",
		Command:                 `["echo","ok"]`,
		Status:                  in.status,
		ClaimedBy:               in.claimedBy,
		ClaimExpiresAt:          in.claimExpiresAt,
		ClaimAttempt:            in.claimAttempt,
		OutstandingPredecessors: 0,
		CreatedAt:               in.updatedAt.Add(-time.Second),
		UpdatedAt:               in.updatedAt,
	}

	s.Require().NoError(s.db.Create(taskRun).Error)
}

func ptrTime(v time.Time) *time.Time {
	return &v
}

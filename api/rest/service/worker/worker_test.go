package worker

import (
	"context"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
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
	s.db = jobdeftestutil.OpenTestDB(s.T())
}

func (s *WorkerStatusSuite) TearDownTest() {
	jobdeftestutil.CloseDB(s.db)
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
		claimExpiresAt: new(now.Add(2 * time.Minute)),
		updatedAt:      now.Add(-5 * time.Second).In(time.FixedZone("east", 5*60*60)),
	})
	s.seedTaskRun(taskRunSeed{
		claimedBy:      "node-a",
		status:         "running",
		claimAttempt:   2,
		claimExpiresAt: new(now.Add(-2 * time.Minute)),
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
	s.Equal(time.UTC, resp.LastActivityAt.Location())

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
		Image:                   "alpine:3.23",
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

func TestParseAggregateTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want time.Time
	}{
		{"empty", "", time.Time{}},
		{"blank", " \t\n", time.Time{}},
		{"malformed", "not-a-timestamp", time.Time{}},
		{"padded valid timestamp remains invalid", " 2025-01-02T03:04:05Z ", time.Time{}},
		{"leading whitespace remains invalid", "\t2025-01-02 03:04:05", time.Time{}},
		{"SQL UTC", "2025-01-02 03:04:05.123456789", time.Date(2025, 1, 2, 3, 4, 5, 123456789, time.UTC)},
		{"SQL offset", "2025-01-02 03:04:05.123456-07:00", time.Date(2025, 1, 2, 10, 4, 5, 123456000, time.UTC)},
		{"RFC3339 offset", "2025-01-02T03:04:05.123456789+02:00", time.Date(2025, 1, 2, 1, 4, 5, 123456789, time.UTC)},
		{"SQL seconds", "2025-01-02 03:04:05", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAggregateTime(tc.raw)
			if tc.want.IsZero() {
				if got != nil {
					t.Fatalf("parseAggregateTime(%q) = %v; want nil", tc.raw, got)
				}
				return
			}
			if got == nil || !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("parseAggregateTime(%q) = %v; want %v in UTC", tc.raw, got, tc.want)
			}
		})
	}
}

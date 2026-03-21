package job

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	backfillstore "github.com/caesium-cloud/caesium/internal/backfill"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/robfig/cron"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"
	"gorm.io/datatypes"
)

type fakeBackfillStateReader struct {
	running         bool
	cancelRequested bool
}

func (f fakeBackfillStateReader) IsRunning(uuid.UUID) (bool, error) {
	return f.running, nil
}

func (f fakeBackfillStateReader) IsCancelRequested(uuid.UUID) (bool, error) {
	return f.cancelRequested, nil
}

func mustParseCron(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	s, err := parser.Parse(expr)
	if err != nil {
		t.Fatalf("mustParseCron(%q): %v", expr, err)
	}
	return s
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("mustParseTime(%q): %v", s, err)
	}
	return ts
}

// TestEnumerateLogicalDates_HourlyInOneHourWindow expects exactly 1 fire time.
func TestEnumerateLogicalDates_HourlyInOneHourWindow(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *") // every hour on the hour
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	end := mustParseTime(t, "2024-01-01T01:00:00Z") // exclusive

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 1 {
		t.Fatalf("expected 1 date, got %d: %v", len(dates), dates)
	}
	want := mustParseTime(t, "2024-01-01T00:00:00Z")
	if !dates[0].Equal(want) {
		t.Errorf("dates[0] = %v, want %v", dates[0], want)
	}
}

// TestEnumerateLogicalDates_HourlyInTwelveHourWindow expects 12 fire times.
func TestEnumerateLogicalDates_HourlyInTwelveHourWindow(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *")
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	end := mustParseTime(t, "2024-01-01T12:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 12 {
		t.Fatalf("expected 12 dates, got %d", len(dates))
	}
}

// TestEnumerateLogicalDates_EndBeforeStart returns empty.
func TestEnumerateLogicalDates_EndBeforeStart(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *")
	start := mustParseTime(t, "2024-01-01T12:00:00Z")
	end := mustParseTime(t, "2024-01-01T00:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 0 {
		t.Fatalf("expected 0 dates, got %d", len(dates))
	}
}

// TestEnumerateLogicalDates_FiveMinuteSchedule expects 12 fire times in 1 hour.
func TestEnumerateLogicalDates_FiveMinuteSchedule(t *testing.T) {
	sched := mustParseCron(t, "*/5 * * * *")
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	end := mustParseTime(t, "2024-01-01T01:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 12 {
		t.Fatalf("expected 12 dates, got %d: %v", len(dates), dates)
	}
}

// TestEnumerateLogicalDates_ExactBoundary verifies that end is exclusive.
func TestEnumerateLogicalDates_ExactBoundary(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *")
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	// end is exactly on a fire time — should NOT be included
	end := mustParseTime(t, "2024-01-01T02:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 2 {
		t.Fatalf("expected 2 dates (00:00 and 01:00), got %d: %v", len(dates), dates)
	}
}

func TestFilterDatesRespectsReprocessPolicies(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	store := backfillstore.NewStore(db)
	jobID := uuid.New()

	insertRun := func(logicalDate string, status string, createdAt time.Time) {
		t.Helper()

		params, err := json.Marshal(map[string]string{"logical_date": logicalDate})
		require.NoError(t, err)

		run := &models.JobRun{
			ID:        uuid.New(),
			JobID:     jobID,
			Status:    status,
			Params:    datatypes.JSON(params),
			StartedAt: createdAt,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		}
		require.NoError(t, db.Create(run).Error)
	}

	date1 := mustParseTime(t, "2024-01-01T00:00:00Z")
	date2 := mustParseTime(t, "2024-01-01T01:00:00Z")
	date3 := mustParseTime(t, "2024-01-01T02:00:00Z")

	insertRun(date1.UTC().Format(time.RFC3339), "succeeded", date1.Add(-2*time.Minute))
	insertRun(date2.UTC().Format(time.RFC3339), "succeeded", date2.Add(-3*time.Minute))
	insertRun(date2.UTC().Format(time.RFC3339), "failed", date2.Add(-1*time.Minute))

	dates := []time.Time{date1, date2, date3}

	all, err := FilterDates(store, jobID, dates, string(models.ReprocessAll))
	require.NoError(t, err)
	require.Equal(t, dates, all)

	none, err := FilterDates(store, jobID, dates, string(models.ReprocessNone))
	require.NoError(t, err)
	require.Equal(t, []time.Time{date3}, none)

	failed, err := FilterDates(store, jobID, dates, string(models.ReprocessFailed))
	require.NoError(t, err)
	require.Equal(t, []time.Time{date2, date3}, failed)
}

func TestShouldStopBackfillHonorsCancelRequest(t *testing.T) {
	stop := shouldStopBackfill(
		context.Background(),
		fakeBackfillStateReader{running: true, cancelRequested: true},
		uuid.New(),
	)
	require.True(t, stop)
}

func TestShouldStopBackfillHonorsNotRunningState(t *testing.T) {
	stop := shouldStopBackfill(
		context.Background(),
		fakeBackfillStateReader{running: false},
		uuid.New(),
	)
	require.True(t, stop)
}

func TestWaitForBackfillSlotStopsWhileWaiting(t *testing.T) {
	sem := semaphore.NewWeighted(1)
	require.NoError(t, sem.Acquire(context.Background(), 1))
	defer sem.Release(1)

	var stopRequested atomic.Bool
	go func() {
		time.Sleep(75 * time.Millisecond)
		stopRequested.Store(true)
	}()

	start := time.Now()
	acquired := waitForBackfillSlot(context.Background(), sem, func() bool {
		return stopRequested.Load()
	})

	require.False(t, acquired)
	require.Less(t, time.Since(start), time.Second)
}

func TestWaitForBackfillSlotAcquiresPermit(t *testing.T) {
	sem := semaphore.NewWeighted(1)

	acquired := waitForBackfillSlot(context.Background(), sem, func() bool {
		return false
	})
	require.True(t, acquired)

	sem.Release(1)
}

package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestLimiterAcquireConditionalUpsertRowsAffected(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	metrics.RateLimitAcquiredTotal.Reset()
	metrics.RateLimitRejectedTotal.Reset()

	now := time.Date(2026, 6, 29, 12, 0, 15, 0, time.UTC)
	limiter := NewLimiter(db, WithClock(func() time.Time { return now }))

	acquired, err := limiter.Acquire(context.Background(), "database", 1, 2, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired, "first insert should affect one row")

	acquired, err = limiter.Acquire(context.Background(), "database", 1, 2, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired, "guarded update within the limit should affect one row")

	acquired, err = limiter.Acquire(context.Background(), "database", 1, 2, time.Minute)
	require.NoError(t, err)
	require.False(t, acquired, "guarded update over the limit should affect zero rows")

	var token models.RateLimitToken
	require.NoError(t, db.First(&token, "resource = ? AND window_key = ?", "database", now.Truncate(time.Minute).Format(time.RFC3339Nano)).Error)
	require.Equal(t, 2, token.Consumed, "rejected acquire must not increment consumed")
	require.Equal(t, 2, token.LimitVal)

	now = now.Add(time.Minute)
	acquired, err = limiter.Acquire(context.Background(), "database", 1, 2, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired, "new window should insert a fresh token")
}

func TestLimiterAcquireConflictUsesCurrentCallerLimit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Date(2026, 6, 29, 12, 0, 15, 0, time.UTC)
	limiter := NewLimiter(db, WithClock(func() time.Time { return now }))

	acquired, err := limiter.Acquire(context.Background(), "database", 1, 5, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)

	acquired, err = limiter.Acquire(context.Background(), "database", 2, 2, time.Minute)
	require.NoError(t, err)
	require.False(t, acquired, "conflict guard must use the second caller's lower limit")

	acquired, err = limiter.Acquire(context.Background(), "database", 1, 2, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired, "one more unit should fit under the second caller's limit")

	var token models.RateLimitToken
	require.NoError(t, db.First(&token, "resource = ? AND window_key = ?", "database", now.Truncate(time.Minute).Format(time.RFC3339Nano)).Error)
	require.Equal(t, 2, token.Consumed)
	require.Equal(t, 2, token.LimitVal)
}

func TestLimiterRejectsSingleAcquireOverLimitOnFreshWindow(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Date(2026, 6, 29, 12, 0, 15, 0, time.UTC)
	limiter := NewLimiter(db, WithClock(func() time.Time { return now }))

	acquired, err := limiter.Acquire(context.Background(), "database", 3, 2, time.Minute)
	require.NoError(t, err)
	require.False(t, acquired)

	var count int64
	require.NoError(t, db.Model(&models.RateLimitToken{}).Where("resource = ?", "database").Count(&count).Error)
	require.Zero(t, count)
}

func TestLimiterNormalizesSubMinuteWindows(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	limiter := NewLimiter(db, WithClock(func() time.Time { return now }))

	acquired, err := limiter.Acquire(context.Background(), "api", 1, 1, time.Second)
	require.NoError(t, err)
	require.True(t, acquired)

	now = now.Add(30 * time.Second)
	acquired, err = limiter.Acquire(context.Background(), "api", 1, 1, time.Second)
	require.NoError(t, err)
	require.False(t, acquired, "sub-minute windows should share the same one-minute bucket")
}

func TestRuleForTaskTreatsMissingOrEmptyResourceAsNoRule(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	rule, ok, err := RuleForTask(context.Background(), db, uuid.New(), uuid.New())
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, rule)

	now := time.Date(2026, 6, 29, 12, 0, 15, 0, time.UTC)
	triggerID := uuid.New()
	jobID := uuid.New()
	atomID := uuid.New()
	taskID := uuid.New()
	runID := uuid.New()

	require.NoError(t, db.Create(&models.Trigger{
		ID:        triggerID,
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     "unlimited-job",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Atom{
		ID:        atomID,
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["sh","-c","true"]`,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{
		ID:        taskID,
		JobID:     jobID,
		AtomID:    atomID,
		Name:      "unlimited-step",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.JobRun{
		ID:          runID,
		JobID:       jobID,
		TriggerID:   triggerID,
		TriggerType: string(models.TriggerTypeCron),
		Status:      "running",
		StartedAt:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error)
	require.NoError(t, db.Create(&models.TaskRun{
		ID:        uuid.New(),
		JobRunID:  runID,
		TaskID:    taskID,
		AtomID:    atomID,
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["sh","-c","true"]`,
		Status:    "pending",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	rule, ok, err = RuleForTask(context.Background(), db, runID, taskID)
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, rule)
}

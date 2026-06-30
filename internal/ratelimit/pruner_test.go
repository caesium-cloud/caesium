package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestPrunerDeletesExpiredWindows(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	require.NoError(t, db.Create(&models.RateLimitToken{
		Resource:  "api",
		WindowKey: "expired",
		Consumed:  1,
		LimitVal:  1,
		ExpiresAt: now.Add(-time.Second),
	}).Error)
	require.NoError(t, db.Create(&models.RateLimitToken{
		Resource:  "api",
		WindowKey: "live",
		Consumed:  1,
		LimitVal:  1,
		ExpiresAt: now.Add(time.Minute),
	}).Error)

	pruner := NewPruner(db, WithClock(func() time.Time { return now }))
	deleted, err := pruner.Prune(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, deleted)

	var tokens []models.RateLimitToken
	require.NoError(t, db.Order("window_key ASC").Find(&tokens).Error)
	require.Len(t, tokens, 1)
	require.Equal(t, "live", tokens[0].WindowKey)
}

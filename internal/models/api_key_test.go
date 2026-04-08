package models

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestIsRevokedFalseByDefault(t *testing.T) {
	key := &APIKey{}
	require.False(t, key.IsRevoked())
}

func TestIsRevokedTrueWhenSet(t *testing.T) {
	now := time.Now()
	key := &APIKey{RevokedAt: &now}
	require.True(t, key.IsRevoked())
}

func TestIsExpiredFalseByDefault(t *testing.T) {
	key := &APIKey{}
	require.False(t, key.IsExpired())
}

func TestIsExpiredFalseWhenFuture(t *testing.T) {
	future := time.Now().Add(time.Hour)
	key := &APIKey{ExpiresAt: &future}
	require.False(t, key.IsExpired())
}

func TestIsExpiredTrueWhenPast(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	key := &APIKey{ExpiresAt: &past}
	require.True(t, key.IsExpired())
}

func TestRoleLevelOrdering(t *testing.T) {
	require.Greater(t, RoleLevel(RoleAdmin), RoleLevel(RoleOperator))
	require.Greater(t, RoleLevel(RoleOperator), RoleLevel(RoleRunner))
	require.Greater(t, RoleLevel(RoleRunner), RoleLevel(RoleViewer))
	require.Equal(t, 0, RoleLevel(Role("unknown")))
}

func TestValidRole(t *testing.T) {
	require.True(t, ValidRole("admin"))
	require.True(t, ValidRole("operator"))
	require.True(t, ValidRole("runner"))
	require.True(t, ValidRole("viewer"))
	require.False(t, ValidRole("superuser"))
	require.False(t, ValidRole(""))
}

package auth

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMintAgentSessionKeyScopeAndExpiry(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := NewService(db)

	incidentID := uuid.New()
	before := time.Now().UTC()
	resp, err := svc.MintAgentSessionKey(incidentID, []string{"beta", "alpha", "alpha", " "}, 30*time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Plaintext)
	require.NotNil(t, resp.Key)

	// Runner role — the minimum the agent routes require.
	require.Equal(t, models.RoleRunner, resp.Key.Role)

	// Short-lived: expires within the ttl window.
	require.NotNil(t, resp.Key.ExpiresAt)
	require.True(t, resp.Key.ExpiresAt.After(before))
	require.True(t, resp.Key.ExpiresAt.Before(before.Add(31*time.Minute)))

	// The stored scope carries the agent claim with a normalized (deduped,
	// sorted, whitespace-stripped) allowlist and NO plain job scope.
	claim, err := DecodeAgentClaim(resp.Key.Scope)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, incidentID, claim.IncidentID)
	require.Equal(t, []string{"alpha", "beta"}, claim.Jobs)

	jobs, err := ScopeJobs(resp.Key.Scope)
	require.NoError(t, err)
	require.Empty(t, jobs, "agent key must not carry a plain job scope")
}

func TestMintAgentSessionKeyRequiresIncident(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := NewService(db)

	_, err := svc.MintAgentSessionKey(uuid.Nil, nil, time.Hour)
	require.ErrorIs(t, err, ErrAgentKeyIncidentRequired)
}

func TestDecodeAgentClaimNonAgentScope(t *testing.T) {
	// A plain job scope is not an agent claim.
	claim, err := DecodeAgentClaim([]byte(`{"jobs":["alpha"]}`))
	require.NoError(t, err)
	require.Nil(t, claim)

	// Empty / nil scope is not an agent claim.
	claim, err = DecodeAgentClaim(nil)
	require.NoError(t, err)
	require.Nil(t, claim)

	// An agent claim with a nil incident id is rejected (not a valid binding).
	claim, err = DecodeAgentClaim([]byte(`{"agent":{"incident_id":"00000000-0000-0000-0000-000000000000"}}`))
	require.NoError(t, err)
	require.Nil(t, claim)
}

func TestMintedAgentKeyValidatesAndIsScoped(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := NewService(db)

	incidentID := uuid.New()
	resp, err := svc.MintAgentSessionKey(incidentID, []string{"alpha"}, time.Hour)
	require.NoError(t, err)

	// The minted plaintext validates and round-trips its agent claim.
	key, err := svc.ValidateKey(resp.Plaintext)
	require.NoError(t, err)
	claim, err := DecodeAgentClaim(key.Scope)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, incidentID, claim.IncidentID)
}

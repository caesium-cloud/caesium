package saml

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestReplayStoreRejectsDuplicateUnexpiredAssertion(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewReplayStore(db)
	expiresAt := time.Now().UTC().Add(time.Minute)

	require.NoError(t, store.Record(t.Context(), "issuer", "assertion-1", expiresAt))
	err := store.Record(t.Context(), "issuer", "assertion-1", expiresAt)
	require.ErrorIs(t, err, ErrAssertionReplay)
}

func TestReplayStoreReapsExpiredAssertions(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Now().UTC()
	store := NewReplayStore(db, WithReplayNow(func() time.Time { return now }))
	require.NoError(t, db.Create(&models.SAMLAssertionReplay{
		Issuer:      "issuer",
		AssertionID: "old",
		CreatedAt:   now.Add(-time.Hour),
		ExpiresAt:   now.Add(-time.Minute),
	}).Error)

	n, err := store.Reap(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

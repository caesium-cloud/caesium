package saml

import (
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/mattn/go-sqlite3"
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

func TestIsUniqueConstraintErrorClassifiesNarrowDuplicateErrors(t *testing.T) {
	require.True(t, isUniqueConstraintError(sqlite3.Error{
		Code:         sqlite3.ErrConstraint,
		ExtendedCode: sqlite3.ErrConstraintUnique,
	}))
	require.True(t, isUniqueConstraintError(sqlite3.Error{
		Code:         sqlite3.ErrConstraint,
		ExtendedCode: sqlite3.ErrConstraintPrimaryKey,
	}))
	require.True(t, isUniqueConstraintError(errors.New("UNIQUE constraint failed: saml_assertion_ids.issuer")))
	require.True(t, isUniqueConstraintError(errors.New("pq: duplicate key value violates unique constraint")))
	require.True(t, isUniqueConstraintError(errors.New("mysql: duplicate entry 'issuer/assertion' for key")))
	require.False(t, isUniqueConstraintError(errors.New("duplicate assertion rejected before database insert")))
}

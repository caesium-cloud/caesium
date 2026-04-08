package auth_test

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestAuditLogWrite(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	err := auditor.Log(auth.AuditEntry{
		Actor:        "csk_live_test",
		Action:       auth.ActionKeyCreate,
		ResourceType: "api_key",
		ResourceID:   "abc-123",
		SourceIP:     "1.2.3.4",
		Outcome:      auth.OutcomeSuccess,
		Metadata:     map[string]interface{}{"role": "admin"},
	})
	require.NoError(t, err)

	// Verify it was persisted.
	var count int64
	db.Model(&models.AuditLog{}).Count(&count)
	require.Equal(t, int64(1), count)
}

func TestAuditLogWriteWithoutMetadata(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	err := auditor.Log(auth.AuditEntry{
		Actor:   "csk_live_test",
		Action:  auth.ActionAuthDenied,
		Outcome: auth.OutcomeDenied,
	})
	require.NoError(t, err)
}

func TestAuditQueryAll(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	for i := 0; i < 5; i++ {
		err := auditor.Log(auth.AuditEntry{
			Actor:   "csk_live_test",
			Action:  auth.ActionKeyCreate,
			Outcome: auth.OutcomeSuccess,
		})
		require.NoError(t, err)
	}

	entries, err := auditor.Query(&auth.AuditQueryRequest{})
	require.NoError(t, err)
	require.Len(t, entries, 5)
}

func TestAuditQueryByActor(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	_ = auditor.Log(auth.AuditEntry{Actor: "alice", Action: "a", Outcome: auth.OutcomeSuccess})
	_ = auditor.Log(auth.AuditEntry{Actor: "bob", Action: "b", Outcome: auth.OutcomeSuccess})
	_ = auditor.Log(auth.AuditEntry{Actor: "alice", Action: "c", Outcome: auth.OutcomeSuccess})

	entries, err := auditor.Query(&auth.AuditQueryRequest{Actor: "alice"})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, e := range entries {
		require.Equal(t, "alice", e.Actor)
	}
}

func TestAuditQueryByAction(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: auth.ActionKeyCreate, Outcome: auth.OutcomeSuccess})
	_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: auth.ActionKeyRevoke, Outcome: auth.OutcomeSuccess})
	_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: auth.ActionKeyCreate, Outcome: auth.OutcomeSuccess})

	entries, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionKeyRevoke})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, auth.ActionKeyRevoke, entries[0].Action)
}

func TestAuditQueryBySince(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: "old", Outcome: auth.OutcomeSuccess})

	// Wait a bit so timestamps differ.
	time.Sleep(50 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(50 * time.Millisecond)

	_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: "new", Outcome: auth.OutcomeSuccess})

	entries, err := auditor.Query(&auth.AuditQueryRequest{Since: &cutoff})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "new", entries[0].Action)
}

func TestAuditQueryLimit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	for i := 0; i < 10; i++ {
		_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: "x", Outcome: auth.OutcomeSuccess})
	}

	entries, err := auditor.Query(&auth.AuditQueryRequest{Limit: 3})
	require.NoError(t, err)
	require.Len(t, entries, 3)
}

func TestAuditQueryDefaultLimit(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	for i := 0; i < 150; i++ {
		_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: "x", Outcome: auth.OutcomeSuccess})
	}

	// Default limit is 100.
	entries, err := auditor.Query(&auth.AuditQueryRequest{})
	require.NoError(t, err)
	require.Len(t, entries, 100)
}

func TestAuditQueryOffset(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	for i := 0; i < 5; i++ {
		_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: "x", Outcome: auth.OutcomeSuccess})
	}

	entries, err := auditor.Query(&auth.AuditQueryRequest{Offset: 3, Limit: 10})
	require.NoError(t, err)
	require.Len(t, entries, 2) // 5 total, skip 3 = 2 remaining
}

func TestAuditQueryOrderDescending(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: "first", Outcome: auth.OutcomeSuccess})
	time.Sleep(10 * time.Millisecond)
	_ = auditor.Log(auth.AuditEntry{Actor: "a", Action: "second", Outcome: auth.OutcomeSuccess})

	entries, err := auditor.Query(&auth.AuditQueryRequest{})
	require.NoError(t, err)
	require.Len(t, entries, 2)
	// Most recent first.
	require.Equal(t, "second", entries[0].Action)
	require.Equal(t, "first", entries[1].Action)
}

package cache

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return NewStore(db)
}

func sampleEntry() *Entry {
	return &Entry{
		Hash:             "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
		JobID:            uuid.New(),
		TaskName:         "my-task",
		Result:           "success",
		Output:           map[string]string{"key": "value"},
		BranchSelections: []string{"branch-a"},
		RunID:            uuid.New(),
		TaskRunID:        uuid.New(),
		CreatedAt:        time.Now().UTC(),
		ExpiresAt:        nil,
	}
}

func TestPutThenGet(t *testing.T) {
	store := newTestStore(t)
	entry := sampleEntry()

	require.NoError(t, store.Put(entry))

	got, found, err := store.Get(entry.Hash)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, entry.Hash, got.Hash)
	assert.Equal(t, entry.JobID, got.JobID)
	assert.Equal(t, entry.TaskName, got.TaskName)
	assert.Equal(t, entry.Result, got.Result)
	assert.Equal(t, entry.Output, got.Output)
	assert.Equal(t, entry.BranchSelections, got.BranchSelections)
	assert.Equal(t, entry.RunID, got.RunID)
	assert.Equal(t, entry.TaskRunID, got.TaskRunID)
}

func TestGetNonexistent(t *testing.T) {
	store := newTestStore(t)

	got, found, err := store.Get("nonexistent-hash")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, got)
}

func TestExpiredEntryReturnsFalse(t *testing.T) {
	store := newTestStore(t)
	entry := sampleEntry()
	expired := time.Now().UTC().Add(-1 * time.Hour)
	entry.ExpiresAt = &expired

	require.NoError(t, store.Put(entry))

	got, found, err := store.Get(entry.Hash)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, got)
}

func TestNonExpiredEntryReturnsTrue(t *testing.T) {
	store := newTestStore(t)
	entry := sampleEntry()
	future := time.Now().UTC().Add(1 * time.Hour)
	entry.ExpiresAt = &future

	require.NoError(t, store.Put(entry))

	got, found, err := store.Get(entry.Hash)
	require.NoError(t, err)
	assert.True(t, found)
	assert.NotNil(t, got)
}

func TestInvalidateRemovesSpecificTask(t *testing.T) {
	store := newTestStore(t)
	jobID := uuid.New()

	entry1 := sampleEntry()
	entry1.JobID = jobID
	entry1.TaskName = "task-a"
	entry1.Hash = "hash1"
	require.NoError(t, store.Put(entry1))

	entry2 := sampleEntry()
	entry2.JobID = jobID
	entry2.TaskName = "task-b"
	entry2.Hash = "hash2"
	require.NoError(t, store.Put(entry2))

	require.NoError(t, store.Invalidate(jobID, "task-a"))

	_, found, err := store.Get("hash1")
	require.NoError(t, err)
	assert.False(t, found)

	_, found, err = store.Get("hash2")
	require.NoError(t, err)
	assert.True(t, found)
}

func TestInvalidateJobRemovesAll(t *testing.T) {
	store := newTestStore(t)
	jobID := uuid.New()

	entry1 := sampleEntry()
	entry1.JobID = jobID
	entry1.TaskName = "task-a"
	entry1.Hash = "hash1"
	require.NoError(t, store.Put(entry1))

	entry2 := sampleEntry()
	entry2.JobID = jobID
	entry2.TaskName = "task-b"
	entry2.Hash = "hash2"
	require.NoError(t, store.Put(entry2))

	require.NoError(t, store.InvalidateJob(jobID))

	_, found, err := store.Get("hash1")
	require.NoError(t, err)
	assert.False(t, found)

	_, found, err = store.Get("hash2")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestListByJobReturnsNonExpired(t *testing.T) {
	store := newTestStore(t)
	jobID := uuid.New()

	// Non-expired entry
	entry1 := sampleEntry()
	entry1.JobID = jobID
	entry1.Hash = "hash1"
	require.NoError(t, store.Put(entry1))

	// Expired entry
	entry2 := sampleEntry()
	entry2.JobID = jobID
	entry2.Hash = "hash2"
	expired := time.Now().UTC().Add(-1 * time.Hour)
	entry2.ExpiresAt = &expired
	require.NoError(t, store.Put(entry2))

	entries, err := store.ListByJob(jobID)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "hash1", entries[0].Hash)
}

func TestPruneRemovesExpiredEntries(t *testing.T) {
	store := newTestStore(t)

	// Expired entry
	entry1 := sampleEntry()
	entry1.Hash = "hash-expired"
	expired := time.Now().UTC().Add(-1 * time.Hour)
	entry1.ExpiresAt = &expired
	require.NoError(t, store.Put(entry1))

	// Non-expired entry
	entry2 := sampleEntry()
	entry2.Hash = "hash-valid"
	future := time.Now().UTC().Add(1 * time.Hour)
	entry2.ExpiresAt = &future
	require.NoError(t, store.Put(entry2))

	// No expiry entry
	entry3 := sampleEntry()
	entry3.Hash = "hash-no-expiry"
	entry3.ExpiresAt = nil
	require.NoError(t, store.Put(entry3))

	count, err := store.Prune()
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Verify expired is gone
	_, found, err := store.Get("hash-expired")
	require.NoError(t, err)
	assert.False(t, found)

	// Verify others remain
	_, found, err = store.Get("hash-valid")
	require.NoError(t, err)
	assert.True(t, found)

	_, found, err = store.Get("hash-no-expiry")
	require.NoError(t, err)
	assert.True(t, found)
}

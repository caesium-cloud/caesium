package cache

import (
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func samplePartitions() []pkgtask.Partition {
	return []pkgtask.Partition{
		{Key: "a", Fingerprint: "sha256:" + strings.Repeat("a1", 32)},
		{
			Key:         "b",
			Fingerprint: "sha256:" + strings.Repeat("b2", 32),
			DependsOn:   []string{"a"},
			Attributes:  map[string]string{"rows": "128", "tier": "gold"},
		},
	}
}

// TestCacheEntryRoundTripPreservesPartitions pins the contract that makes a
// CACHED producer able to expand its group: without the partition list on the
// entry, a cache hit on the producer yields no partitions and the fan-out group
// collapses to the single template row.  Every structured field must survive —
// a fingerprint or dependsOn lost in the round trip changes the group's shape
// or its per-unit cache identity.
func TestCacheEntryRoundTripPreservesPartitions(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	want := samplePartitions()
	entry := &Entry{
		Hash:       "sha256:round-trip",
		JobID:      uuid.New(),
		TaskName:   "list",
		Result:     "success",
		RunID:      uuid.New(),
		TaskRunID:  uuid.New(),
		Partitions: want,
		CreatedAt:  time.Now().UTC().Truncate(time.Second),
	}
	require.NoError(t, store.Put(entry))

	got, found, err := store.Get(entry.Hash)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, got.Partitions, "the full normalized partition list must round-trip")
}

// TestCacheEntryWithoutPartitionsStaysNil asserts the column is omit-when-empty:
// every pre-existing entry (and every non-producer task) reads back nil, not an
// empty slice that would look like "expanded to zero partitions" and trip
// onEmpty.
func TestCacheEntryWithoutPartitionsStaysNil(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	entry := &Entry{
		Hash: "sha256:no-partitions", JobID: uuid.New(), TaskName: "plain",
		Result: "success", RunID: uuid.New(), TaskRunID: uuid.New(),
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, store.Put(entry))

	got, found, err := store.Get(entry.Hash)
	require.NoError(t, err)
	require.True(t, found)
	require.Nil(t, got.Partitions)

	var model models.TaskCache
	require.NoError(t, db.First(&model, "hash = ?", entry.Hash).Error)
	require.Empty(t, model.Partitions, "an unfanned task must not write a partitions column")
}

// TestCacheEntryRecordedEmptyPartitionsStaysNonNilEmpty is the flip side of
// TestCacheEntryWithoutPartitionsStaysNil: a producer that genuinely ran and
// emitted zero partitions must round-trip as a non-nil, zero-length slice —
// NOT collapse to the same nil a never-recorded entry reads back as. F7
// (dynamic-fanout closeout) needs this distinction to tell "a pre-fan-out
// cache entry, which never recorded a list" (nil — must be treated as a MISS
// for a producer with a fan-out consumer, see run.Store.HasFanOutSuccessor)
// apart from "recorded, and it was empty" (non-nil — a legitimate onEmpty).
//
// This is the store layer's isolated proof of the mechanism only. The
// non-nil-empty value asserted here (`[]pkgtask.Partition{}`) is exactly what
// pkg/task's partitionAccumulator.finish now returns for an explicit
// `##caesium::partitions []` (see partition_test.go's
// TestParseMarkers_EmptyPartitionsArrayStillValid) and what both cache-hit
// writer sites assign straight onto cache.Entry.Partitions — the end-to-end
// pipeline proof (producer executes once, second run is a real cache hit,
// consumer still takes onEmpty) lives in
// internal/job/fanout_local_test.go's
// TestFanOutLocalEmptyProducerCacheHitTakesOnEmptyWithoutRerunning and
// internal/worker/fanout_worker_test.go's
// TestWorkerEmptyProducerCacheHitTakesOnEmptyWithoutRerunning.
func TestCacheEntryRecordedEmptyPartitionsStaysNonNilEmpty(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	entry := &Entry{
		Hash: "sha256:recorded-empty", JobID: uuid.New(), TaskName: "list",
		Result: "success", RunID: uuid.New(), TaskRunID: uuid.New(),
		Partitions: []pkgtask.Partition{},
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, store.Put(entry))

	got, found, err := store.Get(entry.Hash)
	require.NoError(t, err)
	require.True(t, found)
	require.NotNil(t, got.Partitions, "a recorded-but-empty partition list must NOT read back as nil")
	require.Empty(t, got.Partitions)

	var model models.TaskCache
	require.NoError(t, db.First(&model, "hash = ?", entry.Hash).Error)
	require.NotEmpty(t, model.Partitions, "the column must carry a recorded marker (\"[]\"), not be left unset")
}

// TestCachePutUpsertRefreshesPartitions pins the easy-to-miss half of Put: the
// OnConflict DoUpdates column list.  A hash re-Put with a new partition list
// must overwrite it; omitting the column from DoUpdates silently keeps the old
// list, so a producer whose partitions changed would expand the previous run's
// group.
func TestCachePutUpsertRefreshesPartitions(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	hash := "sha256:upsert"
	base := &Entry{
		Hash: hash, JobID: uuid.New(), TaskName: "list", Result: "success",
		RunID: uuid.New(), TaskRunID: uuid.New(),
		Partitions: []pkgtask.Partition{{Key: "old"}},
		CreatedAt:  time.Now().UTC(),
	}
	require.NoError(t, store.Put(base))

	updated := *base
	updated.RunID = uuid.New()
	updated.Partitions = samplePartitions()
	require.NoError(t, store.Put(&updated))

	got, found, err := store.Get(hash)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, samplePartitions(), got.Partitions, "an upsert must refresh the partition list")
}

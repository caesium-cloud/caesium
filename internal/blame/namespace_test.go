package blame

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Blame under a history scope (identity-and-access B7) is computed over the
// snapshots the caller may read only.

func setSnapshotNamespace(t *testing.T, db *gorm.DB, snap *models.DagSnapshot, namespace *string) {
	t.Helper()
	require.NoError(t, db.Model(&models.DagSnapshot{}).Where("id = ?", snap.ID).Update("namespace", namespace).Error)
}

func nsPtr(v string) *string { return &v }

// seedMovedJob seeds marketing (c1) → finance (c2, same topology) →
// marketing (c3, adds publish).
func seedMovedJob(t *testing.T, db *gorm.DB) (uuid.UUID, [3]*models.DagSnapshot) {
	t.Helper()
	jobID := seedBlameJob(t, db)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	two := []models.DagSnapshotTask{task("extract", "repo/extract:v1"), task("load", "repo/load:v1")}
	m1 := seedBlameSnapshot(t, db, jobID, "hash-1", "c1", base, two,
		[]models.DagSnapshotEdge{edge("extract", "load", "c1")})
	f2 := seedBlameSnapshot(t, db, jobID, "hash-1", "c2", base.Add(time.Minute), two,
		[]models.DagSnapshotEdge{edge("extract", "load", "c2")})
	m3 := seedBlameSnapshot(t, db, jobID, "hash-2", "c3", base.Add(2*time.Minute),
		append(append([]models.DagSnapshotTask{}, two...), task("publish", "repo/publish:v1")),
		[]models.DagSnapshotEdge{edge("extract", "load", "c3"), edge("load", "publish", "c3")})
	setSnapshotNamespace(t, db, m1, nsPtr("marketing"))
	setSnapshotNamespace(t, db, f2, nsPtr("finance"))
	setSnapshotNamespace(t, db, m3, nsPtr("marketing"))
	return jobID, [3]*models.DagSnapshot{m1, f2, m3}
}

func scoped(allowed ...string) context.Context {
	return authz.WithAllowedNamespaces(context.Background(), allowed)
}

func TestBlameConfinedToReadableSnapshots(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	jobID, snaps := seedMovedJob(t, db)
	m1, f2, m3 := snaps[0], snaps[1], snaps[2]

	// The marketing owner never sees the finance snapshot: its elements keep
	// their marketing introductions.
	mkt, err := New(db).Blame(scoped("marketing"), jobID, Options{})
	require.NoError(t, err)
	requireTaskIntro(t, mkt, "extract", "repo/extract:v1", "c1", m1.ID)
	requireTaskIntro(t, mkt, "publish", "repo/publish:v1", "c3", m3.ID)
	requireEdgeIntro(t, mkt, "extract", "load", "c1", m1.ID, "c1")
	requireEdgeIntro(t, mkt, "load", "publish", "c3", m3.ID, "c3")
	requireNoSnapshot(t, mkt, f2.ID)

	// The finance owner's history starts at the move: everything is
	// attributed to its first readable snapshot, nothing to marketing's.
	fin, err := New(db).Blame(scoped("finance"), jobID, Options{})
	require.NoError(t, err)
	require.Len(t, fin.Tasks, 2)
	requireTaskIntro(t, fin, "extract", "repo/extract:v1", "c2", f2.ID)
	requireTaskIntro(t, fin, "load", "repo/load:v1", "c2", f2.ID)
	requireEdgeIntro(t, fin, "extract", "load", "c2", f2.ID, "c2")
	requireNoSnapshot(t, fin, m1.ID, m3.ID)

	// A commit only an unreadable snapshot carries answers like a missing one,
	// as the from and as the to bound.
	for _, tc := range []struct {
		ctx  context.Context
		opts Options
	}{
		{scoped("marketing"), Options{FromCommit: "c2"}},
		{scoped("marketing"), Options{ToCommit: "c2"}},
		{scoped("finance"), Options{FromCommit: "c1"}},
		{scoped("finance"), Options{ToCommit: "c3"}},
	} {
		_, err := New(db).Blame(tc.ctx, jobID, tc.opts)
		require.True(t, errors.Is(err, ErrCommitNotFound), "%+v", tc.opts)
		_, missing := New(db).Blame(tc.ctx, jobID, Options{FromCommit: "nope", ToCommit: tc.opts.ToCommit})
		require.True(t, errors.Is(missing, ErrCommitNotFound))
	}
}

// A caller with no readable snapshot (another namespace's reader, or only
// legacy rows) gets the empty blame an unrestricted caller gets for a job with
// no snapshots, and any commit bound it names, whether an unreadable
// snapshot carries it or none does, is not found. The unrestricted
// no-snapshot answer is unchanged: an empty blame, bounds and all.
func TestBlameEmptyReadableHistory(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	jobID, snaps := seedMovedJob(t, db)
	bare := seedBlameJob(t, db)

	want, err := New(db).Blame(context.Background(), bare, Options{})
	require.NoError(t, err)
	want.JobID = jobID

	check := func(ctx context.Context) {
		t.Helper()
		got, err := New(db).Blame(ctx, jobID, Options{})
		require.NoError(t, err)
		require.Equal(t, mustJSON(t, want), mustJSON(t, got))
		for _, opts := range []Options{{FromCommit: "c1"}, {ToCommit: "c3"}, {FromCommit: "nope"}, {ToCommit: "nope"}, {FromCommit: "c1", ToCommit: "c3"}} {
			_, err := New(db).Blame(ctx, jobID, opts)
			require.True(t, errors.Is(err, ErrCommitNotFound), "%+v: %v", opts, err)
		}
	}
	check(scoped("ops"))

	// Legacy rows are withheld even from a reader of every namespace named.
	for _, snap := range snaps {
		setSnapshotNamespace(t, db, snap, nil)
	}
	check(scoped("default", "finance", "marketing"))

	for _, opts := range []Options{{FromCommit: "c1"}, {ToCommit: "nope"}} {
		got, err := New(db).Blame(context.Background(), bare, opts)
		require.NoError(t, err, "unrestricted, no snapshots: unchanged")
		require.Empty(t, got.Tasks)
	}
}

// Unrestricted blame is byte-identical whether or not snapshots record a
// namespace (the pre-B7 state is every row NULL).
func TestBlameUnrestrictedIgnoresNamespaces(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	jobID, snaps := seedMovedJob(t, db)

	for _, opts := range []Options{{}, {FromCommit: "c2"}, {ToCommit: "c2"}, {Task: "publish"}} {
		withNamespaces, err := New(db).Blame(context.Background(), jobID, opts)
		require.NoError(t, err)
		for _, snap := range snaps {
			setSnapshotNamespace(t, db, snap, nil)
		}
		legacy, err := New(db).Blame(context.Background(), jobID, opts)
		require.NoError(t, err)
		require.Equal(t, mustJSON(t, legacy), mustJSON(t, withNamespaces), "%+v", opts)
		setSnapshotNamespace(t, db, snaps[0], nsPtr("marketing"))
		setSnapshotNamespace(t, db, snaps[1], nsPtr("finance"))
		setSnapshotNamespace(t, db, snaps[2], nsPtr("marketing"))
	}
}

func requireNoSnapshot(t *testing.T, result *Result, ids ...uuid.UUID) {
	t.Helper()
	body := mustJSON(t, result)
	for _, id := range ids {
		require.NotContains(t, body, id.String())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

package jobdef

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Namespace-aware DAG snapshots (identity-and-access B7).

func (s *ImporterTestSuite) applySampleIn(namespace, commit string, mutate func(string) string) *models.Job {
	manifest := testutil.SampleJob
	if mutate != nil {
		manifest = mutate(manifest)
	}
	def, err := schema.Parse([]byte(manifest))
	s.Require().NoError(err)
	def.Metadata.Namespace = namespace
	job, err := s.importer.ApplyWithOptions(context.Background(), def, &ApplyOptions{
		Provenance: &Provenance{SourceID: "b7", Repo: "https://example.com/b7.git", Ref: "main", Commit: commit, Path: "b7.job.yaml"},
	})
	s.Require().NoError(err)
	return job
}

func (s *ImporterTestSuite) snapshotsOf(jobID uuid.UUID) []models.DagSnapshot {
	var snaps []models.DagSnapshot
	s.Require().NoError(s.db.Where("job_id = ?", jobID).Order("created_at asc").Find(&snaps).Error)
	return snaps
}

func snapshotNamespace(snap models.DagSnapshot) string {
	if snap.Namespace == nil {
		return "<nil>"
	}
	return *snap.Namespace
}

// A move that changes nothing but the namespace writes a fresh snapshot owned
// by the new namespace, and moving back writes a third: each owner's topology
// history stays with it.
func (s *ImporterTestSuite) TestDagSnapshotMoveWithoutTopologyChangeWritesNewRow() {
	job := s.applySampleIn("marketing", "c1", nil)
	s.applySampleIn("finance", "c2", nil)
	snaps := s.snapshotsOf(job.ID)
	s.Require().Len(snaps, 2, "a pure move must write a snapshot in the new namespace")
	s.Equal(snaps[0].ContentHash, snaps[1].ContentHash)
	s.Equal("marketing", snapshotNamespace(snaps[0]))
	s.Equal("finance", snapshotNamespace(snaps[1]))
	s.Equal("c2", snaps[1].GitCommit)

	s.applySampleIn("marketing", "c3", nil)
	snaps = s.snapshotsOf(job.ID)
	s.Require().Len(snaps, 3, "moving back writes a fresh marketing snapshot")
	s.Equal("marketing", snapshotNamespace(snaps[2]))
}

// Re-applying in the same namespace still dedupes, and the default namespace
// is stamped explicitly.
func (s *ImporterTestSuite) TestDagSnapshotDedupesWithinNamespace() {
	job := s.applySampleIn("", "c1", nil)
	s.applySampleIn("", "c2", func(m string) string { return strings.Replace(m, "owner: etl", "owner: analytics", 1) })
	snaps := s.snapshotsOf(job.ID)
	s.Require().Len(snaps, 1)
	s.Equal(models.DefaultNamespace, snapshotNamespace(snaps[0]))
}

// A legacy snapshot (no namespace) is superseded by one namespaced row on the
// job's next apply, which then dedupes; the legacy row is never back-filled.
func (s *ImporterTestSuite) TestDagSnapshotLegacyRowSupersededOnce() {
	job := s.applySampleIn("marketing", "c1", nil)
	s.Require().NoError(s.db.Model(&models.DagSnapshot{}).Where("job_id = ?", job.ID).Update("namespace", gorm.Expr("NULL")).Error)

	s.applySampleIn("marketing", "c2", nil)
	s.applySampleIn("marketing", "c3", nil)
	snaps := s.snapshotsOf(job.ID)
	s.Require().Len(snaps, 2)
	s.Equal("<nil>", snapshotNamespace(snaps[0]), "the legacy row is not back-filled")
	s.Equal("marketing", snapshotNamespace(snaps[1]))
	s.Equal(snaps[0].ContentHash, snaps[1].ContentHash)
}

func seedNamespacedSnapshot(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace *string, hash, commit string, at time.Time) *models.DagSnapshot {
	t.Helper()
	snap := seedSnapshot(t, db, jobID, hash, commit, at)
	require.NoError(t, db.Model(snap).Update("namespace", namespace).Error)
	snap.Namespace = namespace
	return snap
}

func strp(v string) *string { return &v }

// Every snapshot read is narrowed under a history scope: foreign rows and
// legacy rows (NULL or empty namespace) are withheld from every confined
// caller, a "default" reader included; unrestricted reads see every row in
// the same order as before.
func TestSnapshotQueryNamespaceScope(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	jobID := seedJob(t, db)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	legacy := seedNamespacedSnapshot(t, db, jobID, nil, "h0", "c0", base)
	empty := seedNamespacedSnapshot(t, db, jobID, strp(""), "h0", "c0e", base.Add(time.Minute))
	mkt := seedNamespacedSnapshot(t, db, jobID, strp("marketing"), "h1", "c1", base.Add(2*time.Minute))
	fin := seedNamespacedSnapshot(t, db, jobID, strp("finance"), "h1", "c2", base.Add(3*time.Minute))
	def := seedNamespacedSnapshot(t, db, jobID, strp("default"), "h2", "c3", base.Add(4*time.Minute))

	ids := func(snaps []models.DagSnapshot) []uuid.UUID {
		out := make([]uuid.UUID, 0, len(snaps))
		for _, snap := range snaps {
			out = append(out, snap.ID)
		}
		return out
	}

	// Unrestricted: unchanged.
	all, err := NewSnapshotQuery(context.Background(), db).List(jobID)
	require.NoError(t, err)
	var raw []models.DagSnapshot
	require.NoError(t, db.Where("job_id = ?", jobID).Order("created_at desc").Find(&raw).Error)
	require.Equal(t, ids(raw), ids(all))
	require.Equal(t, []uuid.UUID{def.ID, fin.ID, mkt.ID, empty.ID, legacy.ID}, ids(all))

	cases := []struct {
		allowed []string
		list    []uuid.UUID
		latest  *models.DagSnapshot
	}{
		{[]string{"marketing"}, []uuid.UUID{mkt.ID}, mkt},
		{[]string{"finance"}, []uuid.UUID{fin.ID}, fin},
		{[]string{"default"}, []uuid.UUID{def.ID}, def},
		{[]string{"marketing", "finance"}, []uuid.UUID{fin.ID, mkt.ID}, fin},
		{[]string{"ops"}, []uuid.UUID{}, nil},
		{[]string{}, []uuid.UUID{}, nil},
	}
	for _, tc := range cases {
		ctx := authz.WithAllowedNamespaces(context.Background(), tc.allowed)
		q := NewSnapshotQuery(ctx, db)
		list, err := q.List(jobID)
		require.NoError(t, err)
		require.Equal(t, tc.list, ids(list), "%v", tc.allowed)
		require.True(t, authz.Filtered(ctx), "a scoped read marks the request filtered")

		latest, err := q.Latest(jobID)
		if tc.latest == nil {
			require.True(t, errors.Is(err, gorm.ErrRecordNotFound), "%v", tc.allowed)
		} else {
			require.NoError(t, err)
			require.Equal(t, tc.latest.ID, latest.ID, "%v", tc.allowed)
		}

		// A legacy-only hash and commit answer like missing ones.
		_, err = q.ByContentHash(jobID, "h0")
		require.True(t, errors.Is(err, gorm.ErrRecordNotFound), "%v", tc.allowed)
		_, err = q.ByGitCommit(jobID, "c0")
		require.True(t, errors.Is(err, gorm.ErrRecordNotFound), "%v", tc.allowed)
	}

	// A hash shared across namespaces resolves to the caller's own row; a
	// commit only the other owner carries is not found.
	mktQ := NewSnapshotQuery(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), db)
	got, err := mktQ.ByContentHash(jobID, "h1")
	require.NoError(t, err)
	require.Equal(t, mkt.ID, got.ID)
	_, err = mktQ.ByGitCommit(jobID, "c2")
	require.True(t, errors.Is(err, gorm.ErrRecordNotFound))
	got, err = mktQ.ByGitCommit(jobID, "c1")
	require.NoError(t, err)
	require.Equal(t, mkt.ID, got.ID)

	// Unrestricted lookups still reach legacy rows.
	unrestricted := NewSnapshotQuery(context.Background(), db)
	got, err = unrestricted.ByGitCommit(jobID, "c0")
	require.NoError(t, err)
	require.Equal(t, legacy.ID, got.ID)
	got, err = unrestricted.ByContentHash(jobID, "h1")
	require.NoError(t, err)
	require.Equal(t, fin.ID, got.ID)
}

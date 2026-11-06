package blame

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func behaviorTask(name, image, behavior string) models.DagSnapshotTask {
	t := task(name, image)
	if behavior != "" {
		t.Behavior = json.RawMessage(behavior)
	}
	return t
}

func setSequence(t *testing.T, db *gorm.DB, snap *models.DagSnapshot, seq int64) {
	t.Helper()
	require.NoError(t, db.Model(&models.DagSnapshot{}).Where("id = ?", snap.ID).UpdateColumn("sequence", seq).Error)
}

func seedSnapshotWithID(t *testing.T, db *gorm.DB, id, jobID uuid.UUID, commit, image string, at time.Time, seq int64) *models.DagSnapshot {
	t.Helper()
	snap := seedBlameSnapshot(t, db, jobID, "hash-"+commit, commit, at,
		[]models.DagSnapshotTask{task("extract", image)}, nil)
	require.NoError(t, db.Model(&models.DagSnapshot{}).Where("id = ?", snap.ID).Updates(map[string]any{"id": id, "sequence": seq}).Error)
	snap.ID = id
	return snap
}

// TestBlameSequenceBreaksCreatedAtCollision seeds two snapshots with an
// IDENTICAL CreatedAt whose IDs sort opposite to their insertion order. Before
// the sequence column the ID tiebreak would blame the wrong snapshot; the
// sequence must win, whichever way the IDs fall.
func TestBlameSequenceBreaksCreatedAtCollision(t *testing.T) {
	low := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	high := uuid.MustParse("ffffffff-0000-4000-8000-000000000001")
	for _, tc := range []struct {
		name              string
		firstID, secondID uuid.UUID
	}{
		{"later insertion has the lower id", high, low},
		{"later insertion has the higher id", low, high},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			defer testutil.CloseDB(db)

			jobID := seedBlameJob(t, db)
			at := time.Now().UTC()
			seedSnapshotWithID(t, db, tc.firstID, jobID, "commit-1", "repo/extract:v1", at, 1)
			second := seedSnapshotWithID(t, db, tc.secondID, jobID, "commit-2", "repo/extract:v2", at, 2)

			result, err := New(db).Blame(context.Background(), jobID, Options{})
			require.NoError(t, err)
			require.Len(t, result.Tasks, 1)
			require.Equal(t, "repo/extract:v2", result.Tasks[0].Element.Image, "the later insertion is the current descriptor")
			require.Equal(t, "commit-2", result.Tasks[0].IntroducingCommit)
			require.Equal(t, second.ID, result.Tasks[0].SnapshotID)
		})
	}
}

func TestBlameAttributesBehaviorChanges(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedBlameJob(t, db)
	now := time.Now().UTC()
	first := seedBlameSnapshot(t, db, jobID, "h1", "commit-1", now,
		[]models.DagSnapshotTask{
			behaviorTask("extract", "img", `{"retries":1}`),
			behaviorTask("load", "img", `{"env":{"K":"sha256:aa"}}`),
		}, nil)
	second := seedBlameSnapshot(t, db, jobID, "h2", "commit-2", now.Add(time.Minute),
		[]models.DagSnapshotTask{
			behaviorTask("extract", "img", `{"retries":3}`), // changed
			// Same content, different key order and whitespace: not a change.
			behaviorTask("load", "img", `{ "env" : {"K":"sha256:aa"} }`),
		}, nil)
	author, ref := "alice", "refs/heads/main"
	require.NoError(t, db.Model(&models.DagSnapshot{}).Where("id = ?", second.ID).Updates(map[string]any{"author": author, "ref": ref}).Error)

	result, err := New(db).Blame(context.Background(), jobID, Options{})
	require.NoError(t, err)
	require.Equal(t, CoverageStepSpec, result.Coverage)
	byName := map[string]TaskAttribution{}
	for _, tk := range result.Tasks {
		byName[tk.Element.Name] = tk
	}
	require.Equal(t, "commit-2", byName["extract"].IntroducingCommit, "a retries edit is attributed")
	require.Equal(t, second.ID, byName["extract"].SnapshotID)
	require.Equal(t, "alice", byName["extract"].Author)
	require.Equal(t, "refs/heads/main", byName["extract"].Ref)
	require.JSONEq(t, `{"retries":3}`, string(byName["extract"].Element.Behavior))
	require.Equal(t, "commit-1", byName["load"].IntroducingCommit, "a reformatted but identical behavior is not a change")
	require.Equal(t, first.ID, byName["load"].SnapshotID)
	require.Empty(t, byName["load"].Author, "historical rows show no author")
}

// TestBlameLegacyDescriptorBoundaryIsNotAChange pins the upgrade boundary: a
// snapshot written before behavior was recorded (nil Behavior) followed by the
// first post-upgrade snapshot (same topology, behavior now present) must not
// move attribution; a later real behavior edit must.
func TestBlameLegacyDescriptorBoundaryIsNotAChange(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedBlameJob(t, db)
	now := time.Now().UTC()
	legacy := seedBlameSnapshot(t, db, jobID, "h1", "commit-1", now,
		[]models.DagSnapshotTask{task("extract", "img"), task("load", "img")}, nil)
	upgraded := seedBlameSnapshot(t, db, jobID, "h2", "commit-1", now.Add(time.Minute),
		[]models.DagSnapshotTask{
			behaviorTask("extract", "img", `{"retries":1}`),
			behaviorTask("load", "img", `{}`),
		}, nil)
	edited := seedBlameSnapshot(t, db, jobID, "h3", "commit-2", now.Add(2*time.Minute),
		[]models.DagSnapshotTask{
			behaviorTask("extract", "img", `{"retries":2}`),
			behaviorTask("load", "img", `{}`),
		}, nil)
	setSequence(t, db, legacy, 1)
	setSequence(t, db, upgraded, 2)
	setSequence(t, db, edited, 3)

	atUpgrade, err := New(db).Blame(context.Background(), jobID, Options{ToCommit: "commit-1"})
	require.NoError(t, err)
	for _, tk := range atUpgrade.Tasks {
		require.Equal(t, legacy.ID, tk.SnapshotID, "the upgrade snapshot must not re-attribute %s", tk.Element.Name)
	}

	latest, err := New(db).Blame(context.Background(), jobID, Options{})
	require.NoError(t, err)
	got := map[string]uuid.UUID{}
	for _, tk := range latest.Tasks {
		got[tk.Element.Name] = tk.SnapshotID
	}
	require.Equal(t, edited.ID, got["extract"], "a real edit after the boundary is attributed")
	require.Equal(t, legacy.ID, got["load"], "an untouched task stays with its original snapshot")
}

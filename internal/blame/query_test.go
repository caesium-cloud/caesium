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
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestBlameAttributesOrdinaryIntroductions(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedBlameJob(t, db)
	now := time.Now().UTC()
	first := seedBlameSnapshot(t, db, jobID, "hash-1", "commit-1", now,
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{
			edge("extract", "load", "commit-1"),
		},
	)
	second := seedBlameSnapshot(t, db, jobID, "hash-2", "commit-2", now.Add(time.Minute),
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1"),
			task("load", "repo/load:v1"),
			task("publish", "repo/publish:v1"),
		},
		[]models.DagSnapshotEdge{
			edge("extract", "load", "commit-2"),
			edge("load", "publish", "commit-2"),
		},
	)

	result, err := New(db).Blame(context.Background(), jobID, Options{})
	require.NoError(t, err)
	require.Equal(t, CoverageTopologyImageCommand, result.Coverage)
	require.Len(t, result.Tasks, 3)
	require.Len(t, result.Edges, 2)

	requireTaskIntro(t, result, "extract", "repo/extract:v1", "commit-1", first.ID)
	requireTaskIntro(t, result, "load", "repo/load:v1", "commit-1", first.ID)
	requireTaskIntro(t, result, "publish", "repo/publish:v1", "commit-2", second.ID)
	requireEdgeIntro(t, result, "extract", "load", "commit-1", first.ID, "commit-1")
	requireEdgeIntro(t, result, "load", "publish", "commit-2", second.ID, "commit-2")
}

func TestBlameKeysTasksByFullDescriptor(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedBlameJob(t, db)
	now := time.Now().UTC()
	seedBlameSnapshot(t, db, jobID, "hash-1", "commit-1", now,
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1", "sh", "-c", "echo v1"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{edge("extract", "load", "commit-1")},
	)
	mutating := seedBlameSnapshot(t, db, jobID, "hash-2", "commit-2", now.Add(time.Minute),
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v2", "sh", "-c", "echo v2"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{edge("extract", "load", "commit-2")},
	)

	result, err := New(db).Blame(context.Background(), jobID, Options{Task: "extract"})
	require.NoError(t, err)
	require.Equal(t, CoverageTopologyImageCommand, result.Coverage)
	require.Len(t, result.Tasks, 1)
	requireTaskIntro(t, result, "extract", "repo/extract:v2", "commit-2", mutating.ID)
}

func TestBlameAttributesDeleteAndReaddToReaddingSnapshot(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedBlameJob(t, db)
	now := time.Now().UTC()
	firstSnap := seedBlameSnapshot(t, db, jobID, "hash-1", "commit-1", now,
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{edge("extract", "load", "commit-1")},
	)
	seedBlameSnapshot(t, db, jobID, "hash-2", "commit-2", now.Add(time.Minute),
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1"),
		},
		nil,
	)
	readd := seedBlameSnapshot(t, db, jobID, "hash-3", "commit-3", now.Add(2*time.Minute),
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{edge("extract", "load", "commit-3")},
	)

	result, err := New(db).Blame(context.Background(), jobID, Options{})
	require.NoError(t, err)
	require.Equal(t, CoverageTopologyImageCommand, result.Coverage)
	require.Len(t, result.Tasks, 2)
	require.Len(t, result.Edges, 1)
	// The continuously-present "extract" survivor stays attributed to its
	// original introduction; only the deleted-and-readded "load"/edge move.
	requireTaskIntro(t, result, "extract", "repo/extract:v1", "commit-1", firstSnap.ID)
	requireTaskIntro(t, result, "load", "repo/load:v1", "commit-3", readd.ID)
	requireEdgeIntro(t, result, "extract", "load", "commit-3", readd.ID, "commit-3")
}

func TestBlameRangeStartsAfterOriginalIntroduction(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedBlameJob(t, db)
	now := time.Now().UTC()
	seedBlameSnapshot(t, db, jobID, "hash-1", "commit-1", now,
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{edge("extract", "load", "commit-1")},
	)
	second := seedBlameSnapshot(t, db, jobID, "hash-2", "commit-2", now.Add(time.Minute),
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1"),
			task("load", "repo/load:v1"),
			task("publish", "repo/publish:v1"),
		},
		[]models.DagSnapshotEdge{
			edge("extract", "load", "commit-2"),
			edge("load", "publish", "commit-2"),
		},
	)

	result, err := New(db).Blame(context.Background(), jobID, Options{
		FromCommit: "commit-2",
		ToCommit:   "commit-2",
	})
	require.NoError(t, err)
	require.Equal(t, CoverageTopologyImageCommand, result.Coverage)
	require.Len(t, result.Tasks, 1)
	require.Len(t, result.Edges, 1)
	requireTaskIntro(t, result, "publish", "repo/publish:v1", "commit-2", second.ID)
	requireEdgeIntro(t, result, "load", "publish", "commit-2", second.ID, "commit-2")
}

func TestBlameAttributesCommandOnlyMutation(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	jobID := seedBlameJob(t, db)
	now := time.Now().UTC()
	seedBlameSnapshot(t, db, jobID, "hash-1", "commit-1", now,
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1", "sh", "-c", "echo a"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{edge("extract", "load", "commit-1")},
	)
	// Same name AND image; ONLY the command changes. Because the descriptor key
	// is {name,image,command}, this is a content transition attributed to the
	// mutating snapshot — not the original introduction. If the key dropped
	// command, blame would (wrongly) still point at commit-1.
	mutating := seedBlameSnapshot(t, db, jobID, "hash-2", "commit-2", now.Add(time.Minute),
		[]models.DagSnapshotTask{
			task("extract", "repo/extract:v1", "sh", "-c", "echo b"),
			task("load", "repo/load:v1"),
		},
		[]models.DagSnapshotEdge{edge("extract", "load", "commit-2")},
	)

	result, err := New(db).Blame(context.Background(), jobID, Options{Task: "extract"})
	require.NoError(t, err)
	require.Len(t, result.Tasks, 1)
	got := result.Tasks[0]
	require.Equal(t, "extract", got.Element.Name)
	require.Equal(t, "repo/extract:v1", got.Element.Image)
	require.Equal(t, []string{"sh", "-c", "echo b"}, got.Element.Command)
	require.Equal(t, "commit-2", got.IntroducingCommit)
	require.Equal(t, mutating.ID, got.SnapshotID)
}

func seedBlameJob(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()

	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:   triggerID,
		Type: "cron",
	}).Error)

	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID:        jobID,
		TriggerID: triggerID,
		Alias:     "blame-job-" + jobID.String()[:8],
	}).Error)
	return jobID
}

func seedBlameSnapshot(t *testing.T, db *gorm.DB, jobID uuid.UUID, hash, commit string, at time.Time, tasks []models.DagSnapshotTask, edges []models.DagSnapshotEdge) *models.DagSnapshot {
	t.Helper()

	tasksJSON, err := json.Marshal(tasks)
	require.NoError(t, err)
	edgesJSON, err := json.Marshal(edges)
	require.NoError(t, err)

	snap := &models.DagSnapshot{
		ID:          uuid.New(),
		JobID:       jobID,
		ContentHash: hash,
		GitCommit:   commit,
		Tasks:       datatypes.JSON(tasksJSON),
		Edges:       datatypes.JSON(edgesJSON),
		CreatedAt:   at,
	}
	require.NoError(t, db.Create(snap).Error)
	return snap
}

func task(name, image string, command ...string) models.DagSnapshotTask {
	return models.DagSnapshotTask{Name: name, Image: image, Command: command}
}

func edge(from, to, provenanceCommit string) models.DagSnapshotEdge {
	return models.DagSnapshotEdge{From: from, To: to, ProvenanceCommit: provenanceCommit}
}

func requireTaskIntro(t *testing.T, result *Result, name, image, commit string, snapshotID uuid.UUID) {
	t.Helper()

	for _, got := range result.Tasks {
		if got.Element.Name == name && got.Element.Image == image {
			require.Equal(t, commit, got.IntroducingCommit)
			require.Equal(t, snapshotID, got.SnapshotID)
			return
		}
	}
	t.Fatalf("task %s with image %s not found in result: %#v", name, image, result.Tasks)
}

func requireEdgeIntro(t *testing.T, result *Result, from, to, commit string, snapshotID uuid.UUID, provenanceCommit string) {
	t.Helper()

	for _, got := range result.Edges {
		if got.Element.From == from && got.Element.To == to {
			require.Equal(t, commit, got.IntroducingCommit)
			require.Equal(t, snapshotID, got.SnapshotID)
			require.Equal(t, provenanceCommit, got.ProvenanceCommit)
			return
		}
	}
	t.Fatalf("edge %s -> %s not found in result: %#v", from, to, result.Edges)
}

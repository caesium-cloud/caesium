package incident

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestBuildBundleAssemblesAndScrubs(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	runID := mkRun(t, db, jobID, tr)

	// A failing task run whose log carries a high-entropy credential-shaped token
	// the scrubber must strip before it enters the bundle.
	taskID := uuid.New()
	secret := "AKIAIOSFODNN7EXAMPLEkey9s3cr3tV4lue"
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.TaskRun{
		ID:        uuid.New(),
		JobRunID:  runID,
		TaskID:    taskID,
		AtomID:    uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "vendor/extract:1.2",
		Command:   "extract",
		Status:    "failed",
		Error:     "auth failed",
		LogText:   "connecting with token " + secret + " ... 401 unauthorized",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	store := NewStore(db)
	incTaskID := taskID
	incRunID := runID
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{
		JobID:     jobID,
		RunID:     &incRunID,
		TaskID:    &incTaskID,
		TaskName:  "extract",
		Class:     ClassAuthFailure,
		LastError: "auth failed",
	})
	require.NoError(t, err)

	profile := &models.AgentProfile{
		ID:       uuid.New(),
		Name:     "triage",
		Image:    "caesium/triage:latest",
		Engine:   models.AtomEngineDocker,
		Playbook: datatypes.JSON([]byte(`{"allow":["retry_from_failure"]}`)),
	}
	require.NoError(t, db.Create(profile).Error)

	bundle, err := BuildBundle(ctx, db, inc.ID, profile)
	require.NoError(t, err)

	require.Equal(t, inc.ID, bundle.Incident.ID)
	require.Equal(t, string(ClassAuthFailure), bundle.Classification.Class)
	require.Equal(t, "vendor-x", bundle.Job.Alias)

	// The log tail is present, scrubbed, and no longer contains the secret token.
	require.True(t, bundle.Failure.LogTailScrubbed)
	require.NotContains(t, bundle.Failure.LogTail, secret)
	require.Contains(t, bundle.Failure.LogTail, Redacted)
	require.Equal(t, "vendor/extract:1.2", bundle.Failure.Image)

	// The frozen allowlist and the effective playbook ride along.
	require.Equal(t, []string{"vendor-x"}, bundle.LineageImpact.AllowedJobs)
	require.True(t, bundle.LineageImpact.Frozen)
	require.JSONEq(t, `{"allow":["retry_from_failure"]}`, string(bundle.Playbook))

	// Run history includes the seeded run.
	require.NotEmpty(t, bundle.RunHistory)
}

func TestBuildBundleIncludesPriorNotes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobID := mkJob(t, db, tr, "vendor-x")
	store := NewStore(db)
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{JobID: jobID, TaskName: "extract", Class: ClassUnknown})
	require.NoError(t, err)

	_, err = RecordNote(ctx, db, inc.ID, nil, nil, "vendor file late again")
	require.NoError(t, err)

	bundle, err := BuildBundle(ctx, db, inc.ID, nil)
	require.NoError(t, err)
	require.Len(t, bundle.Notes, 1)
	require.Equal(t, "vendor file late again", bundle.Notes[0].Text)
	require.True(t, strings.TrimSpace(string(bundle.Playbook)) == "", "no profile → no playbook")
}

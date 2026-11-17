package reproduce

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func seedDescriptorJobRun(t *testing.T, db *gorm.DB, jobID uuid.UUID) uuid.UUID {
	t.Helper()
	runID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        runID,
		JobID:     jobID,
		Status:    "succeeded",
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return runID
}

func seedDescriptorTask(t *testing.T, db *gorm.DB, jobID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	taskID := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Task{
		ID:        taskID,
		JobID:     jobID,
		AtomID:    uuid.New(),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return taskID
}

func seedDescriptorTaskRun(t *testing.T, db *gorm.DB, runID, taskID uuid.UUID, partitionValue string, partitionIndex, partitionCount int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.TaskRun{
		ID:                  id,
		JobRunID:            runID,
		TaskID:              taskID,
		AtomID:              uuid.New(),
		Engine:              models.AtomEngineDocker,
		Image:               "alpine:3.23",
		Command:             `["echo"]`,
		Status:              "succeeded",
		Attempt:             1,
		MaxAttempts:         1,
		PartitionValue:      partitionValue,
		PartitionIndex:      partitionIndex,
		PartitionCount:      partitionCount,
		ExecutionDescriptor: datatypes.JSON([]byte(`{"schemaVersion":1}`)),
		StartedAt:           &now,
		CompletedAt:         &now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}).Error)
	return id
}

// TestDescriptor_UnfannedResolves is the baseline: an ordinary task run has no
// partition identity and resolves to its stored descriptor.
func TestDescriptor_UnfannedResolves(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	taskID := seedDescriptorTask(t, db, jobID, "load")
	seedDescriptorTaskRun(t, db, runID, taskID, "", 0, 0)

	svc := NewWithDatabase(context.Background(), db)
	got, err := svc.Descriptor(runID, "load")
	require.NoError(t, err)
	assert.JSONEq(t, `{"schemaVersion":1}`, string(got.Descriptor))
}

// TestDescriptor_SinglePartitionExpansionIsAmbiguous pins the fix for the
// adversarial-review finding that the fan-out guard tested partition_count > 1.
// A fan-out that expanded to exactly one partition still produces a
// per-partition descriptor whose identity includes the partition key, so
// resolving it by bare task name silently hands back a partition-specific
// descriptor from a surface that is documented to refuse fanned tasks — and an
// N=1 group can become N>1 on the next run, making the surface's answer
// non-reproducible.
func TestDescriptor_SinglePartitionExpansionIsAmbiguous(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	taskID := seedDescriptorTask(t, db, jobID, "process")
	seedDescriptorTaskRun(t, db, runID, taskID, "2026-07-01", 0, 1)

	svc := NewWithDatabase(context.Background(), db)

	_, err := svc.Descriptor(runID, "process")
	require.ErrorIs(t, err, ErrFannedTaskAmbiguous)

	_, err = svc.Descriptor(runID, taskID.String())
	require.ErrorIs(t, err, ErrFannedTaskAmbiguous, "the task-UUID path must refuse the same row the name path refuses")
}

// TestDescriptor_MultiPartitionExpansionIsAmbiguous is the already-covered N>1
// case, kept so the N=1 fix cannot regress it.
func TestDescriptor_MultiPartitionExpansionIsAmbiguous(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	taskID := seedDescriptorTask(t, db, jobID, "process")
	seedDescriptorTaskRun(t, db, runID, taskID, "a", 0, 2)
	seedDescriptorTaskRun(t, db, runID, taskID, "b", 1, 2)

	svc := NewWithDatabase(context.Background(), db)
	_, err := svc.Descriptor(runID, "process")
	require.ErrorIs(t, err, ErrFannedTaskAmbiguous)
}

// TestDescriptor_NotFound keeps the not-found contract distinct from the
// fanned-ambiguous one.
func TestDescriptor_MiniWhyNamesChangedFieldAndRedactsSecrets(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	const (
		password = "super-secret-value"
		token    = "sk-live-secret"
	)
	literalDigest := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	jobID := uuid.New()
	taskID := seedDescriptorTask(t, db, jobID, "transform")
	earlier := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	later := earlier.Add(time.Hour)
	priorRun := seedDescriptorJobRun(t, db, jobID)
	subjectRun := seedDescriptorJobRun(t, db, jobID)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", priorRun).Update("started_at", earlier).Error)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", subjectRun).Update("started_at", later).Error)

	priorID := seedDescriptorTaskRun(t, db, priorRun, taskID, "", 0, 0)
	subjectID := seedDescriptorTaskRun(t, db, subjectRun, taskID, "", 0, 0)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", priorID).Update("hash_input_blob", datatypes.JSON(`{
		"blobVersion": 1,
		"hash": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"image": "alpine:3.23",
		"env": {
			"LITERAL_ENV": {"redacted": {"digest": "`+literalDigest+`", "redacted": true}},
			"SECRET_ENV": {"secret": "secret://env/CAESIUM_REPRODUCE_CLI_SECRET_ENV"}
		},
		"runParams": {"flavor": "vanilla"},
		"cacheVersion": 0
	}`)).Error)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", subjectID).Update("hash_input_blob", datatypes.JSON(`{
		"blobVersion": 1,
		"hash": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"image": "alpine:3.23",
		"env": {
			"LITERAL_ENV": {"redacted": {"digest": "`+literalDigest+`", "redacted": true}},
			"SECRET_ENV": {"secret": "secret://env/CAESIUM_REPRODUCE_CLI_SECRET_ENV"},
			"PASSWORD": "`+password+`",
			"LEAKED": {"secret": "`+password+`"}
		},
		"runParams": {"flavor": "chocolate", "api_token": "`+token+`"},
		"predecessorOutputs": {"extract": {"row_count": "7", "password": "`+password+`"}},
		"cacheVersion": 0
	}`)).Error)

	svc := NewWithDatabase(context.Background(), db)
	got, err := svc.Descriptor(subjectRun, "transform")
	require.NoError(t, err)
	require.NotNil(t, got.MiniWhy)
	assert.Equal(t, "prior_run", got.MiniWhy.BaselineKind)
	assert.Equal(t, priorRun.String(), got.MiniWhy.ComparedToRunID)
	assert.False(t, got.MiniWhy.HashEqual)

	flavor := miniWhyFieldByName(t, got.MiniWhy, "runParams.flavor")
	assert.Equal(t, "changed", flavor.Change)
	assert.Equal(t, "vanilla", flavor.Before)
	assert.Equal(t, "chocolate", flavor.After)
	assert.False(t, flavor.Redacted)

	passwordField := miniWhyFieldByName(t, got.MiniWhy, "env.PASSWORD")
	assert.True(t, passwordField.Redacted)
	assert.Empty(t, passwordField.Before)
	assert.Empty(t, passwordField.After)

	tokenField := miniWhyFieldByName(t, got.MiniWhy, "runParams.api_token")
	assert.True(t, tokenField.Redacted)
	assert.Empty(t, tokenField.Before)
	assert.Empty(t, tokenField.After)

	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	body := string(encoded)
	assert.NotContains(t, body, password)
	assert.NotContains(t, body, token)
	assert.Contains(t, body, literalDigest)
	assert.Contains(t, body, "secret://env/CAESIUM_REPRODUCE_CLI_SECRET_ENV")
	assert.NotContains(t, got.MiniWhy.Summary, password)
	assert.NotContains(t, got.MiniWhy.Summary, token)
	assert.Contains(t, got.MiniWhy.Summary, "runParams.flavor changed vanilla→chocolate")

	first, err := svc.Descriptor(priorRun, "transform")
	require.NoError(t, err)
	require.NotNil(t, first.MiniWhy)
	assert.Equal(t, "none", first.MiniWhy.BaselineKind)
	assert.NotContains(t, string(first.HashInputBlob), "vanilla-is-not-a-secret")
	assert.Contains(t, string(first.HashInputBlob), literalDigest)
}

func miniWhyFieldByName(t *testing.T, why *MiniWhy, name string) MiniWhyField {
	t.Helper()
	for _, field := range why.Fields {
		if field.Field == name {
			return field
		}
	}
	t.Fatalf("mini-why fields missing %s: %+v", name, why.Fields)
	return MiniWhyField{}
}

func TestDescriptor_NotFound(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	jobID := uuid.New()
	runID := seedDescriptorJobRun(t, db, jobID)
	seedDescriptorTask(t, db, jobID, "load")

	svc := NewWithDatabase(context.Background(), db)
	_, err := svc.Descriptor(runID, "load")
	require.ErrorIs(t, err, runstorage.ErrTaskRunNotFound)
}

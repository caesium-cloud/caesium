package run

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestClassifyVerdict(t *testing.T) {
	cases := []struct {
		name   string
		status TaskStatus
		cache  bool
		want   WhyVerdict
	}{
		{"cached", TaskStatusCached, true, VerdictCacheHit},
		{"succeeded-cache-on", TaskStatusSucceeded, true, VerdictCacheMiss},
		{"succeeded-cache-off", TaskStatusSucceeded, false, VerdictCacheOff},
		{"failed-cache-on", TaskStatusFailed, true, VerdictCacheMiss},
		{"running", TaskStatusRunning, true, VerdictUnknown},
		{"pending", TaskStatusPending, false, VerdictUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &models.TaskRun{Status: string(tc.status), CacheEnabled: tc.cache}
			if got := classifyVerdict(tr); got != tc.want {
				t.Errorf("classifyVerdict(%s, cache=%v) = %s, want %s", tc.status, tc.cache, got, tc.want)
			}
		})
	}
}

func TestSummarize_CacheMissNamesHeadlineField(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "load",
		Verdict:  VerdictCacheMiss,
		Baseline: WhyBaseline{Kind: "prior_run"},
		Diff: &BlobDiff{
			Changes: []FieldChange{
				{Field: "predecessorOutputs.extract.row_count", Kind: fieldMapEntry, Before: "1200000", After: "1400000"},
				{Field: "image", Kind: fieldScalar, Before: "alpine:3.23", After: "busybox:1.36.1"},
			},
		},
	}
	got := summarize(exp)
	if !strings.HasPrefix(got, "CACHE MISS") {
		t.Errorf("expected CACHE MISS prefix, got %q", got)
	}
	if !strings.Contains(got, "extract.row_count") || !strings.Contains(got, "1200000") || !strings.Contains(got, "1400000") {
		t.Errorf("expected headline field + before/after in summary, got %q", got)
	}
	if !strings.Contains(got, "and 1 other field") {
		t.Errorf("expected secondary-change count, got %q", got)
	}
}

func TestSummarize_CacheHitIdentical(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "transform",
		Verdict:  VerdictCacheHit,
		Diff:     &BlobDiff{HashEqual: true},
	}
	got := summarize(exp)
	if !strings.HasPrefix(got, "CACHE HIT") {
		t.Errorf("expected CACHE HIT prefix, got %q", got)
	}
	if !strings.Contains(got, "identical") {
		t.Errorf("expected identical-inputs proof, got %q", got)
	}
}

func TestSummarize_CacheMissNoPriorRun(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "extract",
		Verdict:  VerdictCacheMiss,
		Baseline: WhyBaseline{Kind: "none"},
		Diff:     &BlobDiff{},
	}
	got := summarize(exp)
	if !strings.Contains(got, "first run") {
		t.Errorf("expected first-run explanation, got %q", got)
	}
}

func TestSummarize_Degraded(t *testing.T) {
	exp := &WhyExplanation{
		TaskName: "x",
		Verdict:  VerdictCacheMiss,
		Baseline: WhyBaseline{Kind: "prior_run"},
		Diff:     &BlobDiff{Degraded: "one or both blobs were stored oversized"},
	}
	got := summarize(exp)
	if !strings.Contains(got, "oversized") {
		t.Errorf("expected degraded reason surfaced, got %q", got)
	}
}

func TestDescribeChange_RedactedNeverShowsPlaintext(t *testing.T) {
	c := FieldChange{Field: "env.SECRET", Kind: fieldMapEntry, Before: "sha256:aaa", After: "sha256:bbb", Redacted: true}
	got := describeChange(c)
	if !strings.Contains(got, "redacted") {
		t.Errorf("expected redacted label, got %q", got)
	}
	if !strings.Contains(got, "sha256:aaa") || !strings.Contains(got, "sha256:bbb") {
		t.Errorf("expected digests in redacted description, got %q", got)
	}
}

// blobBytes canonicalizes a HashInput the way the A2 write-path does.
func blobBytes(t *testing.T, h cache.HashInput) []byte {
	t.Helper()
	data, err := h.CanonicalJSON(h.Compute())
	require.NoError(t, err)
	return data
}

func TestWhyTask_CacheHitNilOriginUsesLiveEntry(t *testing.T) {
	// Regression guard for the nil-CacheOriginRunID bug: a cached task whose
	// CacheOriginRunID is nil must still be explained as a CACHE HIT via the live
	// TaskCache entry — not mislabeled as a re-run via the MISS path.
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	taskID := uuid.New()
	runID := uuid.New()
	now := time.Now().UTC()

	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(StatusRunning),
		TriggerType: "cron", StartedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "transform"}).Error)

	hi := cache.HashInput{JobAlias: "etl", TaskName: "transform", Image: "alpine:3.23"}
	hash := hi.Compute()
	blob := blobBytes(t, hi)

	// Cached task run with NO CacheOriginRunID set.
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
		Status: string(TaskStatusCached), CacheHit: true, CacheEnabled: true,
		Hash: hash, HashInputBlob: datatypes.JSON(blob),
		CreatedAt: now, UpdatedAt: now,
	}).Error)

	// Live cache entry keyed by the same hash, carrying the blob.
	require.NoError(t, db.Create(&models.TaskCache{
		Hash: hash, JobID: jobID, TaskName: "transform", Result: "ok",
		RunID: uuid.New(), TaskRunID: uuid.New(), HashInputBlob: datatypes.JSON(blob),
		CreatedAt: now,
	}).Error)

	exp, err := store.WhyTask(ctx, runID, "transform")
	require.NoError(t, err)
	require.Equal(t, VerdictCacheHit, exp.Verdict, "cached task with nil origin must be a HIT, not a re-run")
	require.Equal(t, "cache_origin", exp.Baseline.Kind, "must fall back to the live cache entry")
	require.NotNil(t, exp.Diff)
	require.True(t, exp.Diff.HashEqual, "hit diff should confirm identical hashed inputs")
	require.True(t, strings.HasPrefix(exp.Summary, "CACHE HIT"), "summary=%q", exp.Summary)
}

func TestWhyTask_CacheMissDiffsPriorRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	taskID := uuid.New()
	priorRunID := uuid.New()
	subjectRunID := uuid.New()
	base := time.Now().UTC()

	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "load"}).Error)

	// Prior run (earlier) with row_count=1.2M.
	require.NoError(t, db.Create(&models.JobRun{
		ID: priorRunID, JobID: jobID, Status: string(StatusSucceeded),
		TriggerType: "cron", StartedAt: base,
	}).Error)
	priorHI := cache.HashInput{TaskName: "load", Image: "alpine:3.23",
		PredecessorOutputs: map[string]map[string]string{"extract": {"row_count": "1200000"}}}
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: priorRunID, TaskID: taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
		Status: string(TaskStatusSucceeded), CacheEnabled: true,
		Hash: priorHI.Compute(), HashInputBlob: datatypes.JSON(blobBytes(t, priorHI)),
		StartedAt: &base, CreatedAt: base, UpdatedAt: base,
	}).Error)

	// Subject run (later) with row_count=1.4M — a cache miss.
	later := base.Add(time.Hour)
	require.NoError(t, db.Create(&models.JobRun{
		ID: subjectRunID, JobID: jobID, Status: string(StatusSucceeded),
		TriggerType: "http", TriggerAlias: "webhook-1", StartedAt: later,
	}).Error)
	subjectHI := cache.HashInput{TaskName: "load", Image: "alpine:3.23",
		PredecessorOutputs: map[string]map[string]string{"extract": {"row_count": "1400000"}}}
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: subjectRunID, TaskID: taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
		Status: string(TaskStatusSucceeded), CacheEnabled: true,
		Hash: subjectHI.Compute(), HashInputBlob: datatypes.JSON(blobBytes(t, subjectHI)),
		StartedAt: &later, CreatedAt: later, UpdatedAt: later,
	}).Error)

	exp, err := store.WhyTask(ctx, subjectRunID, "load")
	require.NoError(t, err)
	require.Equal(t, VerdictCacheMiss, exp.Verdict)
	require.Equal(t, "prior_run", exp.Baseline.Kind)
	require.Equal(t, "http", exp.Trigger.Type)
	require.Equal(t, "webhook-1", exp.Trigger.Alias)
	require.NotNil(t, exp.Diff)
	c, ok := findChange(exp.Diff.Changes, "predecessorOutputs.extract.row_count")
	require.True(t, ok, "expected the row_count change, got %+v", exp.Diff.Changes)
	require.Equal(t, "1200000", c.Before)
	require.Equal(t, "1400000", c.After)
	require.Contains(t, exp.Summary, "row_count")
}

func TestWhyTask_FirstRunNoPriorBaseline(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	taskID := uuid.New()
	runID := uuid.New()
	now := time.Now().UTC()

	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(StatusSucceeded), TriggerType: "manual", StartedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "extract"}).Error)

	hi := cache.HashInput{TaskName: "extract", Image: "alpine:3.23"}
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo"]`,
		Status: string(TaskStatusSucceeded), CacheEnabled: true,
		Hash: hi.Compute(), HashInputBlob: datatypes.JSON(blobBytes(t, hi)),
		StartedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error)

	exp, err := store.WhyTask(ctx, runID, "extract")
	require.NoError(t, err)
	require.Equal(t, VerdictCacheMiss, exp.Verdict)
	require.Equal(t, "none", exp.Baseline.Kind)
	require.Contains(t, exp.Summary, "first run")
}

func TestWhyTask_UnknownTaskReturnsNotFound(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)
	ctx := context.Background()

	jobID := uuid.New()
	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: string(StatusRunning), StartedAt: time.Now().UTC(),
	}).Error)

	_, err := store.WhyTask(ctx, runID, "does-not-exist")
	require.ErrorIs(t, err, ErrTaskRunNotFound)
}

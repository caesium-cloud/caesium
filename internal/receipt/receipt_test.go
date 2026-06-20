package receipt

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ReceiptSuite exercises Build/Verify over an in-memory SQLite database holding
// the same persisted shape a real run produces (Job, JobRun, Task, TaskRun,
// DagSnapshot).
type ReceiptSuite struct {
	suite.Suite
	db  *gorm.DB
	ctx context.Context
}

func TestReceiptSuite(t *testing.T) {
	suite.Run(t, new(ReceiptSuite))
}

func (s *ReceiptSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
	s.ctx = context.Background()
}

func (s *ReceiptSuite) TearDownTest() {
	if s.db != nil {
		if sqlDB, _ := s.db.DB(); sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

// taskSpec describes a task to seed into a run.
type taskSpec struct {
	name         string
	image        string
	hash         string
	digest       string // resolved image digest; "" = unpinned
	pinRequested bool
}

// seedRun writes a Job, JobRun, the Tasks, and the TaskRuns for the given
// specs, plus (optionally) a DagSnapshot carrying the manifest content hash.
// It returns the run ID.
func (s *ReceiptSuite) seedRun(alias, gitCommit, manifestHash string, specs []taskSpec) uuid.UUID {
	jobID := uuid.New()
	runID := uuid.New()
	now := time.Now()

	s.Require().NoError(s.db.Create(&models.Job{
		ID:               jobID,
		Alias:            alias,
		TriggerID:        uuid.New(),
		ProvenanceCommit: gitCommit,
		CreatedAt:        now,
		UpdatedAt:        now,
	}).Error)

	s.Require().NoError(s.db.Create(&models.JobRun{
		ID:        runID,
		JobID:     jobID,
		TriggerID: uuid.New(),
		Status:    "succeeded",
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	for _, sp := range specs {
		taskID := uuid.New()
		s.Require().NoError(s.db.Create(&models.Task{
			ID:        taskID,
			JobID:     jobID,
			AtomID:    uuid.New(),
			Name:      sp.name,
			CreatedAt: now,
			UpdatedAt: now,
		}).Error)

		s.Require().NoError(s.db.Create(&models.TaskRun{
			ID:                  uuid.New(),
			JobRunID:            runID,
			TaskID:              taskID,
			AtomID:              uuid.New(),
			Engine:              models.AtomEngineDocker,
			Image:               sp.image,
			Command:             "[]",
			Status:              "succeeded",
			Hash:                sp.hash,
			ResolvedImageDigest: sp.digest,
			CachePinDigests:     sp.pinRequested,
			CreatedAt:           now,
			UpdatedAt:           now,
		}).Error)
	}

	if manifestHash != "" {
		s.Require().NoError(s.db.Create(&models.DagSnapshot{
			ID:          uuid.New(),
			JobID:       jobID,
			ContentHash: manifestHash,
			GitCommit:   gitCommit,
			Tasks:       []byte("[]"),
			Edges:       []byte("[]"),
			CreatedAt:   now,
		}).Error)
	}

	return runID
}

// pinnedSpecs is a two-task, fully-digest-pinned run.
func pinnedSpecs() []taskSpec {
	return []taskSpec{
		{name: "extract", image: "alpine:3.23", hash: "hash-extract", digest: "sha256:aaa", pinRequested: true},
		{name: "load", image: "python:3.12", hash: "hash-load", digest: "sha256:bbb", pinRequested: true},
	}
}

func (s *ReceiptSuite) TestBuildPinnedNotDegraded() {
	runID := s.seedRun("etl", "commit-1", "manifest-1", pinnedSpecs())

	r, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	s.Equal(Version, r.ReceiptVersion)
	s.Equal(runID, r.RunID)
	s.Equal("etl", r.JobAlias)
	s.Equal("commit-1", r.GitCommit)
	s.Equal("manifest-1", r.ManifestContentHash)
	s.False(r.Degraded, "fully digest-pinned run must not be degraded")
	s.Empty(r.DegradedTasks)
	s.NotEmpty(r.ReceiptDigest)

	// Tasks are sorted by name: extract before load.
	s.Require().Len(r.Tasks, 2)
	s.Equal("extract", r.Tasks[0].TaskName)
	s.Equal("load", r.Tasks[1].TaskName)
	s.True(r.Tasks[0].DigestPinned)
	s.True(r.Tasks[1].DigestPinned)
}

// TestDeterminism: building the receipt twice over identical persisted state
// yields the byte-identical receipt digest; task seed order does not matter.
func (s *ReceiptSuite) TestDeterminism() {
	specs := pinnedSpecs()
	// Distinct aliases (the alias is metadata, not folded into the digest), so
	// the unique-alias index is satisfied while inputs that DO affect the digest
	// — git commit, manifest hash, and the per-task identities — are identical.
	runA := s.seedRun("etl-a", "commit-x", "manifest-x", specs)

	// Reverse the seed order for the second run; the digest must be identical.
	reversed := []taskSpec{specs[1], specs[0]}
	runB := s.seedRun("etl-b", "commit-x", "manifest-x", reversed)

	rA, err := Build(s.ctx, s.db, runA)
	s.Require().NoError(err)
	rB, err := Build(s.ctx, s.db, runB)
	s.Require().NoError(err)

	s.Equal(rA.ReceiptDigest, rB.ReceiptDigest,
		"identical inputs in any order must produce the same receipt digest")
}

// TestUnpinnedDegraded: a task with pinning requested but no resolved digest
// (the Podman/k8s tag-fallback case) is marked degraded with an honest reason,
// and the whole receipt is degraded.
func (s *ReceiptSuite) TestUnpinnedDegraded() {
	specs := []taskSpec{
		{name: "extract", image: "alpine:3.23", hash: "h1", digest: "sha256:aaa", pinRequested: true},
		{name: "transform", image: "busybox:1.36.1", hash: "h2", digest: "", pinRequested: true},
	}
	runID := s.seedRun("etl", "commit-1", "manifest-1", specs)

	r, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	s.True(r.Degraded, "a run with an unpinned task must be degraded")
	s.Equal([]string{"transform"}, r.DegradedTasks)

	transform := r.Tasks[s.indexOf(r, "transform")]
	s.False(transform.DigestPinned)
	s.True(transform.Degraded)
	s.Contains(transform.DegradedReason, "not digest-pinned")
	s.Contains(transform.DegradedReason, "busybox:1.36.1")
}

// TestNoHashDegraded: a task with no identity hash at all (caching disabled)
// cannot be attested and is degraded.
func (s *ReceiptSuite) TestNoHashDegraded() {
	specs := []taskSpec{
		{name: "extract", image: "alpine:3.23", hash: "", digest: "sha256:aaa", pinRequested: true},
	}
	runID := s.seedRun("etl", "commit-1", "manifest-1", specs)

	r, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	s.True(r.Degraded)
	s.Equal([]string{"extract"}, r.DegradedTasks)
	s.Contains(r.Tasks[0].DegradedReason, "no identity hash")
}

// TestVerifyClean: re-deriving an unchanged, fully-pinned run against its own
// committed receipt reports a clean match with no drift.
func (s *ReceiptSuite) TestVerifyClean() {
	runID := s.seedRun("etl", "commit-1", "manifest-1", pinnedSpecs())
	committed, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	result, err := Verify(s.ctx, s.db, committed)
	s.Require().NoError(err)

	s.True(result.Match, "unchanged pinned run must verify clean")
	s.False(result.Degraded)
	s.Empty(result.Drifts)
	s.Equal(committed.ReceiptDigest, result.ActualDigest)
}

// TestVerifyDigestDrift: mutate the resolved image digest of a task after the
// receipt was committed (a moved :latest) and verify catches the drift with an
// image-digest-mismatch and the umbrella receipt-digest-mismatch.
func (s *ReceiptSuite) TestVerifyDigestDrift() {
	runID := s.seedRun("etl", "commit-1", "manifest-1", pinnedSpecs())
	committed, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	// Simulate the tag moving: the load task's image digest changed underneath.
	s.Require().NoError(s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND image = ?", runID, "python:3.12").
		Update("resolved_image_digest", "sha256:MUTATED").Error)
	// The identity hash would also change in reality (the digest is folded into
	// it); simulate that too so the test mirrors production.
	s.Require().NoError(s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND image = ?", runID, "python:3.12").
		Update("hash", "hash-load-NEW").Error)

	result, err := Verify(s.ctx, s.db, committed)
	s.Require().NoError(err)

	s.False(result.Match, "a moved digest must fail verification")
	s.False(result.Degraded, "the run is still pinned — drift, not degradation")

	s.True(s.hasDrift(result, DriftImageDigest, "load"))
	s.True(s.hasDrift(result, DriftIdentityHash, "load"))
	s.True(s.hasDrift(result, DriftReceiptDigest, ""))
}

// TestVerifyManifestDrift: the manifest content hash changed since the receipt
// was committed.
func (s *ReceiptSuite) TestVerifyManifestDrift() {
	runID := s.seedRun("etl", "commit-1", "manifest-1", pinnedSpecs())
	committed, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	// A newer snapshot for the same commit changes the manifest hash Build picks.
	s.Require().NoError(s.db.Model(&models.DagSnapshot{}).
		Where("git_commit = ?", "commit-1").
		Update("content_hash", "manifest-CHANGED").Error)

	result, err := Verify(s.ctx, s.db, committed)
	s.Require().NoError(err)

	s.False(result.Match)
	s.True(s.hasDrift(result, DriftManifest, ""))
}

// TestVerifyDegradedNeverMatches: even when the digests are byte-equal, a run
// that ran on an unpinned tag must never report a clean match — the unpinned
// tag could have moved without changing the tag-only digest.
func (s *ReceiptSuite) TestVerifyDegradedNeverMatches() {
	specs := []taskSpec{
		{name: "extract", image: "busybox:1.36.1", hash: "h1", digest: "", pinRequested: true},
	}
	runID := s.seedRun("etl", "commit-1", "manifest-1", specs)
	committed, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	// Nothing changed — digests are identical.
	result, err := Verify(s.ctx, s.db, committed)
	s.Require().NoError(err)

	s.Equal(committed.ReceiptDigest, result.ActualDigest, "digests match...")
	s.True(result.Degraded, "...but the run is degraded")
	s.False(result.Match, "a degraded run must never report a clean match")
	s.Equal([]string{"extract"}, result.DegradedTasks)
}

// TestVerifyGitCommitDrift: the job's provenance commit changed (re-applied
// from a different commit).
func (s *ReceiptSuite) TestVerifyGitCommitDrift() {
	runID := s.seedRun("etl", "commit-1", "manifest-1", pinnedSpecs())
	committed, err := Build(s.ctx, s.db, runID)
	s.Require().NoError(err)

	s.Require().NoError(s.db.Model(&models.Job{}).
		Where("alias = ?", "etl").
		Update("provenance_commit", "commit-2").Error)

	result, err := Verify(s.ctx, s.db, committed)
	s.Require().NoError(err)

	s.False(result.Match)
	s.True(s.hasDrift(result, DriftGitCommit, ""))
}

// TestBuildRunNotFound: a missing run returns ErrRunNotFound.
func (s *ReceiptSuite) TestBuildRunNotFound() {
	_, err := Build(s.ctx, s.db, uuid.New())
	s.ErrorIs(err, ErrRunNotFound)
}

// --- helpers ---

func (s *ReceiptSuite) indexOf(r *Receipt, name string) int {
	for i := range r.Tasks {
		if r.Tasks[i].TaskName == name {
			return i
		}
	}
	s.FailNowf("task not found", "task %q not in receipt", name)
	return -1
}

func (s *ReceiptSuite) hasDrift(result *VerifyResult, kind DriftKind, task string) bool {
	for _, d := range result.Drifts {
		if d.Kind == kind && d.Task == task {
			return true
		}
	}
	return false
}

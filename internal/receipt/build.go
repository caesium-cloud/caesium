package receipt

import (
	"context"
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrRunNotFound is returned by Build when the requested run does not exist.
var ErrRunNotFound = errors.New("receipt: run not found")

// Build re-derives the reproducibility receipt for a run from its persisted
// state. It reads only what the system recorded — TaskRun identity hashes and
// resolved image digests, the job's git provenance, and the matching DAG
// snapshot's manifest content hash — and never re-executes anything. This is
// the single derivation used both when first emitting a receipt and (via the
// same code path) when `caesium verify` re-derives one to compare.
//
// The returned receipt is finalized: tasks sorted, degraded summary populated,
// and ReceiptDigest computed. A run with any unpinned (mutable-tag) task is
// marked Degraded — its digest is still derived from the literal tag, the only
// identity available, but it must not be presented as a reproducibility
// guarantee.
func Build(ctx context.Context, db *gorm.DB, runID uuid.UUID) (*Receipt, error) {
	if db == nil {
		return nil, fmt.Errorf("receipt: nil database connection")
	}

	conn := db.WithContext(ctx)

	var run models.JobRun
	err := conn.Where("id = ?", runID).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("receipt: load run: %w", err)
	}

	// Load the job for git provenance. A soft-deleted job still carries the
	// provenance the run executed under, so this is best-effort: a missing job
	// (hard-deleted) leaves GitCommit empty rather than failing the build.
	var job models.Job
	gitCommit := ""
	if err := conn.Where("id = ?", run.JobID).First(&job).Error; err == nil {
		gitCommit = job.ProvenanceCommit
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("receipt: load job: %w", err)
	}

	// Load the task runs for this run. Each carries the identity hash and the
	// resolved digest the receipt attests over.
	var taskRuns []models.TaskRun
	if err := conn.Where("job_run_id = ?", runID).Find(&taskRuns).Error; err != nil {
		return nil, fmt.Errorf("receipt: load task runs: %w", err)
	}

	// Resolve task names. TaskRun carries only TaskID; the human-meaningful
	// name lives on Task. Build the name map in one query rather than N.
	names, err := taskNames(conn, taskRuns)
	if err != nil {
		return nil, err
	}

	entries := make([]TaskEntry, 0, len(taskRuns))
	for i := range taskRuns {
		tr := &taskRuns[i]
		name := names[tr.TaskID]
		if name == "" {
			// Fall back to the task ID so the entry is still addressable and
			// the receipt stays total even if the Task row was pruned.
			name = tr.TaskID.String()
		}
		entry := TaskEntry{
			TaskName:            name,
			IdentityHash:        tr.Hash,
			Image:               tr.Image,
			ResolvedImageDigest: tr.ResolvedImageDigest,
		}
		markDegraded(&entry)
		entries = append(entries, entry)
	}

	receipt := &Receipt{
		RunID:               run.ID,
		JobID:               run.JobID,
		JobAlias:            job.Alias,
		GitCommit:           gitCommit,
		ManifestContentHash: manifestContentHash(conn, run.JobID, gitCommit),
		Tasks:               entries,
	}
	receipt.finalize()
	return receipt, nil
}

// taskNames returns a TaskID -> name map for the given task runs in a single
// query. An empty input yields an empty map.
func taskNames(conn *gorm.DB, taskRuns []models.TaskRun) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string, len(taskRuns))
	if len(taskRuns) == 0 {
		return out, nil
	}

	ids := make([]uuid.UUID, 0, len(taskRuns))
	seen := make(map[uuid.UUID]struct{}, len(taskRuns))
	for i := range taskRuns {
		id := taskRuns[i].TaskID
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	var tasks []models.Task
	if err := conn.Where("id IN ?", ids).Find(&tasks).Error; err != nil {
		return nil, fmt.Errorf("receipt: load task names: %w", err)
	}
	for i := range tasks {
		out[tasks[i].ID] = tasks[i].Name
	}
	return out, nil
}

// manifestContentHash returns the content hash of the DAG topology this run
// executed, sourced from the append-only dag_snapshot table (Component 3 / B1).
// It prefers the snapshot matching the run's git commit (the topology actually
// applied from that commit); failing that it falls back to the job's most
// recent snapshot. It is best-effort: an empty string when no snapshot exists
// (e.g. a job applied before DAG versioning shipped) simply omits the manifest
// term from the digest rather than failing the build — the per-task identity
// hashes remain the primary attestation.
func manifestContentHash(conn *gorm.DB, jobID uuid.UUID, gitCommit string) string {
	if gitCommit != "" {
		var snap models.DagSnapshot
		err := conn.Where("job_id = ? AND git_commit = ?", jobID, gitCommit).
			Order("created_at DESC").
			First(&snap).Error
		if err == nil {
			return snap.ContentHash
		}
	}

	var snap models.DagSnapshot
	err := conn.Where("job_id = ?", jobID).
		Order("created_at DESC").
		First(&snap).Error
	if err == nil {
		return snap.ContentHash
	}
	return ""
}

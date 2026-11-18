package run

import (
	"context"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// RecoveredRuntime is the runtime a recovery path re-pended a TaskRun away
// from: the row's persisted runtime id as it was read inside the transaction
// that cleared it.
type RecoveredRuntime struct {
	TaskRunID uuid.UUID
	Engine    models.AtomEngine
	RuntimeID string
}

// recoveredRuntimeStopTimeout bounds each best-effort stop.
const recoveredRuntimeStopTimeout = 30 * time.Second

// kubernetesRecoveryEngine builds the engine recovery stops pods with. A var so
// tests can hand it a fake clientset.
var kubernetesRecoveryEngine = func(ctx context.Context) (atom.Engine, error) {
	return kubernetes.NewEngine(ctx)
}

// launchRecoveredRuntimeStop runs the stops off the recovery path so a slow or
// failing cluster API never delays or blocks recovery. Tests make it
// synchronous.
var launchRecoveredRuntimeStop = func(stop func()) { go stop() }

// stoppableRuntime reports whether recovery can stop a row's attempt: Kubernetes
// rows with a persisted runtime id. Docker and Podman ids name a container on
// the host that ran it, which a recovering node generally is not.
func stoppableRuntime(row *models.TaskRun) bool {
	return row.Engine == models.AtomEngineKubernetes && strings.TrimSpace(row.RuntimeID) != ""
}

// recoveredRuntimesFrom is the stop list of a recovery reset: the stoppable rows
// among those the reset actually re-pended. rows is what the recovery read
// before its UPDATE, and it is NOT that set: on dqlite nothing commits between
// the two, but Postgres runs them at READ COMMITTED, so a renewal or completion
// landing between them leaves a row the guarded UPDATE skipped, whose attempt
// is live or finished. reset names the rows the UPDATE landed on, resolved after
// it, so a stop can never reach a row the system still counts. A finished row
// stays out of this list; ReapAbandonedKubernetesPods deletes a pod that row
// still names, without re-pending it.
func recoveredRuntimesFrom(rows []models.TaskRun, reset map[uuid.UUID]struct{}) []RecoveredRuntime {
	out := make([]RecoveredRuntime, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if !stoppableRuntime(row) {
			continue
		}
		if _, ok := reset[row.ID]; !ok {
			continue
		}
		out = append(out, RecoveredRuntime{TaskRunID: row.ID, Engine: row.Engine, RuntimeID: row.RuntimeID})
	}
	return out
}

// RecoveredRuntimesAfterReset resolves the stop list for a caller outside this
// package. Call it inside the reset's transaction, after its UPDATE, with the
// rows read before it. A reset with nothing to stop issues no statement, so
// Docker and Podman deployments and idle reclaim ticks pay nothing. A read error
// is the transaction's own failure, not a failed stop: return it and the
// recovery retries.
func RecoveredRuntimesAfterReset(tx *gorm.DB, rows []models.TaskRun) ([]RecoveredRuntime, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	for i := range rows {
		if stoppableRuntime(&rows[i]) {
			ids = append(ids, rows[i].ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	reset, err := taskRunIDsWithStatusTx(tx, ids, TaskStatusPending)
	if err != nil {
		return nil, err
	}
	return recoveredRuntimesFrom(rows, uuidSet(reset)), nil
}

// StopRecoveredRuntimes stops the previous attempt's pod for each runtime a
// recovery path re-pended (identity-and-access design §10.1: the persisted id
// is namespace-qualified, so recovery finds the pod without the policy; a
// legacy bare id resolves in CAESIUM_KUBERNETES_NAMESPACE). Callers pass only a
// stop list from recoveredRuntimesFrom, after their reset committed: such an
// attempt has lost its claim — its worker's renewal cancels it and its
// completion is fenced — so it can never be an attempt the system still counts
// as live. The stop is best effort: errors are logged and never fail or delay
// recovery.
func StopRecoveredRuntimes(reason string, runtimes []RecoveredRuntime) {
	if len(runtimes) == 0 {
		return
	}
	pending := append([]RecoveredRuntime(nil), runtimes...)
	launchRecoveredRuntimeStop(func() {
		stopKubernetesRuntimes(reason, pending)
	})
}

// stopKubernetesRuntimes stops each pod and returns the runtimes whose pod is
// gone (deleted, or already absent). A failed stop stays out of the result so
// the caller can retry it on a later tick.
func stopKubernetesRuntimes(reason string, runtimes []RecoveredRuntime) []RecoveredRuntime {
	if len(runtimes) == 0 {
		return nil
	}
	engine, err := kubernetesRecoveryEngine(context.Background())
	if err != nil {
		log.Warn("recovery: cannot stop the previous attempt's pods", "reason", reason, "count", len(runtimes), "error", err)
		return nil
	}
	gone := make([]RecoveredRuntime, 0, len(runtimes))
	for _, rt := range runtimes {
		stopErr := engine.Stop(&atom.EngineStopRequest{ID: rt.RuntimeID, Force: true, Timeout: recoveredRuntimeStopTimeout})
		switch {
		case stopErr == nil:
			log.Info("recovery: stopped the previous attempt's pod", "reason", reason, "task_run_id", rt.TaskRunID, "runtime_id", rt.RuntimeID)
			gone = append(gone, rt)
		case apierrors.IsNotFound(stopErr):
			log.Debug("recovery: previous attempt's pod already gone", "reason", reason, "task_run_id", rt.TaskRunID, "runtime_id", rt.RuntimeID)
			gone = append(gone, rt)
		default:
			log.Warn("recovery: failed to stop the previous attempt's pod", "reason", reason, "task_run_id", rt.TaskRunID, "runtime_id", rt.RuntimeID, "error", stopErr)
		}
	}
	return gone
}

// ReapAbandonedKubernetesPods deletes Kubernetes task pods whose worker is
// gone and will not delete them. A terminal row still naming a runtime is the
// attempt that reached Succeeded or Failed and lost its worker before that
// worker's Stop; the executor already snapshotted logs before the terminal
// write, and this path does not read them again. The runtime id stays on that
// row: it is the record of which pod ran. A running row is reaped only when
// its claim has lapsed and its run is no longer running, so no owner reclaim
// will fence and stop it, and that reset still clears the runtime id. A
// running attempt with an unexpired claim stays, including one that belongs
// to another run. runID limits the scan to one run; uuid.Nil scans every run.
// The leader's reclaim tick passes Nil so a finished run is still swept.
func (s *Store) ReapAbandonedKubernetesPods(runID uuid.UUID) {
	if s == nil || s.db == nil {
		return
	}
	now := time.Now().UTC()
	rows, err := s.abandonedKubernetesPods(runID, now)
	if err != nil {
		log.Warn("recovery: abandoned pod scan failed", "run_id", runID, "error", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	pending := append([]models.TaskRun(nil), rows...)
	launchRecoveredRuntimeStop(func() {
		targets := make([]RecoveredRuntime, 0, len(pending))
		confirmed := time.Now().UTC()
		for i := range pending {
			if remembered, ok := s.reapedPodIDs.Load(pending[i].ID); ok && remembered == pending[i].RuntimeID {
				continue
			}
			current, ok := s.confirmAbandonedKubernetesPod(pending[i], confirmed)
			if !ok {
				continue
			}
			targets = append(targets, RecoveredRuntime{
				TaskRunID: current.ID,
				Engine:    current.Engine,
				RuntimeID: current.RuntimeID,
			})
		}
		gone := stopKubernetesRuntimes("abandoned_task_pod", targets)
		s.forgetReapedRuntimeIDs(gone, time.Now().UTC())
	})
}

// abandonedKubernetesPods is the candidate scan for ReapAbandonedKubernetesPods.
// confirmAbandonedKubernetesPod is the same predicate, re-read before the delete.
func (s *Store) abandonedKubernetesPods(runID uuid.UUID, now time.Time) ([]models.TaskRun, error) {
	q := s.db.Model(&models.TaskRun{}).
		Select("id", "job_run_id", "engine", "runtime_id", "status", "claim_expires_at").
		Where("engine = ?", models.AtomEngineKubernetes).
		Where("runtime_id <> ''").
		Where(
			s.db.Where("status IN ?", terminalStatusStrings()).
				Or("status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ? AND EXISTS (SELECT 1 FROM job_runs WHERE job_runs.id = task_runs.job_run_id AND job_runs.status <> ?)",
					string(TaskStatusRunning), now, string(StatusRunning)),
		)
	if runID != uuid.Nil {
		q = q.Where("job_run_id = ?", runID)
	}
	var rows []models.TaskRun
	err := q.Find(&rows).Error
	return rows, err
}

// confirmAbandonedKubernetesPod reports whether row still names a pod its
// worker will not delete. A claim renewed since the scan, or a runtime id
// replaced by a live attempt, is no longer this pod's to stop.
func (s *Store) confirmAbandonedKubernetesPod(row models.TaskRun, now time.Time) (models.TaskRun, bool) {
	var current models.TaskRun
	err := s.db.Select("id", "job_run_id", "engine", "runtime_id", "status", "claim_expires_at").
		First(&current, "id = ?", row.ID).Error
	if err != nil {
		return models.TaskRun{}, false
	}
	if current.Engine != models.AtomEngineKubernetes || strings.TrimSpace(current.RuntimeID) == "" {
		return models.TaskRun{}, false
	}
	if current.RuntimeID != row.RuntimeID {
		return models.TaskRun{}, false
	}
	if IsTerminal(TaskStatus(current.Status)) {
		return current, true
	}
	if current.Status != string(TaskStatusRunning) || current.ClaimExpiresAt == nil || !current.ClaimExpiresAt.Before(now) {
		return models.TaskRun{}, false
	}
	var runStatus string
	if err := s.db.Model(&models.JobRun{}).Select("status").Where("id = ?", current.JobRunID).Scan(&runStatus).Error; err != nil {
		return models.TaskRun{}, false
	}
	if runStatus == string(StatusRunning) || runStatus == "" {
		return models.TaskRun{}, false
	}
	return current, true
}

// forgetReapedRuntimeIDs remembers a deleted pod so the next tick does not
// stop it again. A terminal row keeps its runtime id: that is which pod ran.
// Only a lapsed running attempt, whose row is being reset, has the id cleared.
func (s *Store) forgetReapedRuntimeIDs(gone []RecoveredRuntime, now time.Time) {
	if s == nil || s.db == nil || len(gone) == 0 {
		return
	}
	for _, rt := range gone {
		err := s.db.Model(&models.TaskRun{}).
			Where("id = ? AND runtime_id = ? AND status NOT IN ?", rt.TaskRunID, rt.RuntimeID, terminalStatusStrings()).
			Where("status = ? AND claim_expires_at IS NOT NULL AND claim_expires_at < ? AND EXISTS (SELECT 1 FROM job_runs WHERE job_runs.id = task_runs.job_run_id AND job_runs.status <> ?)",
				string(TaskStatusRunning), now, string(StatusRunning)).
			Update("runtime_id", "").Error
		if err != nil {
			log.Warn("recovery: failed to clear a reaped runtime id", "task_run_id", rt.TaskRunID, "runtime_id", rt.RuntimeID, "error", err)
			continue
		}
		s.reapedPodIDs.Store(rt.TaskRunID, rt.RuntimeID)
	}
}

// SetRecoveryEngineForTesting replaces the engine recovery stops pods with and
// runs the stops synchronously, so tests in other packages can observe them.
// It returns the restore function.
func SetRecoveryEngineForTesting(newEngine func(context.Context) (atom.Engine, error)) (restore func()) {
	prevEngine, prevLaunch := kubernetesRecoveryEngine, launchRecoveredRuntimeStop
	kubernetesRecoveryEngine = newEngine
	launchRecoveredRuntimeStop = func(stop func()) { stop() }
	return func() {
		kubernetesRecoveryEngine, launchRecoveredRuntimeStop = prevEngine, prevLaunch
	}
}

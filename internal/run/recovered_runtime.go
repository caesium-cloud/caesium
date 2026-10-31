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
// it, so a stop can never reach a row the system still counts.
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
		engine, err := kubernetesRecoveryEngine(context.Background())
		if err != nil {
			log.Warn("recovery: cannot stop the previous attempt's pods", "reason", reason, "count", len(pending), "error", err)
			return
		}
		for _, rt := range pending {
			stopErr := engine.Stop(&atom.EngineStopRequest{ID: rt.RuntimeID, Force: true, Timeout: recoveredRuntimeStopTimeout})
			switch {
			case stopErr == nil:
				log.Info("recovery: stopped the previous attempt's pod", "reason", reason, "task_run_id", rt.TaskRunID, "runtime_id", rt.RuntimeID)
			case apierrors.IsNotFound(stopErr):
				log.Debug("recovery: previous attempt's pod already gone", "reason", reason, "task_run_id", rt.TaskRunID, "runtime_id", rt.RuntimeID)
			default:
				log.Warn("recovery: failed to stop the previous attempt's pod", "reason", reason, "task_run_id", rt.TaskRunID, "runtime_id", rt.RuntimeID, "error", stopErr)
			}
		}
	})
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

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// accessModeReadWriteOnce is the Kubernetes access mode for a volume that one
// node can mount read-write. On a Docker or Podman worker that node is the
// daemon, so every step that mounts the volume has to run there.
const accessModeReadWriteOnce = "ReadWriteOnce"

// rwoBinding is one task's shared ReadWriteOnce volumes and, when an earlier
// task in the run was already placed on a worker, that worker.
type rwoBinding struct {
	volumes []string
	// worker is set when a different step that mounts one of volumes already
	// has a claim. Empty means this task is the first placement.
	worker string
	// conflict is set when those volumes are already claimed on different
	// workers. The task cannot satisfy both, so it fails.
	conflict string
}

type affinityDB interface {
	DB() *gorm.DB
}

// rwoBindings reads the run's task rows and atom specs. A volume counts only
// when its access mode is ReadWriteOnce, the source is not per-container
// scratch, and at least one other step mounts it. A store that cannot see the
// catalog (test stubs) returns nil, and placement stays round-robin.
func (l *DispatchLoop) rwoBindings(ctx context.Context, runID uuid.UUID) (map[uuid.UUID]rwoBinding, error) {
	holder, ok := l.cfg.Store.(affinityDB)
	if !ok || holder == nil || holder.DB() == nil {
		return nil, nil
	}
	db := holder.DB().WithContext(ctx)

	var rows []models.TaskRun
	if err := db.Where("job_run_id = ?", runID).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	specs, err := atomSpecs(db, rows)
	if err != nil {
		return nil, err
	}

	type mounted struct {
		row     models.TaskRun
		volumes []string
	}
	mountedRows := make([]mounted, 0, len(rows))
	stepsByVolume := make(map[string]map[uuid.UUID]struct{})
	for i := range rows {
		names := rwoVolumeNames(specs[rows[i].AtomID])
		mountedRows = append(mountedRows, mounted{row: rows[i], volumes: names})
		for _, name := range names {
			steps := stepsByVolume[name]
			if steps == nil {
				steps = make(map[uuid.UUID]struct{})
				stepsByVolume[name] = steps
			}
			steps[rows[i].TaskID] = struct{}{}
		}
	}

	catalogRows := make(map[uuid.UUID]int, len(rows))
	for _, m := range mountedRows {
		catalogRows[m.row.TaskID]++
	}

	out := make(map[uuid.UUID]rwoBinding)
	for _, m := range mountedRows {
		shared := make([]string, 0, len(m.volumes))
		for _, name := range m.volumes {
			if len(stepsByVolume[name]) < 2 {
				continue
			}
			shared = append(shared, name)
		}
		if len(shared) == 0 {
			continue
		}
		sort.Strings(shared)

		workers := make(map[string]struct{})
		for _, name := range shared {
			for _, other := range mountedRows {
				// A fan-out sibling is another row of this catalog task. Its
				// claim is where the volume was opened. Skipping every row
				// with this task id hides that worker, and the next instance
				// round-robins onto a different one.
				if other.row.ID == m.row.ID {
					continue
				}
				if !containsString(other.volumes, name) {
					continue
				}
				if worker := strings.TrimSpace(other.row.ClaimedBy); worker != "" {
					workers[worker] = struct{}{}
				}
			}
		}
		binding := rwoBinding{volumes: shared}
		switch len(workers) {
		case 0:
		case 1:
			for worker := range workers {
				binding.worker = worker
			}
		default:
			list := make([]string, 0, len(workers))
			for worker := range workers {
				list = append(list, worker)
			}
			sort.Strings(list)
			binding.conflict = fmt.Sprintf(
				"shared ReadWriteOnce volumes %s are already placed on different workers (%s); refusing to place the task",
				strings.Join(shared, ", "), strings.Join(list, ", "))
		}
		out[m.row.ID] = binding
		// An unfanned task is dispatched by its catalog id. A fan-out group
		// has many rows for one catalog id, so only the row id is safe there.
		if catalogRows[m.row.TaskID] == 1 && m.row.TaskID != m.row.ID {
			out[m.row.TaskID] = binding
		}
	}
	return out, nil
}

func atomSpecs(db *gorm.DB, rows []models.TaskRun) (map[uuid.UUID]container.Spec, error) {
	seen := make(map[uuid.UUID]struct{}, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for i := range rows {
		id := rows[i].AtomID
		if id == uuid.Nil {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	out := make(map[uuid.UUID]container.Spec, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var atoms []models.Atom
	if err := db.Where("id IN ?", ids).Find(&atoms).Error; err != nil {
		return nil, err
	}
	for i := range atoms {
		if len(atoms[i].Spec) == 0 {
			continue
		}
		var spec container.Spec
		if err := json.Unmarshal(atoms[i].Spec, &spec); err != nil {
			return nil, fmt.Errorf("atom %s spec: %w", atoms[i].ID, err)
		}
		out[atoms[i].ID] = spec
	}
	return out, nil
}

// rwoVolumeNames returns the logical names of ReadWriteOnce mounts that can
// outlive one container. Per-step scratch (tmpfs, a claim template, emptyDir)
// is not a volume two steps share, even when the access mode says otherwise.
func rwoVolumeNames(spec container.Spec) []string {
	seen := make(map[string]struct{})
	names := make([]string, 0, len(spec.ResolvedVolumeMounts))
	for _, mount := range spec.ResolvedVolumeMounts {
		if strings.TrimSpace(mount.AccessMode) != accessModeReadWriteOnce {
			continue
		}
		if perInstanceVolume(mount) {
			continue
		}
		name := strings.TrimSpace(mount.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// perInstanceVolume matches the sources internal/jobdef/lint treats as private
// to one container or pod. Those bytes are not on a shared host volume.
func perInstanceVolume(mount container.VolumeMount) bool {
	switch mount.Type {
	case container.VolumeMountTypeTmpfs, container.VolumeMountTypeClaimTemplate:
		return true
	case container.VolumeMountTypeVolumeSource:
		if len(mount.VolumeSource) != 1 {
			return false
		}
		if _, emptyDir := mount.VolumeSource["emptyDir"]; emptyDir {
			return true
		}
		_, ephemeral := mount.VolumeSource["ephemeral"]
		return ephemeral
	default:
		return false
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// bindingWorker combines the durable claim with workers this process already
// chose for the same volumes. They have to name the same worker. A
// disagreement means the volume was opened in two places, so the task fails
// instead of picking one.
func (l *DispatchLoop) bindingWorker(runID uuid.UUID, binding rwoBinding) (string, string) {
	if binding.conflict != "" {
		return "", binding.conflict
	}
	worker := binding.worker
	for _, volume := range binding.volumes {
		remembered := l.rwoWorker(runID, volume)
		if remembered == "" {
			continue
		}
		if worker == "" {
			worker = remembered
			continue
		}
		if worker != remembered {
			return "", fmt.Sprintf(
				"shared ReadWriteOnce volume %s is already placed on worker %s, not %s; refusing to place the task",
				volume, worker, remembered)
		}
	}
	return worker, ""
}

func (l *DispatchLoop) rwoWorker(runID uuid.UUID, volume string) string {
	l.rwoMu.Lock()
	defer l.rwoMu.Unlock()
	return l.rwoPinned[runID][volume]
}

// rememberRWO records the worker chosen for these volumes so a later step in
// the same run, including one selected later in this tick, follows it. An
// existing pin is kept: placement never moves to a second worker.
func (l *DispatchLoop) rememberRWO(runID uuid.UUID, volumes []string, worker string) {
	worker = strings.TrimSpace(worker)
	if worker == "" || len(volumes) == 0 {
		return
	}
	l.rwoMu.Lock()
	defer l.rwoMu.Unlock()
	pins := l.rwoPinned[runID]
	if pins == nil {
		pins = make(map[string]string, len(volumes))
		l.rwoPinned[runID] = pins
	}
	for _, volume := range volumes {
		if current := pins[volume]; current != "" && current != worker {
			continue
		}
		pins[volume] = worker
	}
}

func (l *DispatchLoop) forgetRWO(owned map[uuid.UUID]int64) {
	l.rwoMu.Lock()
	defer l.rwoMu.Unlock()
	for runID := range l.rwoPinned {
		if _, still := owned[runID]; !still {
			delete(l.rwoPinned, runID)
		}
	}
}

func (l *DispatchLoop) setMemberPeers(peers []peer) {
	l.memberMu.Lock()
	defer l.memberMu.Unlock()
	l.memberPeers = append([]peer(nil), peers...)
}

// findMemberPeer looks up a pinned worker in the full membership, including
// peers the healthy-peer filter has benched. A benched peer is retried in
// place. A peer that is no longer a member is gone.
func (l *DispatchLoop) findMemberPeer(nodeID string, fallback []peer) (peer, bool) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return peer{}, false
	}
	l.memberMu.Lock()
	members := l.memberPeers
	l.memberMu.Unlock()
	if p, ok := peerByID(members, nodeID); ok {
		return p, true
	}
	return peerByID(fallback, nodeID)
}

func peerByID(peers []peer, nodeID string) (peer, bool) {
	for _, p := range peers {
		if p.nodeID == nodeID {
			return p, true
		}
	}
	return peer{}, false
}

func missingRWOWorkerError(worker string, volumes []string) error {
	return fmt.Errorf(
		"worker %s is not in the cluster; refusing to place the task elsewhere because it shares ReadWriteOnce volume %s with an earlier step on that worker",
		worker, strings.Join(volumes, ", "))
}

// failColocatedTask records a terminal failure for a task that cannot be
// placed without opening a host-local volume on the wrong worker. The
// in-memory owner is updated when this process owns the run, so the ready
// queue does not dispatch the task again. Otherwise the SQL row is failed.
func (l *DispatchLoop) failColocatedTask(ctx context.Context, runID uuid.UUID, candidate dispatchCandidate, failure error) {
	if failure == nil {
		return
	}
	log.Error("dispatch loop: ReadWriteOnce co-location failed",
		"run_id", runID,
		"task_id", candidate.taskID,
		"task_run_id", candidate.taskRunID,
		"error", failure,
	)
	if l.cfg.OwnerManager != nil && l.cfg.OwnerManager.Owns(runID) {
		if _, err := l.cfg.OwnerManager.CompleteInstance(
			runID, candidate.taskID, candidate.taskRunID,
			run.TaskStatusFailed, "failure", failure.Error(), "", nil, nil, nil,
		); err != nil {
			log.Error("dispatch loop: could not fail ReadWriteOnce task",
				"run_id", runID, "task_id", candidate.taskID, "error", err)
		}
		return
	}
	failer, ok := l.cfg.Store.(interface {
		FailTask(runID, taskID uuid.UUID, failure error) error
	})
	if !ok {
		log.Error("dispatch loop: store cannot fail a task whose ReadWriteOnce worker is gone",
			"run_id", runID, "task_id", candidate.taskID)
		return
	}
	ref := candidate.executionRef
	if ref == uuid.Nil {
		ref = candidate.taskID
	}
	if err := failer.FailTask(runID, ref, failure); err != nil && ctx.Err() == nil {
		log.Error("dispatch loop: could not fail ReadWriteOnce task",
			"run_id", runID, "task_id", candidate.taskID, "error", err)
	}
}

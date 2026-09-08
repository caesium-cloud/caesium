package run

import (
	"errors"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// admitDataHoldTx is the data circuit breaker's DOWNSTREAM gate, called from
// Store.admit inside the transaction that would otherwise insert the run.
//
// It answers one question: does this job declare, under `datasets.consumes`, a
// dataset that is currently HELD? If so the default disposition is
// skip-with-reason — the run is created directly in terminal `skipped` with
// SkipReason "dataset_hold:<ns>/<name>" and a full set of skipped task rows,
// because an invisible non-run would be silent poison's evil twin: run history
// must say why nothing happened.
//
// Returns (nil, nil) when the gate does not apply — the flag is off, the job
// consumes nothing held, or the job opted out with metadata.onUpstreamHold:
// run — and the caller proceeds to ordinary concurrency admission.
//
// Cost: nothing at all on a deployment with the feature off; one indexed join
// per admission with it on; a second single-row read only on a hit.
//
// BACKFILLS ARE GATED TOO. The gate sits ahead of admit's backfill
// early-return: a backfill of a consumer over held upstream data would produce
// exactly the bad output the breaker exists to stop, and each refused backfill
// run leaves a visible skipped row saying so.
//
// LIMITATION (v1, deliberate): the check happens at ADMISSION only. A hold
// opened while a run is already in flight does not stop that run's remaining
// tasks — mid-run task starts do not re-check holds. The run that was admitted
// under clean upstream data finishes under it.
func (s *Store) admitDataHoldTx(tx *gorm.DB, model *models.JobRun) (*admissionResult, error) {
	// startRun retries the whole transaction on contention, and a previous
	// attempt may have stamped this model terminal before its commit failed.
	// If the hold was released in between, the gate now declines and the model
	// would go on to be inserted as a `skipped` run that nobody skipped. Reset
	// first, unconditionally: the fresh-run shape is what every other admission
	// path expects to receive.
	model.Status = string(StatusRunning)
	model.SkipReason = ""
	model.CompletedAt = nil

	if !DataAssertionsEnabled() {
		return nil, nil
	}

	hold, err := activeHoldForConsumerTx(tx, model.JobID)
	if err != nil {
		return nil, err
	}
	if hold == nil {
		return nil, nil
	}

	var job models.Job
	jobFound := true
	if err := tx.Select("on_upstream_hold", toAnySlice(registerTasksJobColumns)...).
		First(&job, "id = ?", model.JobID).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
		jobFound = false
	}

	// metadata.onUpstreamHold is read off the PERSISTED scalar column, written
	// when the jobdef was applied — never by re-parsing the stored definition,
	// which is not something an admission transaction can afford. Empty is the
	// documented "skip" default; only the literal "run" opts out.
	if strings.TrimSpace(job.OnUpstreamHold) == jobdef.OnUpstreamHoldRun {
		log.Warn("job runs on held upstream data by declaration (metadata.onUpstreamHold: run)",
			"job_id", model.JobID, "job_alias", job.Alias, "dataset", hold.Name, "hold_id", hold.ID)
		return nil, nil
	}

	// policyOnly (AdmitRun) is deliberately NOT honoured here. That flag means
	// "only act if a concurrency policy has something to say"; the breaker
	// always has something to say, and its answer is a real, recorded decision
	// rather than a policy probe.
	events, skipped, err := s.insertHeldRunTx(tx, model, job, jobFound, hold)
	if err != nil {
		return nil, err
	}

	held := &admissionResult{
		decision:   admissionHeld,
		jobAlias:   job.Alias,
		skipReason: DatasetHoldSkipReason(hold.Namespace, hold.Name),
		heldBy:     hold,
		heldEvents: events,
	}
	log.Info("dataset hold gated a downstream run",
		"job_id", model.JobID, "run_id", model.ID, "dataset", hold.Name,
		"hold_id", hold.ID, "skipped_tasks", skipped)
	return held, nil
}

// insertHeldRunTx writes the whole terminal run — the JobRun row, one TaskRun
// row per catalog task, each flipped to `skipped`, and the events — in the
// caller's transaction.
//
// Why the task rows exist. A run with no tasks is a shape no reader tolerates:
// the DAG view, run history and the task-scoped `caesium why` all expect rows.
// For a normal run the LOCAL EXECUTOR registers them after start; this run never
// reaches an executor, so admission has to do it, which is why registerTasksTx
// was factored out of RegisterTasks.
//
// Fan-out shape. A fanned step is registered exactly as a fresh run registers it
// before its producer has emitted: ONE unexpanded template row at PartitionIndex
// 0 with PartitionCount 0. No per-partition instances are synthesised, because
// the producer never ran and the partition list never existed — the same shape a
// fanned group already has whenever its producer does not run, which every
// reader already knows how to render.
//
// The task rows carry OutstandingPredecessors 0 and emit no task_ready: they are
// terminal before this transaction commits, so no worker or dispatcher can ever
// observe them pending, and announcing readiness for work that will never be
// claimed would be a lie in the event stream.
func (s *Store) insertHeldRunTx(
	tx *gorm.DB,
	model *models.JobRun,
	job models.Job,
	jobFound bool,
	hold *models.DatasetHold,
) ([]event.Event, int, error) {
	now := time.Now().UTC()
	model.Status = string(StatusSkipped)
	model.SkipReason = DatasetHoldSkipReason(hold.Namespace, hold.Name)
	model.CompletedAt = &now
	model.UpdatedAt = now

	// A plain INSERT, not insertRunIfSlotTx: a run that is terminal on arrival
	// occupies no concurrency slot, so gating it on one would make the breaker's
	// verdict depend on how busy the job happens to be.
	if err := tx.Create(model).Error; err != nil {
		return nil, 0, err
	}

	inputs, err := catalogRegisterInputsTx(tx, model.JobID)
	if err != nil {
		return nil, 0, err
	}

	var events []event.Event
	evt, err := s.appendRunHeldUpstreamEventTx(tx, model, job, hold, len(inputs))
	if err != nil {
		return nil, 0, err
	}
	if evt != nil {
		events = append(events, *evt)
	}

	if len(inputs) == 0 {
		// A job with no catalog tasks (retired steps, or a definition still
		// being applied) still gets its run row and its reason.
		return events, 0, nil
	}

	// The dbWriteCounts instrumentation is deliberately not accumulated on this
	// path: it is committed by the caller of RegisterTasks after ITS retry
	// budget resolves, and this write lives inside somebody else's transaction.
	var counts dbWriteCounts
	_, rows, err := s.registerTasksTx(tx, model.ID, *model, job, jobFound, inputs, false, &counts)
	if err != nil {
		return nil, 0, err
	}

	reason := datasetHoldTaskSkipReason(hold)
	skipped := 0
	for i := range rows {
		marked, err := s.markInstanceSkippedWhereTx(tx, model.ID, &rows[i], reason,
			"status IN ?", []any{[]string{string(TaskStatusPending)}}, nil, &events, &counts)
		if err != nil {
			return nil, 0, err
		}
		if marked {
			skipped++
		}
	}
	return events, skipped, nil
}

// catalogRegisterInputsTx loads a job's catalog tasks and their atoms as
// RegisterTaskInputs, inside the caller's transaction.
//
// Every input is at PartitionIndex 0 with OutstandingPredecessors 0 — the DAG's
// edges are irrelevant to a run in which nothing will execute, and reading them
// would only add queries to a path whose whole point is to end quickly.
func catalogRegisterInputsTx(tx *gorm.DB, jobID uuid.UUID) ([]RegisterTaskInput, error) {
	var tasks []models.Task
	if err := tx.Where("job_id = ?", jobID).Order("created_at ASC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}

	atomIDs := make([]uuid.UUID, 0, len(tasks))
	for i := range tasks {
		atomIDs = append(atomIDs, tasks[i].AtomID)
	}
	var atoms []models.Atom
	if err := tx.Where("id IN ?", atomIDs).Find(&atoms).Error; err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]*models.Atom, len(atoms))
	for i := range atoms {
		byID[atoms[i].ID] = &atoms[i]
	}

	inputs := make([]RegisterTaskInput, 0, len(tasks))
	for i := range tasks {
		atom, ok := byID[tasks[i].AtomID]
		if !ok {
			// A step whose atom is gone cannot be materialised; the rest of the
			// run's history is still worth writing.
			log.Warn("skipping a catalog task with no atom while recording a hold-skipped run",
				"job_id", jobID, "task_id", tasks[i].ID)
			continue
		}
		inputs = append(inputs, RegisterTaskInput{
			Task:                    &tasks[i],
			Atom:                    atom,
			OutstandingPredecessors: 0,
		})
	}
	return inputs, nil
}

// appendRunHeldUpstreamEventTx persists run_held_upstream for a gated run.
func (s *Store) appendRunHeldUpstreamEventTx(
	tx *gorm.DB,
	model *models.JobRun,
	job models.Job,
	hold *models.DatasetHold,
	tasks int,
) (*event.Event, error) {
	if s.eventStore == nil {
		return nil, nil
	}
	payload, err := encodeEventPayload(RunHeldUpstreamEvent{
		HoldID:       hold.ID,
		Namespace:    hold.Namespace,
		Dataset:      hold.Name,
		SkipReason:   model.SkipReason,
		JobAlias:     job.Alias,
		SkippedTasks: tasks,
	})
	if err != nil {
		return nil, err
	}
	evt := &event.Event{
		Type:      event.TypeRunHeldUpstream,
		JobID:     model.JobID,
		RunID:     model.ID,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	}
	if err := s.eventStore.AppendTx(tx, evt); err != nil {
		return nil, err
	}
	return evt, nil
}

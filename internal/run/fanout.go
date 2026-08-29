package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ExpandedInstance is one materialized fan-out TaskRun the local executor
// needs because it never re-reads the store mid-run.
type ExpandedInstance struct {
	TaskRunID               uuid.UUID
	TaskID                  uuid.UUID
	PartitionIndex          int
	Partition               pkgtask.Partition
	OutstandingPredecessors int
}

// FanOutExpansion is the payload CompleteTaskResult returns so all three
// advancement paths observe the same instance set.
type FanOutExpansion struct {
	ProducerTaskID uuid.UUID
	Partitions     []pkgtask.Partition
	Groups         []ExpandedGroup
}

// ExpandedGroup is the N instances of one fanned successor step.
type ExpandedGroup struct {
	TaskID      uuid.UUID
	TaskName    string
	OnEmpty     string
	Skipped     bool
	MaxParallel int
	Instances   []ExpandedInstance
	Dependents  map[string][]string
}

func fanOutServerCap() int {
	cap := env.Variables().FanOutMaxPartitions
	if cap <= 0 {
		return jobdefschema.DefaultFanOutMaxPartitions
	}
	return cap
}

func (s *Store) persistProducerPartitionsTx(tx *gorm.DB, producerID uuid.UUID, parts []pkgtask.Partition) error {
	if len(parts) == 0 {
		return nil
	}
	encoded, err := pkgtask.EncodePartitions(parts)
	if err != nil {
		return fmt.Errorf("run: encode partitions: %w", err)
	}
	return tx.Model(&models.TaskRun{}).Where("id = ?", producerID).Update("partitions", datatypes.JSON(encoded)).Error
}

func (s *Store) expandFanOutSuccessorsTx(
	tx *gorm.DB,
	runID, producerTaskID uuid.UUID,
	producerRow *models.TaskRun,
	producerName string,
	successorTaskIDs []uuid.UUID,
	partitions []pkgtask.Partition,
	pendingEvents *[]event.Event,
	counts *dbWriteCounts,
) (*FanOutExpansion, error) {
	return s.expandFanOutSuccessors(tx, runID, producerTaskID, producerRow, producerName, successorTaskIDs, partitions, pendingEvents, counts, true)
}

// PlanFanOutExpansion validates partitions and assigns instance IDs without
// writing rows. The owner in-memory path applies this to RunState first, then
// persists the same IDs inside CompleteTaskOwner.
//
// The producer is addressed by its catalog task id, which resolves its TaskRun
// only while that task is UNFANNED. Use PlanFanOutExpansionForRow when the
// producer may itself be a fanned instance.
func (s *Store) PlanFanOutExpansion(runID, producerTaskID uuid.UUID, partitions []pkgtask.Partition) (*FanOutExpansion, error) {
	return s.PlanFanOutExpansionForRow(runID, producerTaskID, producerTaskID, partitions)
}

// PlanFanOutExpansionForRow is PlanFanOutExpansion with the producer's two
// identities stated separately, because the function genuinely needs both:
// producerTaskID is the CATALOG task (the Task row whose name fanOut.from
// matches, and the from_task_id the successor edges hang off), while
// producerRowRef names the TaskRun that actually ran.
//
// Collapsing them broke every fanned producer. A fanned step that emits
// partitions of its own — and, more commonly, ANY instance completing through
// OwnerManager.CompleteInstance, which plans an expansion on every success —
// resolves its catalog id to N rows, so the single-row load failed with
// ErrAmbiguousTaskRun. The owner treats a planning error as the task having
// FAILED, so a partition that succeeded was recorded failed, and under the
// default fail_fast policy that killed its pending siblings and the run. The
// error surfaced only as a `record not found` line in the query log.
//
// producerRowRef follows the usual TaskRun-primary-key-or-catalog-task-ID
// contract, so an unfanned producer may still pass its catalog id.
func (s *Store) PlanFanOutExpansionForRow(runID, producerTaskID, producerRowRef uuid.UUID, partitions []pkgtask.Partition) (*FanOutExpansion, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run: plan fan-out: nil store")
	}
	var producer models.Task
	if err := s.db.First(&producer, "id = ?", producerTaskID).Error; err != nil {
		return nil, err
	}
	if producerRowRef == uuid.Nil {
		producerRowRef = producerTaskID
	}
	producerRow, err := loadTaskRunByIDOrUnique(s.db, runID, producerRowRef)
	if err != nil {
		return nil, err
	}
	var edges []models.TaskEdge
	if err := s.db.Where("from_task_id = ?", producerTaskID).Find(&edges).Error; err != nil {
		return nil, err
	}
	succIDs := make([]uuid.UUID, 0, len(edges))
	for _, e := range edges {
		succIDs = append(succIDs, e.ToTaskID)
	}
	return s.expandFanOutSuccessors(s.db, runID, producerTaskID, producerRow, producer.Name, succIDs, partitions, nil, nil, false)
}

// HasAnyFanOutConsumerForRun reports whether ANY task in runID's job declares
// a fanOut config at all. It is the cheap pre-filter HasFanOutSuccessor's
// callers (internal/job/job.go, internal/worker/runtime_executor.go) run
// first: on an ordinary run that never uses fan-out — the overwhelming
// majority of cache hits — this single indexed join is the ENTIRE cost of the
// F7 cache-hit gate, and HasFanOutSuccessor's fuller per-producer check never
// runs at all.
func (s *Store) HasAnyFanOutConsumerForRun(runID uuid.UUID) (bool, error) {
	if hasAnyFanOutConsumerErrForTest != nil {
		return false, hasAnyFanOutConsumerErrForTest
	}
	if s == nil || s.db == nil {
		return false, fmt.Errorf("run: has any fan-out consumer: nil store")
	}
	var count int64
	err := s.db.Model(&models.Task{}).
		Joins("JOIN job_runs ON job_runs.job_id = tasks.job_id").
		Where("job_runs.id = ? AND tasks.fan_out_config IS NOT NULL AND tasks.fan_out_config != ''", runID).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// HasFanOutSuccessor reports whether any task depending directly on
// producerTaskID declares `fanOut.from` naming the producer AND still carries
// an unexpanded, expandable template row for runID — i.e. whether completing
// producerTaskID is expected to expand a fanned group right now. It applies
// the identical gates expandFanOutSuccessors uses per successor
// (fo.From == producer's name; fanOutTemplateExpandable), standalone, so a
// cache-hit call site can decide BEFORE committing a completion whether an
// entry with no recorded partition list (cache.Entry.Partitions == nil) is
// safe to treat as a hit: a nil list only means "onEmpty" when something
// downstream is actually still waiting to fan out from this producer. See
// internal/job/job.go and internal/worker/runtime_executor.go's cache-hit
// sites — both call HasAnyFanOutConsumerForRun first, so this heavier
// per-successor query (a joined lookup plus one template check per matching
// successor) only runs for runs that use fan-out somewhere at all.
//
// A malformed FanOutConfig on a successor is not this helper's error to
// raise — expandFanOutSuccessors will surface it, correctly, at the point it
// actually tries to expand that successor. Here it is treated as "does not
// match", so a single bad successor never blocks a cache hit that has
// nothing to do with it. Likewise a successor with no template row for this
// run, or one already expanded into N sibling instances (ErrAmbiguousTaskRun),
// is treated as "not currently expandable" rather than an error — mirroring
// fanOutTemplateExpandable's own posture in expandFanOutSuccessors — so a
// group the DAG already resolved (branch-skipped, cancelled, already
// materialized) never forces a needless producer re-run.
// hasFanOutSuccessorErrForTest, when non-nil, makes HasFanOutSuccessor return
// it instead of running its real query. Test-only seam, set exclusively
// through SetHasFanOutSuccessorErrForTest: internal/job and internal/worker
// tests use it to exercise the fail-CLOSED behaviour their cache-hit gates
// apply when this lookup itself errors (P2-A) — there is no narrower DB-level
// fault to inject, since this query's tables (tasks, task_edges) are the same
// ones ordinary DAG advancement needs, so sabotaging the schema breaks the
// rest of the run too, not just this one call. Never set outside a test, and
// always reset (nil) by the setting test's own cleanup — it is package-level
// and therefore visible to every *Store in the process, though safe across
// packages in practice because `go test` compiles and runs each package's
// tests as its own process.
var hasFanOutSuccessorErrForTest error

// hasAnyFanOutConsumerErrForTest is the same kind of process-wide fault
// injection for HasAnyFanOutConsumerForRun (the per-run pre-filter), so a
// test can prove the OUTER lookup error fails closed too. Same rules: never
// set outside a test, always reset by the setting test's own cleanup.
var hasAnyFanOutConsumerErrForTest error

// SetHasAnyFanOutConsumerErrForTest overrides HasAnyFanOutConsumerForRun's
// result process-wide for exactly as long as the override is set. Pass nil to
// restore the real behaviour; callers MUST do so (typically via t.Cleanup).
func SetHasAnyFanOutConsumerErrForTest(err error) {
	hasAnyFanOutConsumerErrForTest = err
}

// SetHasFanOutSuccessorErrForTest overrides HasFanOutSuccessor's result
// process-wide for exactly as long as the override is set — see
// hasFanOutSuccessorErrForTest. Pass nil to restore the real behaviour;
// callers MUST do so (typically via t.Cleanup) before their test returns.
func SetHasFanOutSuccessorErrForTest(err error) {
	hasFanOutSuccessorErrForTest = err
}

func (s *Store) HasFanOutSuccessor(runID, producerTaskID uuid.UUID) (bool, error) {
	if hasFanOutSuccessorErrForTest != nil {
		return false, hasFanOutSuccessorErrForTest
	}
	if s == nil || s.db == nil {
		return false, fmt.Errorf("run: has fan-out successor: nil store")
	}
	var producer models.Task
	if err := s.db.Select("id", "name").First(&producer, "id = ?", producerTaskID).Error; err != nil {
		return false, err
	}

	// Pre-filtered to successors that even DECLARE a fan-out config, in one
	// joined query. The N+1 this replaces (a bare Find(edges) followed by one
	// First(succ) per outgoing edge) was the dominant cost of this check on
	// every cache hit that reached it.
	var succs []models.Task
	if err := s.db.
		Joins("JOIN task_edges ON task_edges.to_task_id = tasks.id").
		Where("task_edges.from_task_id = ? AND tasks.fan_out_config IS NOT NULL AND tasks.fan_out_config != ''", producerTaskID).
		Find(&succs).Error; err != nil {
		return false, err
	}

	for _, succ := range succs {
		fo, decodeErr := decodeFanOutConfig(succ.FanOutConfig)
		if decodeErr != nil || fo == nil || fo.From != producer.Name {
			continue
		}
		template, err := loadUniqueTaskRun(s.db, runID, succ.ID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrAmbiguousTaskRun) {
				continue
			}
			return false, err
		}
		if fanOutTemplateExpandable(template) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) expandFanOutSuccessors(
	tx *gorm.DB,
	runID, producerTaskID uuid.UUID,
	producerRow *models.TaskRun,
	producerName string,
	successorTaskIDs []uuid.UUID,
	partitions []pkgtask.Partition,
	pendingEvents *[]event.Event,
	counts *dbWriteCounts,
	persist bool,
) (*FanOutExpansion, error) {
	if producerRow == nil {
		return nil, nil
	}

	// Canonicalize ONCE, before anything reads the list. The in-group indegree
	// comes from pkgtask.ValidatePartitionGraph, which canonicalizes dependsOn
	// internally, while the persisted partition_depends_on used to be marshalled
	// from the RAW partition — so a dependency written " a" produced indegree 1
	// (the graph saw "a") beside a persisted [" a"] that
	// decrementInGroupDependentsTx, which matches d == completed.PartitionValue,
	// can never satisfy. The dependent instance then waits forever on a
	// decrement that never comes.
	//
	// The marker parser canonicalizes at the source, so this covers the
	// non-parser producers: a cached producer replaying entry.Partitions and the
	// owner's replan path. Normalizing here also makes the producer's persisted
	// `partitions` column and the planned expansion agree with the graph.
	normalized, err := pkgtask.NormalizePartitions(partitions)
	if err != nil {
		return nil, asFanOutProducerError(err)
	}
	partitions = normalized

	if persist {
		if err := s.persistProducerPartitionsTx(tx, producerRow.ID, partitions); err != nil {
			return nil, err
		}
	}

	expansion := &FanOutExpansion{ProducerTaskID: producerTaskID, Partitions: partitions}
	serverCap := fanOutServerCap()

	for _, succID := range successorTaskIDs {
		var succTask models.Task
		if err := tx.First(&succTask, "id = ?", succID).Error; err != nil {
			return nil, err
		}
		fo, err := decodeFanOutConfig(succTask.FanOutConfig)
		if err != nil {
			return nil, err
		}
		if fo == nil || fo.From != producerName {
			continue
		}

		cap := fo.MaxPartitions
		if cap <= 0 || cap > serverCap {
			cap = serverCap
		}
		if len(partitions) > cap {
			// partitions[cap] is the first element past the cap and always exists
			// here, so it names the offending key without a second bounds test.
			return nil, &pkgtask.PartitionError{Msg: fmt.Sprintf("partition list exceeds count cap %d (offending key %q)", cap, partitions[cap].Key)}
		}

		graph, err := pkgtask.ValidatePartitionGraph(partitions)
		if err != nil {
			return nil, asFanOutProducerError(err)
		}

		group := ExpandedGroup{TaskID: succID, TaskName: succTask.Name, OnEmpty: fo.OnEmpty, MaxParallel: fo.MaxParallel}
		if graph != nil {
			group.Dependents = graph.Dependents
		}

		template, err := loadUniqueTaskRun(tx, runID, succID)
		if err != nil {
			return nil, fmt.Errorf("run: fan-out template for %s: %w", succTask.Name, err)
		}

		// Expansion may only run from the successor's UNEXPANDED, UNRESOLVED
		// state. Without this the expansion loaded whatever row was there and
		// rewrote it into N pending instances — so a successor the DAG had
		// ALREADY resolved (skipped by a branch selection, skipped by an
		// unsatisfied trigger rule, cancelled with the run) was resurrected:
		// its own row kept the terminal status while N-1 brand-new PENDING
		// sibling rows appeared beside it and got dispatched. A step the DAG
		// decided not to run must stay not-run, and so must its descendants.
		if !fanOutTemplateExpandable(template) {
			continue
		}

		if len(partitions) == 0 {
			onEmpty := fo.OnEmpty
			if onEmpty == "" {
				onEmpty = jobdefschema.FanOutOnEmptySkip
			}
			if onEmpty == jobdefschema.FanOutOnEmptyFail {
				return nil, &pkgtask.PartitionError{Msg: fmt.Sprintf("fan-out produced no partitions for step %q", succTask.Name)}
			}
			if persist {
				if _, err := s.skipTaskAndDescendantsTx(tx, runID, succID, "fan-out produced no partitions", pendingEvents, counts); err != nil {
					return nil, err
				}
			}
			group.Skipped = true
			expansion.Groups = append(expansion.Groups, group)
			continue
		}

		n := len(partitions)
		if persist {
			// The metric's labels are {job alias, task name} — producerName is a
			// TASK name and was landing in the job-alias slot, so every series was
			// mislabelled. Resolve the run's job alias once per expanded group.
			metrics.FanOutPartitionsTotal.WithLabelValues(s.jobAliasForRunTx(tx, runID), succTask.Name).Add(float64(n))
		}

		instances := make([]models.TaskRun, 0, n)
		for i, p := range partitions {
			attrs, err := encodePartitionMap(p.Attributes)
			if err != nil {
				return nil, err
			}
			deps, err := json.Marshal(p.DependsOn)
			if err != nil {
				return nil, err
			}
			indegree := 0
			if graph != nil {
				indegree = graph.Indegree[p.Key]
			}
			outstanding := template.OutstandingPredecessors + indegree
			row := *template
			if i == 0 {
				row.ID = template.ID
			} else {
				row.ID = uuid.New()
				row.CreatedAt = template.CreatedAt
			}
			row.PartitionValue = p.Key
			row.PartitionIndex = i
			row.PartitionCount = n
			row.PartitionFingerprint = p.Fingerprint
			row.PartitionAttributes = attrs
			row.PartitionDependsOn = datatypes.JSON(deps)
			row.OutstandingPredecessors = outstanding
			row.Status = string(TaskStatusPending)
			row.ClaimedBy = ""
			row.RuntimeID = ""
			row.StartedAt = nil
			row.CompletedAt = nil
			instances = append(instances, row)
			group.Instances = append(group.Instances, ExpandedInstance{
				TaskRunID:               row.ID,
				TaskID:                  succID,
				PartitionIndex:          i,
				Partition:               p,
				OutstandingPredecessors: outstanding,
			})
		}

		if persist {
			if err := s.persistExpandedGroupTx(tx, template.ID, instances, counts); err != nil {
				return nil, err
			}
		}
		expansion.Groups = append(expansion.Groups, group)
	}

	return expansion, nil
}

// fanOutTemplateExpandable reports whether a successor's TaskRun is still the
// pre-expansion template: pending (nothing has resolved it) and unfanned
// (partition_count 0, so it has not already been expanded by an earlier
// completion or a replayed one). Anything else — a terminal row, or a row that
// is already one of N — must be left exactly as it is.
func fanOutTemplateExpandable(template *models.TaskRun) bool {
	if template == nil {
		return false
	}
	return template.Status == string(TaskStatusPending) && template.PartitionCount == 0
}

func (s *Store) persistExpansionTx(tx *gorm.DB, runID uuid.UUID, expansion *FanOutExpansion, pendingEvents *[]event.Event, counts *dbWriteCounts) error {
	if expansion == nil {
		return nil
	}
	if producer, err := loadUniqueTaskRun(tx, runID, expansion.ProducerTaskID); err == nil && producer != nil {
		if err := s.persistProducerPartitionsTx(tx, producer.ID, expansion.Partitions); err != nil {
			return err
		}
	}
	for _, g := range expansion.Groups {
		if g.Skipped || len(g.Instances) == 0 {
			continue
		}
		template, err := loadUniqueTaskRun(tx, runID, g.TaskID)
		if err != nil {
			return err
		}
		rows := make([]models.TaskRun, 0, len(g.Instances))
		for _, inst := range g.Instances {
			attrs, err := encodePartitionMap(inst.Partition.Attributes)
			if err != nil {
				return err
			}
			deps, err := json.Marshal(inst.Partition.DependsOn)
			if err != nil {
				return err
			}
			row := *template
			row.ID = inst.TaskRunID
			row.PartitionValue = inst.Partition.Key
			row.PartitionIndex = inst.PartitionIndex
			row.PartitionCount = len(g.Instances)
			row.PartitionFingerprint = inst.Partition.Fingerprint
			row.PartitionAttributes = attrs
			row.PartitionDependsOn = datatypes.JSON(deps)
			row.OutstandingPredecessors = inst.OutstandingPredecessors
			row.Status = string(TaskStatusPending)
			row.ClaimedBy = ""
			row.RuntimeID = ""
			row.StartedAt = nil
			row.CompletedAt = nil
			rows = append(rows, row)
		}
		if err := s.persistExpandedGroupTx(tx, template.ID, rows, counts); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) persistExpandedGroupTx(tx *gorm.DB, templateID uuid.UUID, instances []models.TaskRun, counts *dbWriteCounts) error {
	if len(instances) == 0 {
		return nil
	}
	if err := tx.Model(&models.TaskRun{}).Where("id = ?", templateID).Updates(map[string]any{
		"partition_value":          instances[0].PartitionValue,
		"partition_index":          instances[0].PartitionIndex,
		"partition_count":          instances[0].PartitionCount,
		"partition_fingerprint":    instances[0].PartitionFingerprint,
		"partition_attributes":     instances[0].PartitionAttributes,
		"partition_depends_on":     instances[0].PartitionDependsOn,
		"outstanding_predecessors": instances[0].OutstandingPredecessors,
	}).Error; err != nil {
		return err
	}
	if len(instances) == 1 {
		return nil
	}
	rest := instances[1:]
	existing := make(map[uuid.UUID]struct{}, len(rest))
	ids := make([]uuid.UUID, 0, len(rest))
	for i := range rest {
		ids = append(ids, rest[i].ID)
	}
	var found []models.TaskRun
	if err := tx.Select("id").Where("id IN ?", ids).Find(&found).Error; err != nil {
		return err
	}
	for i := range found {
		existing[found[i].ID] = struct{}{}
	}
	toCreate := make([]models.TaskRun, 0, len(rest))
	for i := range rest {
		if _, ok := existing[rest[i].ID]; ok {
			continue
		}
		toCreate = append(toCreate, rest[i])
	}
	if len(toCreate) == 0 {
		return nil
	}
	if err := tx.Create(&toCreate).Error; err != nil {
		return err
	}
	if counts != nil {
		counts.addTaskRunInsert(len(toCreate))
	}
	return nil
}

// fanOutMaxParallelPredicateTx returns an extra WHERE fragment (and its bound
// args) that caps a fanned group's in-flight instances at fanOut.maxParallel.
// Empty string when the row is not a fan-out instance or the step declares no
// cap.
//
// This is a PREDICATE, not a pre-check, deliberately. The previous form ran a
// separate `COUNT(*) … status='running'` and returned early — correct only if
// nothing can start a sibling between the count and the claim UPDATE. Under
// dqlite every write serializes through Raft, so two concurrent owner dispatches
// cannot interleave *within* one transaction's writes; but the count ran on the
// transaction's read snapshot, and a sibling claimed by the worker-pull path
// (internal/worker/claimer.go, which caps the same group with its own subquery
// inside its single atomic UPDATE) could commit between the two statements in
// the same tx. Folding the count into the guarded UPDATE removes the window
// entirely and matches the claimer's shape: the cap is evaluated by the same
// statement that takes the claim, so an over-claim can only manifest as
// RowsAffected == 0 → ErrTaskClaimMismatch, which the caller already handles by
// leaving the instance pending for the next tick.
//
// Deadlock is impossible for an ordered chain deeper than maxParallel: readiness
// comes from outstanding_predecessors reaching zero, which is driven by TERMINAL
// siblings, never by a free slot. a→b→c with maxParallel=1 runs a, and only once
// a is terminal does b become ready — at which point the running count is 0.
func (s *Store) fanOutMaxParallelPredicateTx(tx *gorm.DB, row *models.TaskRun) (string, []interface{}, error) {
	if row == nil || !isFanOutInstance(row) {
		return "", nil, nil
	}
	var task models.Task
	if err := tx.First(&task, "id = ?", row.TaskID).Error; err != nil {
		return "", nil, err
	}
	fo, err := decodeFanOutConfig(task.FanOutConfig)
	if err != nil || fo == nil || fo.MaxParallel <= 0 {
		return "", nil, err
	}
	return " AND (SELECT COUNT(*) FROM task_runs sib WHERE sib.job_run_id = ? AND sib.task_id = ? AND sib.status = ?) < ?",
		[]interface{}{row.JobRunID, row.TaskID, string(TaskStatusRunning), fo.MaxParallel},
		nil
}

// IsFanOutInstance reports whether a TaskRun row is one instance of a fanned
// group. Exported so internal/worker can ask the question from the SAME
// definition the SQL lane uses rather than re-deriving "does this row have a
// partition" from the column set — the two drifting is how a fanned instance
// ends up treated as an ordinary task on one path only.
func IsFanOutInstance(tr *models.TaskRun) bool {
	return isFanOutInstance(tr)
}

func isFanOutInstance(tr *models.TaskRun) bool {
	if tr == nil {
		return false
	}
	return tr.PartitionCount > 1 || (tr.PartitionCount > 0 && tr.PartitionValue != "")
}

func (s *Store) advanceCrossStepSuccessorsTx(
	tx *gorm.DB,
	runID, taskID uuid.UUID,
	pendingEvents *[]event.Event,
	skippedIDs *[]uuid.UUID,
	counts *dbWriteCounts,
) error {
	var taskModel models.Task
	if err := tx.First(&taskModel, "id = ?", taskID).Error; err != nil {
		return err
	}
	edges, err := s.successorEdgesForRunTx(tx, runID, taskID, taskModel)
	if err != nil {
		return err
	}
	toDecrement := make([]uuid.UUID, 0, len(edges))
	for _, edge := range edges {
		toDecrement = append(toDecrement, edge.ToTaskID)
	}
	updated, err := s.batchDecrementPredecessorsTx(tx, runID, toDecrement)
	if err != nil {
		return err
	}
	if counts != nil {
		counts.addTaskRunStatus(len(toDecrement))
	}
	for i := range updated {
		successor := &updated[i]
		if successor.OutstandingPredecessors != 0 || successor.Status != string(TaskStatusPending) {
			continue
		}
		shouldRun, rule, err := s.shouldRunTaskTx(tx, runID, successor.TaskID)
		if err != nil {
			return err
		}
		if shouldRun {
			if err := s.appendTaskReadyEventTx(tx, runID, successor.TaskID, pendingEvents, counts); err != nil {
				return err
			}
			continue
		}
		skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", rule)
		skipped, err := s.skipTaskAndDescendantsTx(tx, runID, successor.TaskID, skipRuleReason, pendingEvents, counts)
		if err != nil {
			return err
		}
		if skippedIDs != nil {
			*skippedIDs = append(*skippedIDs, skipped...)
		}
	}
	return nil
}

// jobAliasForRunTx resolves a run's job alias for metric labelling. Best effort:
// an unresolvable alias yields "" rather than failing the completion
// transaction, because a mislabelled metric must never fail a run.
func (s *Store) jobAliasForRunTx(tx *gorm.DB, runID uuid.UUID) string {
	var row struct {
		Alias string
	}
	if err := tx.Table("job_runs").
		Select("jobs.alias AS alias").
		Joins("join jobs on jobs.id = job_runs.job_id").
		Where("job_runs.id = ?", runID).
		Take(&row).Error; err != nil {
		return ""
	}
	return row.Alias
}

// groupAllTerminalTx reports whether every instance of a fan-out group is
// terminal, and — on the transition that makes that true — observes the group's
// wall-clock duration (min StartedAt → max CompletedAt) on
// FanOutGroupDurationSeconds. The owner in-memory lane observes the same
// histogram from owner_manager.go; this is the SQL lane's half, without which
// the metric only ever has data under CAESIUM_RUN_OWNER_IN_MEMORY.
func (s *Store) groupAllTerminalTx(tx *gorm.DB, runID, taskID uuid.UUID) (bool, error) {
	if err := lockGroupForTerminalDecisionTx(tx, runID, taskID); err != nil {
		return false, err
	}
	var rows []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).Find(&rows).Error; err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return true, nil
	}
	for i := range rows {
		if !IsTerminal(TaskStatus(rows[i].Status)) {
			return false, nil
		}
	}
	s.observeFanOutGroupDurationTx(tx, runID, taskID, rows)
	return true, nil
}

// groupAllSucceededTx reports whether EVERY instance of a fan-out group is a
// terminal SUCCESS (succeeded or cached).
//
// Distinct from groupAllTerminalTx, and the distinction is the whole point:
// "terminal" answers "may the DAG move on?" (yes — a failed group's successors
// are resolved by their trigger rules), while "succeeded" answers "did this step
// work?". Conflating them let a group with failed partitions announce
// task_succeeded. A group with no rows is not a success.
func (s *Store) groupAllSucceededTx(tx *gorm.DB, runID, taskID uuid.UUID) (bool, error) {
	var rows []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ?", runID, taskID).Find(&rows).Error; err != nil {
		return false, err
	}
	return predecessorGroupSatisfied(rows), nil
}

// lockGroupForTerminalDecisionTx serializes the "is this whole group terminal?"
// decision across concurrently completing siblings.
//
// PostgreSQL is a first-class catalog backend (pkg/db/db.go opens
// gorm.io/driver/postgres for CAESIUM_DATABASE_TYPE=postgres), and there two
// sibling completion transactions genuinely run at the same time under READ
// COMMITTED. Each writes its OWN row terminal and then reads the group; neither
// sees the other's uncommitted write, so both conclude "a sibling is still
// running", both return false, and the group's cross-step successors are never
// released. Every instance is terminal and the run hangs until its timeout —
// and it only reproduces under real write concurrency, which is why no test
// caught it.
//
// Taking an exclusive lock on ONE deterministic row of the group (the lowest
// partition index, tie-broken by id) fixes both halves: the transactions
// serialize, and the loser's post-lock read under READ COMMITTED sees the
// winner's COMMITTED row. Exactly one transaction therefore observes the
// complete terminal set. The row is chosen deterministically so two groups can
// never lock each other's rows in opposite orders.
//
// dqlite/SQLite need nothing: every writer already serializes (through the Raft
// log and the single write connection respectively), and `FOR UPDATE` is not
// parseable there — hence the empty statement rather than a shared one.
func lockGroupForTerminalDecisionTx(tx *gorm.DB, runID, taskID uuid.UUID) error {
	if tx == nil || tx.Dialector == nil {
		return nil
	}
	stmt, err := groupTerminalLockSQL(tx.Name())
	if err != nil {
		return err
	}
	if stmt == "" {
		return nil
	}
	var locked []struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	return tx.Raw(stmt, runID, taskID).Scan(&locked).Error
}

// groupTerminalLockSQL returns the dialect's lock statement for the terminal
// group decision, or "" for a dialect whose writers already serialize. An
// unknown dialect is an error rather than a silent "" so a future backend
// surfaces the missing guard at run time instead of shipping the hang.
func groupTerminalLockSQL(dialect string) (string, error) {
	switch dialect {
	case "postgres":
		return "SELECT id FROM task_runs WHERE job_run_id = ? AND task_id = ? " +
			"ORDER BY partition_index ASC, id ASC LIMIT 1 FOR UPDATE", nil
	case "dqlite", "sqlite", "sqlite3":
		return "", nil
	default:
		return "", fmt.Errorf("run: unsupported dialect %q for fan-out group terminal lock", dialect)
	}
}

// observeFanOutGroupDurationTx records the fanned group's span. Unfanned tasks
// (a single instance with no partition value) are not groups and are skipped, so
// the series stays fan-out-only.
func (s *Store) observeFanOutGroupDurationTx(tx *gorm.DB, runID, taskID uuid.UUID, rows []models.TaskRun) {
	fanned := false
	for i := range rows {
		if isFanOutInstance(&rows[i]) {
			fanned = true
			break
		}
	}
	if !fanned {
		return
	}
	var firstStart, lastEnd *time.Time
	for i := range rows {
		if rows[i].StartedAt != nil && (firstStart == nil || rows[i].StartedAt.Before(*firstStart)) {
			firstStart = rows[i].StartedAt
		}
		if rows[i].CompletedAt != nil && (lastEnd == nil || rows[i].CompletedAt.After(*lastEnd)) {
			lastEnd = rows[i].CompletedAt
		}
	}
	if firstStart == nil || lastEnd == nil || lastEnd.Before(*firstStart) {
		return
	}
	var task models.Task
	if err := tx.Select("name").First(&task, "id = ?", taskID).Error; err != nil {
		return
	}
	metrics.FanOutGroupDurationSeconds.
		WithLabelValues(s.jobAliasForRunTx(tx, runID), task.Name).
		Observe(lastEnd.Sub(*firstStart).Seconds())
}

func decodeFanOutConfig(raw []byte) (*jobdefschema.FanOut, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var fo jobdefschema.FanOut
	if err := json.Unmarshal(raw, &fo); err != nil {
		return nil, fmt.Errorf("decode fanOut config: %w", err)
	}
	return &fo, nil
}

func encodePartitionMap(attrs map[string]string) (datatypes.JSON, error) {
	if len(attrs) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(b), nil
}

func asFanOutProducerError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*pkgtask.PartitionError); ok {
		return err
	}
	return &pkgtask.PartitionError{Msg: err.Error()}
}

func (s *Store) decrementInGroupDependentsTx(tx *gorm.DB, runID uuid.UUID, completed *models.TaskRun) error {
	if completed == nil || completed.PartitionCount == 0 || completed.PartitionValue == "" {
		return nil
	}
	var siblings []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ? AND id <> ?", runID, completed.TaskID, completed.ID).Find(&siblings).Error; err != nil {
		return err
	}
	var ids []uuid.UUID
	for i := range siblings {
		var deps []string
		if len(siblings[i].PartitionDependsOn) > 0 {
			_ = json.Unmarshal(siblings[i].PartitionDependsOn, &deps)
		}
		for _, d := range deps {
			if d == completed.PartitionValue {
				ids = append(ids, siblings[i].ID)
				break
			}
		}
	}
	_, err := s.batchDecrementSiblingPredecessorsTx(tx, runID, ids)
	return err
}

// resolveInstanceFailureTx is THE SQL lane's terminal-failure resolution: one
// instance has just been written terminal-failed, and everything that follows
// from that — the group's failurePolicy, the task_failed event carrying this
// instance's identity, and the group-terminal gate that releases the fanned
// step's cross-step successors — happens here, once.
//
// It exists because the SQL lane reaches a failed instance by TWO routes and
// they must not drift:
//
//	failTask        — the task never produced a result (image pull failed,
//	                  attempts exhausted, an infrastructure error).
//	completeTask    — the container RAN and reported a non-zero exit. The
//	                  distributed worker calls sink.Succeeded with result
//	                  "failure" for this (internal/worker/runtime_executor.go),
//	                  so it lands on CompleteTaskClaimed, not FailTaskClaimed;
//	                  the later sink.Failed finds the row already terminal and
//	                  returns early.
//
// The second is the COMMON case — an exit-1 partition — and it had its own,
// smaller copy of the consequences (the dependency cascade only). fail_fast was
// therefore route-dependent: it held under the local executor's in-memory Kahn
// loop and under failTask, and silently degraded to `continue` for exactly the
// scenario it exists for, a partition that exits non-zero under
// CAESIUM_EXECUTION_MODE=distributed. That is the mode-dependent-defect shape
// the plan's route-completeness contract exists to prevent, so there is one
// implementation and both routes call it.
//
// catalogTaskID is the catalog task id (group-level queries and metric labels);
// row is the instance that failed, already updated to its terminal status. row
// may be nil on completeTask's unclaimed path when the TaskRun row could not be
// loaded — then only the event is recorded, keyed by catalogTaskID.
func (s *Store) resolveInstanceFailureTx(
	tx *gorm.DB,
	runID, catalogTaskID uuid.UUID,
	row *models.TaskRun,
	pendingEvents *[]event.Event,
	skippedTaskIDs *[]uuid.UUID,
	counts *dbWriteCounts,
) error {
	if row != nil {
		if err := s.resolveGroupOnInstanceFailureTx(tx, runID, row, pendingEvents, counts); err != nil {
			return err
		}
	}

	if s.eventStore != nil {
		var (
			evt *event.Event
			err error
		)
		if row != nil {
			evt, err = s.recordTaskRunEventTx(tx, event.TypeTaskFailed, runID, row, counts)
		} else {
			evt, err = s.recordTaskEventTx(tx, event.TypeTaskFailed, runID, catalogTaskID, counts)
		}
		if err != nil {
			return err
		}
		if pendingEvents != nil {
			*pendingEvents = append(*pendingEvents, *evt)
		}
	}

	// Only a fanned group has a group to resolve. An unfanned task's successors
	// are advanced by the ordinary trigger-rule path, not from here.
	if row == nil || !isFanOutInstance(row) {
		return nil
	}
	allTerminal, err := s.groupAllTerminalTx(tx, runID, catalogTaskID)
	if err != nil || !allTerminal {
		return err
	}
	return s.advanceCrossStepSuccessorsTx(tx, runID, catalogTaskID, pendingEvents, skippedTaskIDs, counts)
}

// resolveGroupOnInstanceFailureTx applies the group's fanOut.failurePolicy when
// one instance fails. It is the SQL lane's half of the policy
// RunState.ApplyCompletion implements in memory (internal/run/owner_state.go),
// and the two must agree: a policy that holds under
// CAESIUM_RUN_OWNER_IN_MEMORY=true and degrades to `continue` in the default SQL
// configuration is exactly the mode-dependent stall the plan's route-completeness
// contract exists to prevent — it passes CI in the configuration nobody varied.
//
//	fail_fast (the DEFAULT — pkg/jobdef's validateSteps normalizes "" to it):
//	  the first instance failure stops the group taking on any MORE work. Every
//	  sibling that has NOT YET STARTED is resolved skipped with its own
//	  terminal_sequence and its own task_skipped event, so none is ever
//	  dispatched and a takeover does not re-dispatch it. Not-yet-started rather
//	  than merely pending: both claim paths flip a row to `running` before any
//	  container exists, so a claimed sibling waiting for a worker slot would
//	  otherwise start AFTER the group had already failed (see
//	  failFastSkipSiblingsTx and taskRunStarted). This is a strict SUPERSET of
//	  the dependency cascade (a dependent of the failure had by definition not
//	  started), so the two branches are mutually exclusive.
//	continue:
//	  only the failed instance's TRANSITIVE in-group dependents are skipped
//	  (skipInGroupDependentsTx); independent siblings run to completion and the
//	  group resolves when the last one lands.
//
// Either way every affected instance ends terminal, which is what lets
// groupAllTerminalTx fire and the group's cross-step successors be handled by
// the normal trigger-rule path.
func (s *Store) resolveGroupOnInstanceFailureTx(tx *gorm.DB, runID uuid.UUID, failed *models.TaskRun, pendingEvents *[]event.Event, counts *dbWriteCounts) error {
	if failed == nil || !isFanOutInstance(failed) {
		return nil
	}
	failFast, err := s.groupFailsFastTx(tx, failed.TaskID)
	if err != nil {
		return err
	}
	if !failFast {
		return s.skipInGroupDependentsTx(tx, runID, failed, pendingEvents, counts)
	}
	return s.failFastSkipSiblingsTx(tx, runID, failed, pendingEvents, counts)
}

// groupFailsFastTx reads a fanned step's effective failure policy from the
// catalog. An absent fanOut block, an unreadable one, or an empty
// failurePolicy all resolve to fail_fast, matching normalizeFanOutFailurePolicy
// in the owner engine and validateSteps in pkg/jobdef. Treating "" as
// `continue` would silently ship the wrong default to every job that never
// wrote the field.
func (s *Store) groupFailsFastTx(tx *gorm.DB, taskID uuid.UUID) (bool, error) {
	var task models.Task
	if err := tx.Select("id", "fan_out_config").First(&task, "id = ?", taskID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, nil
		}
		return false, err
	}
	fo, err := decodeFanOutConfig(task.FanOutConfig)
	if err != nil {
		return false, err
	}
	if fo == nil {
		return true, nil
	}
	return fo.FailurePolicy != jobdefschema.FanOutFailureContinue, nil
}

// taskRunStarted reports whether a TaskRun has actually begun executing — a
// container/pod exists for it.
//
// runtime_id is the marker, deliberately, and NOT started_at:
//
//   - runtime_id is written by StartTask / StartTaskClaimed with the engine's
//     atom id, which only exists once the runtime accepted the container. The
//     claim paths never write it, and retryTask clears it, so it is empty for
//     exactly the window between a claim and a start.
//   - started_at looks like the natural marker and is not: ClaimTaskForDispatch
//     stamps it at CLAIM time (internal/run/store.go), so on the owner-push lane
//     a task queued in a worker pool with no container is indistinguishable from
//     one halfway through its run. The pull lane's claim leaves it null, so the
//     two lanes would not even agree.
//
// A row is therefore cancellable-before-start when it is pending, or running
// with no runtime_id. Both claim paths flip a row to running BEFORE any
// container exists — ClaimNext's single-statement UPDATE and
// ClaimTaskForDispatch alike — so `pending` alone is NOT the set of rows that
// have yet to start, on either lane.
func taskRunStarted(row *models.TaskRun) bool {
	return row != nil && strings.TrimSpace(row.RuntimeID) != ""
}

// cancellableBeforeStartPredicate is the SQL half of taskRunStarted: rows that
// can still be resolved terminal without stranding a live container. It is a
// predicate rather than a status list because the check must happen INSIDE the
// guarded UPDATE — a worker that starts the container between the read and the
// write must make the cancel fail, not lose the race silently.
func cancellableBeforeStartPredicate() (string, []interface{}) {
	return "(status = ? OR (status = ? AND COALESCE(runtime_id, '') = ''))",
		[]interface{}{string(TaskStatusPending), string(TaskStatusRunning)}
}

// failFastSkipSiblingsTx resolves every not-yet-started sibling of a failed instance,
// each through the shared terminal-skip primitive so it gets its own
// terminal_sequence and its own task_skipped event carrying its own partition.
// The reason string is the exact one the owner lane emits, because consumers
// (and the integration lane) match on it.
//
// NOT-YET-STARTED, not merely pending, is the contract. The design says
// fail_fast "cancels pending siblings" (docs/design-dynamic-fanout.md:324,
// :691, :1111), but `status = pending` is not that set: both claim paths flip a
// row to `running` BEFORE any container exists — ClaimNext's single-statement
// UPDATE, and ClaimTaskForDispatch — and the worker pool may hold the claimed
// task for as long as it takes a slot to free up. A pending-only skip therefore
// left a claimed-but-unstarted sibling to start seconds AFTER its group had
// already failed, which is precisely what fail_fast exists to prevent. It
// surfaced as a scheduling-dependent CI failure (one worker slot, the sibling
// queued behind the failing instance), not a deterministic one. taskRunStarted
// documents why runtime_id, not started_at, decides.
//
// A genuinely RUNNING sibling is left alone, unchanged: Caesium cannot kill a
// running container, so marking one skipped would claim a terminal state for
// live work and invite its worker's later completion to contradict the row —
// the same shape as the local-mode replace-cancel resurrection bug. The run
// loses nothing by waiting: an in-flight instance reaches a terminal row on its
// own, at which point groupAllTerminalTx fires and the cross-step successors are
// resolved against a group that already contains a failure (the failed sibling
// alone fixes group status, so the fan-in is skipped by its trigger rule either
// way).
//
// The owner in-memory lane cancels the same set for the same reasons:
// RunState.ApplyCompletion (internal/run/owner_state.go) skips siblings that are
// pending or dispatched-but-not-started.
func (s *Store) failFastSkipSiblingsTx(tx *gorm.DB, runID uuid.UUID, failed *models.TaskRun, pendingEvents *[]event.Event, counts *dbWriteCounts) error {
	predSQL, predArgs := cancellableBeforeStartPredicate()
	args := append([]interface{}{runID, failed.TaskID, failed.ID}, predArgs...)
	var siblings []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ? AND id <> ? AND "+predSQL, args...).
		Order("partition_index ASC").
		Find(&siblings).Error; err != nil {
		return err
	}
	for i := range siblings {
		if _, err := s.markInstanceCancelledBeforeStartTx(tx, runID, &siblings[i],
			"fan-out group failed fast", pendingEvents, counts); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) skipInGroupDependentsTx(tx *gorm.DB, runID uuid.UUID, failed *models.TaskRun, pendingEvents *[]event.Event, counts *dbWriteCounts) error {
	if failed == nil || failed.PartitionCount == 0 || failed.PartitionValue == "" {
		return nil
	}
	var siblings []models.TaskRun
	if err := tx.Where("job_run_id = ? AND task_id = ?", runID, failed.TaskID).Find(&siblings).Error; err != nil {
		return err
	}
	dependents := map[string][]string{}
	byKey := map[string]*models.TaskRun{}
	for i := range siblings {
		byKey[siblings[i].PartitionValue] = &siblings[i]
		var deps []string
		if len(siblings[i].PartitionDependsOn) > 0 {
			_ = json.Unmarshal(siblings[i].PartitionDependsOn, &deps)
		}
		for _, d := range deps {
			dependents[d] = append(dependents[d], siblings[i].PartitionValue)
		}
	}
	queue := []string{failed.PartitionValue}
	seen := map[string]struct{}{failed.PartitionValue: {}}
	reason := fmt.Sprintf("fan-out dependency %s failed", failed.PartitionValue)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range dependents[cur] {
			if _, ok := seen[dep]; ok {
				continue
			}
			seen[dep] = struct{}{}
			queue = append(queue, dep)
			row := byKey[dep]
			if row == nil || IsTerminal(TaskStatus(row.Status)) {
				continue
			}
			// Route through the shared terminal-skip primitive rather than a raw
			// UPDATE. The raw form left terminal_sequence at 0 and emitted no
			// task_skipped event, so a cascade-skipped instance was invisible both
			// to the event stream and to TerminalTaskRunsSince replay — a
			// recovering owner would believe the instance was still pending.
			if _, err := s.markInstanceSkippedTx(tx, runID, row, reason, pendingEvents, counts); err != nil {
				return err
			}
		}
	}
	return nil
}

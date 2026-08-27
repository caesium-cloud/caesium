package run

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// topologyNode is one resolved DAG node: its task ID, its trigger rule, and its
// direct successors. Both topology sources — the live catalog and the frozen
// replay descriptors — produce this, and buildRunTopology turns it into a
// RunTopology. It is the G7 de-duplication for topology: LoadRunTopology and
// loadReplayRunTopology used to each assemble Adjacency/Predecessors/TriggerRule/
// Order by hand, so a change to the topology shape had to be made twice and the
// second copy is the one that gets forgotten.
type topologyNode struct {
	taskID      uuid.UUID
	triggerRule string
	successors  []uuid.UUID
}

// buildRunTopology is the ONE place a RunTopology is assembled. nodes must
// already be in dispatch order; an edge to a task not present in nodes is
// dropped (a successor outside this run's node set cannot be advanced).
func buildRunTopology(nodes []topologyNode) RunTopology {
	topo := RunTopology{
		Adjacency:    make(map[uuid.UUID][]uuid.UUID, len(nodes)),
		Predecessors: make(map[uuid.UUID][]uuid.UUID, len(nodes)),
		TriggerRule:  make(map[uuid.UUID]string, len(nodes)),
		Order:        make(map[uuid.UUID]int, len(nodes)),
	}
	for i, node := range nodes {
		topo.Adjacency[node.taskID] = nil
		topo.Predecessors[node.taskID] = nil
		topo.Order[node.taskID] = i
		topo.TriggerRule[node.taskID] = node.triggerRule
	}
	for _, node := range nodes {
		for _, to := range node.successors {
			if _, ok := topo.Order[to]; !ok {
				continue
			}
			topo.Adjacency[node.taskID] = append(topo.Adjacency[node.taskID], to)
			topo.Predecessors[to] = append(topo.Predecessors[to], node.taskID)
		}
	}
	return topo
}

// LoadRunTopology reads a run's immutable DAG shape and builds the RunTopology
// the owner's in-memory RunState needs. Normal runs use the live job catalog;
// quarantined replay runs use the per-task execution descriptors captured on
// the TaskRun rows so a later apply cannot change replay dispatch order.
//
// The two sources differ ONLY in how they enumerate nodes and edges; both feed
// the same buildRunTopology.
func (s *Store) LoadRunTopology(runID uuid.UUID) (RunTopology, error) {
	var jobRun models.JobRun
	if err := s.db.Select("job_id", "quarantine").First(&jobRun, "id = ?", runID).Error; err != nil {
		return RunTopology{}, fmt.Errorf("run topology: job run %s: %w", runID, err)
	}

	var (
		nodes []topologyNode
		err   error
	)
	if jobRun.Quarantine {
		nodes, err = s.replayTopologyNodes(runID)
	} else {
		nodes, err = s.catalogTopologyNodes(jobRun.JobID)
	}
	if err != nil {
		return RunTopology{}, err
	}
	return buildRunTopology(nodes), nil
}

// catalogTopologyNodes enumerates the live job catalog in dispatch order.
func (s *Store) catalogTopologyNodes(jobID uuid.UUID) ([]topologyNode, error) {
	var tasks []models.Task
	if err := s.db.
		Where("job_id = ?", jobID).
		Order("position asc").
		Order("created_at asc").
		Find(&tasks).Error; err != nil {
		return nil, err
	}

	nodes := make([]topologyNode, 0, len(tasks))
	for i := range tasks {
		t := &tasks[i]
		rule := t.TriggerRule
		if rule == "" {
			rule = jobdefschema.TriggerRuleAllSuccess
		}
		edges, err := s.successorEdgesTx(s.db, *t)
		if err != nil {
			return nil, err
		}
		successors := make([]uuid.UUID, 0, len(edges))
		for _, e := range edges {
			successors = append(successors, e.ToTaskID)
		}
		nodes = append(nodes, topologyNode{taskID: t.ID, triggerRule: rule, successors: successors})
	}
	return nodes, nil
}

// replayTopologyNodes enumerates a quarantined run's frozen descriptors, ordered
// by the captured DAG position so replay dispatch order cannot drift with the
// live catalog.
func (s *Store) replayTopologyNodes(runID uuid.UUID) ([]topologyNode, error) {
	var taskRuns []models.TaskRun
	if err := s.db.
		Select("task_id", "execution_descriptor", "created_at").
		Where("job_run_id = ?", runID).
		Order("created_at asc").
		Find(&taskRuns).Error; err != nil {
		return nil, err
	}
	type descriptorTask struct {
		taskID     uuid.UUID
		createdAt  int
		descriptor models.TaskExecutionDescriptor
	}
	tasks := make([]descriptorTask, 0, len(taskRuns))
	for idx := range taskRuns {
		row := &taskRuns[idx]
		if len(row.ExecutionDescriptor) == 0 {
			return nil, fmt.Errorf("run topology: replay task %s missing execution descriptor", row.TaskID)
		}
		var descriptor models.TaskExecutionDescriptor
		if err := json.Unmarshal(row.ExecutionDescriptor, &descriptor); err != nil {
			return nil, fmt.Errorf("run topology: decode replay descriptor for task %s: %w", row.TaskID, err)
		}
		if descriptor.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
			return nil, fmt.Errorf("run topology: unsupported replay descriptor version %d for task %s", descriptor.SchemaVersion, row.TaskID)
		}
		tasks = append(tasks, descriptorTask{taskID: row.TaskID, createdAt: idx, descriptor: descriptor})
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].descriptor.DAG.TaskPosition != tasks[j].descriptor.DAG.TaskPosition {
			return tasks[i].descriptor.DAG.TaskPosition < tasks[j].descriptor.DAG.TaskPosition
		}
		return tasks[i].createdAt < tasks[j].createdAt
	})

	nodes := make([]topologyNode, 0, len(tasks))
	for _, task := range tasks {
		successors := make([]uuid.UUID, 0, len(task.descriptor.DAG.Successors))
		for _, successor := range task.descriptor.DAG.Successors {
			if successor.TaskID == uuid.Nil {
				continue
			}
			successors = append(successors, successor.TaskID)
		}
		nodes = append(nodes, topologyNode{
			taskID:      task.taskID,
			triggerRule: normalizedTriggerRule(task.descriptor.DAG.TriggerRule),
			successors:  successors,
		})
	}
	return nodes, nil
}

// branchTargets is a branch task's successor set as ResolveBranchSkips needs
// it: the successors in edge order (what may be skipped) and the name→ID map a
// runtime branch selection is resolved through (what may be selected).
//
// The two are deliberately NOT the same set. On the live path a successor Task
// with an empty name has no entry in byName, so it can never be selected and is
// therefore always skipped; the replay path falls back to the task ID string as
// a name, so every frozen successor is selectable. That asymmetry predates G7
// and is preserved here rather than smoothed over — changing it would change
// which steps run, which is not a refactor.
type branchTargets struct {
	ordered []uuid.UUID
	byName  map[string]uuid.UUID
	names   []string
}

// ResolveBranchSkips returns the immediate successor task IDs a branch task
// excluded at runtime via its branch selections — i.e. the successors to skip.
// It returns nil for non-branch tasks (which skip nothing here).  It errors if a
// selection names a step that is not a valid successor, matching completeTask's
// validation.
//
// G7: live and replay differ ONLY in how they enumerate a branch's successors,
// so that is the one thing that forks (branchTargetsTx) and the selection
// algorithm below exists once. Previously both halves carried their own copy of
// "build the name map, validate each selection, invert the selection into
// skips", which is the shape where a fix lands on one path and quietly does not
// on the other.
func (s *Store) ResolveBranchSkips(runID, taskID uuid.UUID, branchSelections []string) ([]uuid.UUID, error) {
	targets, isBranch, err := s.branchTargetsTx(runID, taskID)
	if err != nil {
		return nil, err
	}
	if !isBranch || targets == nil || len(targets.ordered) == 0 {
		return nil, nil
	}

	selected := make(map[uuid.UUID]bool, len(branchSelections))
	for _, name := range branchSelections {
		id, ok := targets.byName[name]
		if !ok {
			return nil, fmt.Errorf("branch selected unknown step %q; valid targets: %v", name, targets.names)
		}
		selected[id] = true
	}

	skips := make([]uuid.UUID, 0, len(targets.ordered))
	for _, id := range targets.ordered {
		if !selected[id] {
			skips = append(skips, id)
		}
	}
	return skips, nil
}

// branchTargetsTx enumerates a task's successors from whichever source governs
// this run, and reports whether the task is a branch at all. A quarantined
// replay run reads its frozen execution descriptor so a later apply cannot
// change what a replay considers selectable; a normal run reads the live
// catalog.
func (s *Store) branchTargetsTx(runID, taskID uuid.UUID) (*branchTargets, bool, error) {
	descriptor, replay, err := s.replayTaskExecutionDescriptorTx(s.db, runID, taskID)
	if err != nil {
		return nil, false, err
	}
	if replay {
		taskType := firstNonEmpty(descriptor.Runtime.TaskType, descriptor.DAG.BranchBehavior, "task")
		if taskType != "branch" {
			return nil, false, nil
		}
		targets := &branchTargets{byName: make(map[string]uuid.UUID, len(descriptor.DAG.Successors))}
		for _, successor := range descriptor.DAG.Successors {
			if successor.TaskID == uuid.Nil {
				continue
			}
			// The ID string is the fallback name, so a frozen successor whose
			// name was not captured stays selectable.
			name := firstNonEmpty(successor.TaskName, successor.TaskID.String())
			targets.byName[name] = successor.TaskID
			targets.names = append(targets.names, name)
			targets.ordered = append(targets.ordered, successor.TaskID)
		}
		return targets, true, nil
	}

	var task models.Task
	if err := s.db.First(&task, "id = ?", taskID).Error; err != nil {
		return nil, false, err
	}
	if task.Type != "branch" {
		return nil, false, nil
	}
	edges, err := s.successorEdgesTx(s.db, task)
	if err != nil {
		return nil, false, err
	}
	if len(edges) == 0 {
		return nil, true, nil
	}

	targets := &branchTargets{ordered: make([]uuid.UUID, 0, len(edges))}
	for _, e := range edges {
		targets.ordered = append(targets.ordered, e.ToTaskID)
	}
	var successorTasks []models.Task
	if err := s.db.Where("id IN ?", targets.ordered).Find(&successorTasks).Error; err != nil {
		return nil, false, err
	}
	targets.byName = make(map[string]uuid.UUID, len(successorTasks))
	for _, st := range successorTasks {
		// No ID fallback here, deliberately: an unnamed catalog successor is
		// unselectable and therefore always skipped. See branchTargets.
		if st.Name != "" {
			targets.byName[st.Name] = st.ID
			targets.names = append(targets.names, st.Name)
		}
	}
	return targets, true, nil
}

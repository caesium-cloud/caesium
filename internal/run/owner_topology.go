package run

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// LoadRunTopology reads a run's immutable DAG shape and builds the RunTopology
// the owner's in-memory RunState needs. Normal runs use the live job catalog;
// quarantined replay runs use the per-task execution descriptors captured on
// the TaskRun rows so a later apply cannot change replay dispatch order.
func (s *Store) LoadRunTopology(runID uuid.UUID) (RunTopology, error) {
	var jobRun models.JobRun
	if err := s.db.Select("job_id", "quarantine").First(&jobRun, "id = ?", runID).Error; err != nil {
		return RunTopology{}, fmt.Errorf("run topology: job run %s: %w", runID, err)
	}
	if jobRun.Quarantine {
		return s.loadReplayRunTopology(runID)
	}

	var tasks []models.Task
	if err := s.db.
		Where("job_id = ?", jobRun.JobID).
		Order("position asc").
		Order("created_at asc").
		Find(&tasks).Error; err != nil {
		return RunTopology{}, err
	}

	topo := RunTopology{
		Adjacency:    make(map[uuid.UUID][]uuid.UUID, len(tasks)),
		Predecessors: make(map[uuid.UUID][]uuid.UUID, len(tasks)),
		TriggerRule:  make(map[uuid.UUID]string, len(tasks)),
		Order:        make(map[uuid.UUID]int, len(tasks)),
	}
	for i := range tasks {
		t := &tasks[i]
		topo.Adjacency[t.ID] = nil
		topo.Predecessors[t.ID] = nil
		topo.Order[t.ID] = i
		rule := t.TriggerRule
		if rule == "" {
			rule = jobdefschema.TriggerRuleAllSuccess
		}
		topo.TriggerRule[t.ID] = rule
	}

	for i := range tasks {
		t := &tasks[i]
		edges, err := s.successorEdgesTx(s.db, *t)
		if err != nil {
			return RunTopology{}, err
		}
		for _, e := range edges {
			to := e.ToTaskID
			if _, ok := topo.Adjacency[to]; !ok {
				continue
			}
			topo.Adjacency[t.ID] = append(topo.Adjacency[t.ID], to)
			topo.Predecessors[to] = append(topo.Predecessors[to], t.ID)
		}
	}

	return topo, nil
}

func (s *Store) loadReplayRunTopology(runID uuid.UUID) (RunTopology, error) {
	var taskRuns []models.TaskRun
	if err := s.db.
		Select("task_id", "execution_descriptor", "created_at").
		Where("job_run_id = ?", runID).
		Order("created_at asc").
		Find(&taskRuns).Error; err != nil {
		return RunTopology{}, err
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
			return RunTopology{}, fmt.Errorf("run topology: replay task %s missing execution descriptor", row.TaskID)
		}
		var descriptor models.TaskExecutionDescriptor
		if err := json.Unmarshal(row.ExecutionDescriptor, &descriptor); err != nil {
			return RunTopology{}, fmt.Errorf("run topology: decode replay descriptor for task %s: %w", row.TaskID, err)
		}
		if descriptor.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
			return RunTopology{}, fmt.Errorf("run topology: unsupported replay descriptor version %d for task %s", descriptor.SchemaVersion, row.TaskID)
		}
		tasks = append(tasks, descriptorTask{taskID: row.TaskID, createdAt: idx, descriptor: descriptor})
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].descriptor.DAG.TaskPosition != tasks[j].descriptor.DAG.TaskPosition {
			return tasks[i].descriptor.DAG.TaskPosition < tasks[j].descriptor.DAG.TaskPosition
		}
		return tasks[i].createdAt < tasks[j].createdAt
	})

	topo := RunTopology{
		Adjacency:    make(map[uuid.UUID][]uuid.UUID, len(tasks)),
		Predecessors: make(map[uuid.UUID][]uuid.UUID, len(tasks)),
		TriggerRule:  make(map[uuid.UUID]string, len(tasks)),
		Order:        make(map[uuid.UUID]int, len(tasks)),
	}
	for idx, task := range tasks {
		topo.Adjacency[task.taskID] = nil
		topo.Predecessors[task.taskID] = nil
		topo.Order[task.taskID] = idx
		topo.TriggerRule[task.taskID] = normalizedTriggerRule(task.descriptor.DAG.TriggerRule)
	}
	for _, task := range tasks {
		for _, successor := range task.descriptor.DAG.Successors {
			if successor.TaskID == uuid.Nil {
				continue
			}
			if _, ok := topo.Order[successor.TaskID]; !ok {
				continue
			}
			topo.Adjacency[task.taskID] = append(topo.Adjacency[task.taskID], successor.TaskID)
			topo.Predecessors[successor.TaskID] = append(topo.Predecessors[successor.TaskID], task.taskID)
		}
	}
	return topo, nil
}

// ResolveBranchSkips returns the immediate successor task IDs a branch task
// excluded at runtime via its branch selections — i.e. the successors to skip.
// It returns nil for non-branch tasks (which skip nothing here).  It errors if a
// selection names a step that is not a valid successor, matching completeTask's
// validation.
func (s *Store) ResolveBranchSkips(runID, taskID uuid.UUID, branchSelections []string) ([]uuid.UUID, error) {
	if descriptor, replay, err := s.replayTaskExecutionDescriptorTx(s.db, runID, taskID); err != nil {
		return nil, err
	} else if replay {
		taskType := firstNonEmpty(descriptor.Runtime.TaskType, descriptor.DAG.BranchBehavior, "task")
		if taskType != "branch" {
			return nil, nil
		}
		nameToID := make(map[string]uuid.UUID, len(descriptor.DAG.Successors))
		validTargets := make([]string, 0, len(descriptor.DAG.Successors))
		successorIDs := make([]uuid.UUID, 0, len(descriptor.DAG.Successors))
		for _, successor := range descriptor.DAG.Successors {
			if successor.TaskID == uuid.Nil {
				continue
			}
			name := firstNonEmpty(successor.TaskName, successor.TaskID.String())
			nameToID[name] = successor.TaskID
			validTargets = append(validTargets, name)
			successorIDs = append(successorIDs, successor.TaskID)
		}
		selected := make(map[uuid.UUID]bool, len(branchSelections))
		for _, name := range branchSelections {
			id, ok := nameToID[name]
			if !ok {
				return nil, fmt.Errorf("branch selected unknown step %q; valid targets: %v", name, validTargets)
			}
			selected[id] = true
		}
		skips := make([]uuid.UUID, 0, len(successorIDs))
		for _, id := range successorIDs {
			if !selected[id] {
				skips = append(skips, id)
			}
		}
		return skips, nil
	}

	var task models.Task
	if err := s.db.First(&task, "id = ?", taskID).Error; err != nil {
		return nil, err
	}
	if task.Type != "branch" {
		return nil, nil
	}

	edges, err := s.successorEdgesTx(s.db, task)
	if err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, nil
	}

	successorIDs := make([]uuid.UUID, 0, len(edges))
	for _, e := range edges {
		successorIDs = append(successorIDs, e.ToTaskID)
	}
	var successorTasks []models.Task
	if err := s.db.Where("id IN ?", successorIDs).Find(&successorTasks).Error; err != nil {
		return nil, err
	}

	nameToID := make(map[string]uuid.UUID, len(successorTasks))
	validTargets := make([]string, 0, len(successorTasks))
	for _, st := range successorTasks {
		if st.Name != "" {
			nameToID[st.Name] = st.ID
			validTargets = append(validTargets, st.Name)
		}
	}

	selected := make(map[uuid.UUID]bool, len(branchSelections))
	for _, name := range branchSelections {
		id, ok := nameToID[name]
		if !ok {
			return nil, fmt.Errorf("branch selected unknown step %q; valid targets: %v", name, validTargets)
		}
		selected[id] = true
	}

	skips := make([]uuid.UUID, 0, len(successorIDs))
	for _, id := range successorIDs {
		if !selected[id] {
			skips = append(skips, id)
		}
	}
	return skips, nil
}

package run

import (
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// LoadRunTopology reads a run's immutable DAG shape from the catalog — the tasks
// and edges of its job — and builds the RunTopology the owner's in-memory
// RunState needs.  Edge resolution (including the implicit sequential fallback
// when a job declares no explicit edges) reuses successorEdgesTx, so the graph
// matches exactly what the local executor and completeTask see.
func (s *Store) LoadRunTopology(runID uuid.UUID) (RunTopology, error) {
	var jobRun models.JobRun
	if err := s.db.Select("job_id").First(&jobRun, "id = ?", runID).Error; err != nil {
		return RunTopology{}, fmt.Errorf("run topology: job run %s: %w", runID, err)
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

// ResolveBranchSkips returns the immediate successor task IDs a branch task
// excluded at runtime via its branch selections — i.e. the successors to skip.
// It returns nil for non-branch tasks (which skip nothing here).  It errors if a
// selection names a step that is not a valid successor, matching completeTask's
// validation.
func (s *Store) ResolveBranchSkips(taskID uuid.UUID, branchSelections []string) ([]uuid.UUID, error) {
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

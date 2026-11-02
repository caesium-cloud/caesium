package run

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// MaxAssertionInstances bounds how many instances of ONE fanned step a
// RunAssertions read returns. The counts always cover the whole group; the
// instances listed are the most worrying ones first (violated, then those that
// decided nothing), so a 1000-partition group with one breach still shows it.
const MaxAssertionInstances = 100

// AssertionInstance is one TaskRun instance's recorded assertion outcome. It is
// addressed by the instance's own TaskRun id: a fanned step has N verdicts, one
// per partition (plan Open Question 1), and none may stand in for another.
type AssertionInstance struct {
	TaskRunID      uuid.UUID  `json:"task_run_id"`
	PartitionValue string     `json:"partition_value,omitempty"`
	PartitionIndex int        `json:"partition_index,omitempty"`
	Status         TaskStatus `json:"status"`
	// Attempt is the attempt the results belong to. A retry resets the row, so
	// results are only ever those of the latest attempt.
	Attempt  int  `json:"attempt"`
	CacheHit bool `json:"cache_hit"`
	// Verdict is VerdictOf(Results), or VerdictNotEvaluated for a cache hit
	// (no assertion ran for this execution), or VerdictViolated with
	// Violations and no Results for an instance whose violations were recorded
	// before per-check results existed, or VerdictNotRecorded for a sibling of
	// an instance that has evidence but has none of its own (still running,
	// skipped, failed before evaluating, or reset).
	Verdict string            `json:"verdict"`
	Results []AssertionResult `json:"results,omitempty"`
	// Violations are this instance's OWN recorded violations when it predates
	// per-check results (Results empty). A fanned group's collapsed run row
	// carries only one partition's, so this is where the others are read.
	Violations []DataViolation `json:"violations,omitempty"`
}

// TaskAssertions is one step's recorded outcomes across its instances.
type TaskAssertions struct {
	TaskID uuid.UUID `json:"task_id"`
	// Fanned is true for a fan-out group, whose InstanceCount instances each
	// carry their own verdict.
	Fanned        bool                `json:"fanned"`
	InstanceCount int                 `json:"instance_count"`
	VerdictCounts map[string]int      `json:"verdict_counts"`
	Instances     []AssertionInstance `json:"instances"`
	// InstancesTruncated reports that Instances is capped at
	// MaxAssertionInstances; VerdictCounts still covers every instance.
	InstancesTruncated bool `json:"instances_truncated,omitempty"`
}

// RunAssertions is what GET /v1/jobs/:id/runs/:run_id/assertions returns.
type RunAssertions struct {
	RunID uuid.UUID        `json:"run_id"`
	JobID uuid.UUID        `json:"job_id"`
	Tasks []TaskAssertions `json:"tasks"`
}

type assertionRow struct {
	ID               uuid.UUID
	TaskID           uuid.UUID
	Status           string
	Attempt          int
	CacheHit         bool
	PartitionValue   string
	PartitionIndex   int
	PartitionCount   int
	DataViolations   datatypes.JSON
	AssertionResults datatypes.JSON `gorm:"column:data_assertion_results"`
}

// RunAssertions reads what the post-task evaluator recorded for every task
// instance of a run: the outcome of each declared check, as decided at the time
// (never recomputed from today's manifest or baseline). A step appears when an
// instance recorded results or violations, or when it was served from cache
// while declaring assertions — in which case the instance says no assertion
// ran, rather than saying nothing.
//
// A reader confined to some namespaces (authz.Restricted(ctx)) receives the
// results under the rule violations follow: baseline evidence not computed over
// the run's own namespace is withheld (WithholdUnownedBaselineEvidenceFromResults).
// The caller has already authorised the run itself.
//
// It returns gorm.ErrRecordNotFound when the run does not belong to jobID.
func (s *Store) RunAssertions(ctx context.Context, jobID, runID uuid.UUID) (*RunAssertions, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("run: run assertions: nil store")
	}
	conn := s.db.WithContext(ctx)

	var run models.JobRun
	if err := conn.Select("id", "job_id").Where("id = ? AND job_id = ?", runID, jobID).First(&run).Error; err != nil {
		return nil, err
	}

	var rows []assertionRow
	if err := conn.Model(&models.TaskRun{}).
		Select("id", "task_id", "status", "attempt", "cache_hit", "partition_value", "partition_index",
			"data_violations", "data_assertion_results").
		Where("job_run_id = ?", runID).
		Order("partition_index ASC, created_at ASC, id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}

	groups := make(map[uuid.UUID][]assertionRow)
	var order []uuid.UUID
	needsDeclared := false
	for _, row := range rows {
		if _, ok := groups[row.TaskID]; !ok {
			order = append(order, row.TaskID)
		}
		groups[row.TaskID] = append(groups[row.TaskID], row)
		if (row.CacheHit || TaskStatus(row.Status) == TaskStatusCached) && len(row.AssertionResults) == 0 {
			needsDeclared = true
		}
	}

	// Whether a cache-hit step declares assertions is read off the registry,
	// and only when a cache hit makes the answer matter: a step that declares
	// none has nothing to say about not having run them.
	var declares map[uuid.UUID]string
	if needsDeclared {
		var err error
		if declares, err = s.stepsDeclaringAssertions(ctx, jobID, order); err != nil {
			return nil, err
		}
	}

	restricted := authz.Restricted(ctx)
	out := &RunAssertions{RunID: runID, JobID: jobID, Tasks: []TaskAssertions{}}
	for _, taskID := range order {
		group := groups[taskID]
		task := TaskAssertions{
			TaskID:        taskID,
			Fanned:        models.IsFannedGroup(len(group), group[0].PartitionValue),
			InstanceCount: len(group),
			VerdictCounts: make(map[string]int, 4),
		}
		var instances []AssertionInstance
		evidence := false
		for _, row := range group {
			instance, ok := assertionInstance(row, declares)
			evidence = evidence || ok
			if restricted {
				WithholdUnownedBaselineEvidenceFromResults(instance.Results)
				WithholdUnownedBaselineEvidenceFromViolations(instance.Violations)
			}
			instances = append(instances, instance)
		}
		if !evidence {
			continue
		}
		// Once any instance of the group has evidence, every instance is
		// accounted for: a sibling with none is not_recorded, never omitted,
		// so the counts add up to instance_count. The cap below is applied
		// after counting.
		for _, instance := range instances {
			task.VerdictCounts[instance.Verdict]++
		}
		if len(instances) > MaxAssertionInstances {
			// The most worrying first, then back into partition order.
			slices.SortStableFunc(instances, func(a, b AssertionInstance) int {
				return verdictRank(a.Verdict) - verdictRank(b.Verdict)
			})
			instances = instances[:MaxAssertionInstances]
			slices.SortStableFunc(instances, func(a, b AssertionInstance) int {
				return a.PartitionIndex - b.PartitionIndex
			})
			task.InstancesTruncated = true
		}
		task.Instances = instances
		out.Tasks = append(out.Tasks, task)
	}
	return out, nil
}

// assertionInstance projects one row and reports whether the row itself has
// assertion evidence. A row without any is returned as VerdictNotRecorded: the
// caller decides whether its group has evidence to make that worth reporting.
func assertionInstance(row assertionRow, declares map[uuid.UUID]string) (AssertionInstance, bool) {
	instance := AssertionInstance{
		TaskRunID:      row.ID,
		PartitionValue: row.PartitionValue,
		PartitionIndex: row.PartitionIndex,
		Status:         TaskStatus(row.Status),
		Attempt:        row.Attempt,
		CacheHit:       row.CacheHit || TaskStatus(row.Status) == TaskStatusCached,
	}
	results := decodeAssertionResults(row.AssertionResults)
	switch {
	case len(results) > 0:
		instance.Results = results
		instance.Verdict = VerdictOf(results)
	case instance.CacheHit:
		// A cache hit executes nothing, so no assertion ran. Whatever verdict
		// the original execution earned belongs to that run.
		if _, declared := declares[row.TaskID]; !declared {
			instance.Verdict = VerdictNotRecorded
			return instance, false
		}
		instance.Verdict = VerdictNotEvaluated
	case len(row.DataViolations) > 0:
		instance.Verdict = VerdictViolated
		// Unreadable evidence is still a recorded breach; it just has no
		// detail to show.
		_ = json.Unmarshal(row.DataViolations, &instance.Violations)
	default:
		instance.Verdict = VerdictNotRecorded
		return instance, false
	}
	return instance, true
}

// verdictRank orders verdicts most-worrying first for the truncation cap.
func verdictRank(verdict string) int {
	switch verdict {
	case VerdictViolated:
		return 0
	case VerdictUndecided:
		return 1
	case VerdictSeeding:
		return 2
	case VerdictNotEvaluated, VerdictNotRecorded:
		return 3
	default:
		return 4
	}
}

// stepsDeclaringAssertions maps each of taskIDs whose step declares assertions
// on a produced dataset to that step's name.
func (s *Store) stepsDeclaringAssertions(ctx context.Context, jobID uuid.UUID, taskIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	conn := s.db.WithContext(ctx)
	var declared []string
	if err := conn.Model(&models.DatasetDeclaration{}).
		Where("job_id = ? AND direction = ? AND assertions_json IS NOT NULL AND assertions_json <> ''",
			jobID, models.DatasetDirectionProduces).
		Distinct().Pluck("step_name", &declared).Error; err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]string)
	if len(declared) == 0 {
		return out, nil
	}
	for _, chunk := range chunkTaskRunIDs(taskIDs) {
		var tasks []models.Task
		if err := conn.Unscoped().Select("id", "name").Where("id IN ?", chunk).Find(&tasks).Error; err != nil {
			return nil, err
		}
		for _, task := range tasks {
			if slices.Contains(declared, task.Name) {
				out[task.ID] = task.Name
			}
		}
	}
	return out, nil
}

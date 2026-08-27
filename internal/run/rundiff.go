package run

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// RunDiffVerdict is the per-task verdict for an explicit left -> right run
// comparison.
type RunDiffVerdict string

const (
	// RunDiffVerdictWouldCacheHit means the compared task HashInput blobs carry
	// the same identity hash; relative to the left run, the right run would have
	// cache-hit.
	RunDiffVerdictWouldCacheHit RunDiffVerdict = "WOULD_CACHE_HIT"
	// RunDiffVerdictReran means the compared task HashInput blobs differ; the
	// field changes explain why the right-side task re-ran relative to the left.
	RunDiffVerdictReran RunDiffVerdict = "RERAN"
	// RunDiffVerdictDegraded means the task pair could not be diffed
	// field-by-field because one side's persisted blob is missing or degraded.
	RunDiffVerdictDegraded RunDiffVerdict = "DEGRADED"
)

// RunDiff is the machine-readable, read-side diff of two runs of the same job.
// It is cache-bust attribution only: it compares persisted HashInput blobs and
// trigger/run params, not row- or column-level data values.
type RunDiff struct {
	JobID      uuid.UUID `json:"jobId"`
	LeftRunID  uuid.UUID `json:"leftRunId"`
	RightRunID uuid.UUID `json:"rightRunId"`

	LeftStatus  Status `json:"leftStatus"`
	RightStatus Status `json:"rightStatus"`

	LeftTrigger  WhyTrigger `json:"leftTrigger"`
	RightTrigger WhyTrigger `json:"rightTrigger"`

	TriggerChanges    []FieldChange `json:"triggerChanges,omitempty"`
	ParamChanges      []FieldChange `json:"paramChanges,omitempty"`
	Tasks             []RunDiffTask `json:"tasks"`
	TasksAdded        []string      `json:"tasksAdded,omitempty"`
	TasksRemoved      []string      `json:"tasksRemoved,omitempty"`
	PartitionsAdded   []string      `json:"partitionsAdded,omitempty"`
	PartitionsRemoved []string      `json:"partitionsRemoved,omitempty"`
	GeneratedAt       time.Time     `json:"generatedAt"`
}

// RunDiffTask is one paired task-name comparison in a RunDiff.
type RunDiffTask struct {
	TaskName  string `json:"taskName"`
	Partition string `json:"partition,omitempty"`

	LeftTaskRunID  uuid.UUID `json:"leftTaskRunId"`
	RightTaskRunID uuid.UUID `json:"rightTaskRunId"`
	LeftTaskID     uuid.UUID `json:"leftTaskId"`
	RightTaskID    uuid.UUID `json:"rightTaskId"`

	LeftStatus   TaskStatus `json:"leftStatus"`
	RightStatus  TaskStatus `json:"rightStatus"`
	LeftAttempt  int        `json:"leftAttempt"`
	RightAttempt int        `json:"rightAttempt"`
	LeftHash     string     `json:"leftHash,omitempty"`
	RightHash    string     `json:"rightHash,omitempty"`

	Verdict   RunDiffVerdict `json:"verdict"`
	HashEqual bool           `json:"hashEqual"`
	Changes   []FieldChange  `json:"changes,omitempty"`
	Degraded  string         `json:"degraded,omitempty"`
}

var (
	// ErrRunDiffJobMismatch is returned when either run does not belong to the
	// requested job, or the two runs are from different jobs.
	ErrRunDiffJobMismatch = errors.New("run: run diff job mismatch")
)

// DiffRuns compares the latest terminal task-runs in rightRunID against
// leftRunID for one job. It pairs rows by task name, diffs each pair's
// persisted HashInput blob via DiffHashInputBlobs, and returns a JSON-ready
// read model for API/CLI layers to render.
//
// Pairing considers terminal task-runs only (Succeeded/Failed/Skipped/Cached).
// A task with no terminal task-run on one side is treated as absent there and
// reported in TasksAdded/TasksRemoved; this read model is intended for two
// completed runs, where every task has a terminal attempt.
func (s *Store) DiffRuns(ctx context.Context, jobID, leftRunID, rightRunID uuid.UUID) (*RunDiff, error) {
	db := s.db.WithContext(ctx)

	leftRun, err := loadRunDiffRun(db, leftRunID)
	if err != nil {
		return nil, err
	}
	rightRun, err := loadRunDiffRun(db, rightRunID)
	if err != nil {
		return nil, err
	}
	if leftRun.JobID != jobID || rightRun.JobID != jobID || leftRun.JobID != rightRun.JobID {
		return nil, fmt.Errorf("%w: left run job=%s right run job=%s requested job=%s",
			ErrRunDiffJobMismatch, leftRun.JobID, rightRun.JobID, jobID)
	}

	leftTasks, err := s.latestTerminalTaskRunsByName(ctx, leftRunID)
	if err != nil {
		return nil, err
	}
	rightTasks, err := s.latestTerminalTaskRunsByName(ctx, rightRunID)
	if err != nil {
		return nil, err
	}

	leftTrigger := s.loadTrigger(ctx, leftRun)
	rightTrigger := s.loadTrigger(ctx, rightRun)

	out := &RunDiff{
		JobID:          jobID,
		LeftRunID:      leftRunID,
		RightRunID:     rightRunID,
		LeftStatus:     Status(leftRun.Status),
		RightStatus:    Status(rightRun.Status),
		LeftTrigger:    leftTrigger,
		RightTrigger:   rightTrigger,
		TriggerChanges: diffRunTriggers(leftTrigger, rightTrigger),
		ParamChanges:   diffStringMap("params", leftTrigger.Params, rightTrigger.Params),
		Tasks:          make([]RunDiffTask, 0),
		GeneratedAt:    time.Now().UTC(),
	}

	for _, instanceID := range sortedInstanceIDs(leftTasks, rightTasks) {
		leftTask, leftOK := leftTasks[instanceID]
		rightTask, rightOK := rightTasks[instanceID]
		switch {
		case !leftOK:
			label := runDiffInstanceLabel(rightTask.TaskName, rightTask.PartitionValue)
			if rightTask.PartitionValue != "" {
				out.PartitionsAdded = append(out.PartitionsAdded, label)
			} else {
				out.TasksAdded = append(out.TasksAdded, label)
			}
		case !rightOK:
			label := runDiffInstanceLabel(leftTask.TaskName, leftTask.PartitionValue)
			if leftTask.PartitionValue != "" {
				out.PartitionsRemoved = append(out.PartitionsRemoved, label)
			} else {
				out.TasksRemoved = append(out.TasksRemoved, label)
			}
		default:
			dt := diffRunTask(leftTask.TaskName, leftTask.TaskRun, rightTask.TaskRun)
			dt.Partition = leftTask.PartitionValue
			if dt.Partition == "" {
				dt.Partition = rightTask.PartitionValue
			}
			out.Tasks = append(out.Tasks, dt)
		}
	}

	return out, nil
}

type runDiffTaskRunRow struct {
	models.TaskRun
	TaskName string
}

func loadRunDiffRun(db *gorm.DB, runID uuid.UUID) (*models.JobRun, error) {
	var run models.JobRun
	if err := db.First(&run, "id = ?", runID).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// runDiffInstanceID identifies one comparable unit in a run diff: an unfanned
// task (empty Partition) or one fan-out instance of it.
//
// It is a STRUCT, not the string taskName+":"+partition it replaced. That
// concatenation was ambiguous — task "a" partition "b:c" and task "a:b"
// partition "c" both flattened to "a:b:c" — so two unrelated instances shared a
// pairing slot and one silently overwrote the other, dropping a task from the
// diff and comparing rows that never corresponded. Neither component is
// constrained to exclude ":", so the only sound key is one that never joins
// them.
type runDiffInstanceID struct {
	TaskName  string
	Partition string
}

func (id runDiffInstanceID) less(other runDiffInstanceID) bool {
	if id.TaskName != other.TaskName {
		return id.TaskName < other.TaskName
	}
	return id.Partition < other.Partition
}

func (s *Store) latestTerminalTaskRunsByName(ctx context.Context, runID uuid.UUID) (map[runDiffInstanceID]runDiffTaskRunRow, error) {
	var rows []runDiffTaskRunRow
	if err := s.db.WithContext(ctx).
		Table("task_runs").
		Select("task_runs.*, tasks.name AS task_name").
		Joins("JOIN tasks ON tasks.id = task_runs.task_id").
		Where("task_runs.job_run_id = ? AND task_runs.status IN ?", runID, terminalTaskStatuses()).
		Order("tasks.name ASC, task_runs.attempt ASC, task_runs.terminal_sequence ASC, task_runs.updated_at ASC, task_runs.id ASC").
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	byName := make(map[runDiffInstanceID]runDiffTaskRunRow, len(rows))
	for _, row := range rows {
		key := runDiffInstanceID{TaskName: row.TaskName, Partition: row.PartitionValue}
		if current, ok := byName[key]; !ok || laterTerminalTaskRun(row.TaskRun, current.TaskRun) {
			byName[key] = row
		}
	}
	return byName, nil
}

// runDiffInstanceLabel is the human-readable name an added/removed instance is
// reported under. It is presentation only — never a map key (see
// runDiffInstanceID for why).
func runDiffInstanceLabel(taskName, partition string) string {
	if partition == "" {
		return taskName
	}
	return taskName + ":" + partition
}

func terminalTaskStatuses() []string {
	return []string{
		string(TaskStatusSucceeded),
		string(TaskStatusFailed),
		string(TaskStatusSkipped),
		string(TaskStatusCached),
		string(TaskStatusCancelled),
	}
}

func laterTerminalTaskRun(candidate, current models.TaskRun) bool {
	if candidate.Attempt != current.Attempt {
		return candidate.Attempt > current.Attempt
	}
	if candidate.TerminalSequence != current.TerminalSequence {
		return candidate.TerminalSequence > current.TerminalSequence
	}
	if !sameOptionalTime(candidate.CompletedAt, current.CompletedAt) {
		return optionalTimeAfter(candidate.CompletedAt, current.CompletedAt)
	}
	if !candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return candidate.UpdatedAt.After(current.UpdatedAt)
	}
	if !candidate.CreatedAt.Equal(current.CreatedAt) {
		return candidate.CreatedAt.After(current.CreatedAt)
	}
	return candidate.ID.String() > current.ID.String()
}

func sameOptionalTime(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
}

func optionalTimeAfter(a, b *time.Time) bool {
	switch {
	case a == nil:
		return false
	case b == nil:
		return true
	default:
		return a.After(*b)
	}
}

func sortedInstanceIDs(left, right map[runDiffInstanceID]runDiffTaskRunRow) []runDiffInstanceID {
	seen := make(map[runDiffInstanceID]struct{}, len(left)+len(right))
	for id := range left {
		seen[id] = struct{}{}
	}
	for id := range right {
		seen[id] = struct{}{}
	}
	ids := make([]runDiffInstanceID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].less(ids[j]) })
	return ids
}

func diffRunTask(taskName string, left, right models.TaskRun) RunDiffTask {
	base := RunDiffTask{
		TaskName:       taskName,
		LeftTaskRunID:  left.ID,
		RightTaskRunID: right.ID,
		LeftTaskID:     left.TaskID,
		RightTaskID:    right.TaskID,
		LeftStatus:     TaskStatus(left.Status),
		RightStatus:    TaskStatus(right.Status),
		LeftAttempt:    left.Attempt,
		RightAttempt:   right.Attempt,
		LeftHash:       left.Hash,
		RightHash:      right.Hash,
	}

	// DiffHashInputBlobs(subject, baseline): right is the subject (newer/after),
	// left the baseline (older/before), so the resulting FieldChanges read
	// Before=left, After=right — matching RunDiff's left→right framing. Do not
	// swap these to match the left,right parameter order.
	diff, err := DiffHashInputBlobs(right.HashInputBlob, left.HashInputBlob)
	if err != nil {
		// A decode failure on one task's persisted blob must not abort the
		// whole run diff; degrade just this task, mirroring why's per-task
		// degraded handling.
		base.Verdict = RunDiffVerdictDegraded
		base.Degraded = fmt.Sprintf("decode hash-input blob: %v", err)
		return base
	}

	base.Verdict = classifyRunDiffVerdict(diff)
	base.HashEqual = diff.HashEqual
	// Only genuinely discriminating fields: `run diff` reports "N changed
	// fields" per task, and a chain: values exclusion marker is an explanation of
	// how the key was built, not an input that differed. Naming the exclusion is
	// `caesium why`'s job (spec §4.3), which has a Notes channel for it; counting
	// it here would report an identical pair of tasks as having a changed field.
	base.Changes = discriminatingChanges(diff.Changes)
	base.Degraded = diff.Degraded
	return base
}

func classifyRunDiffVerdict(diff *BlobDiff) RunDiffVerdict {
	if diff == nil || diff.Degraded != "" {
		return RunDiffVerdictDegraded
	}
	if diff.HashEqual {
		return RunDiffVerdictWouldCacheHit
	}
	return RunDiffVerdictReran
}

func diffRunTriggers(left, right WhyTrigger) []FieldChange {
	var changes []FieldChange
	addScalar := func(field, before, after string) {
		if before != after {
			changes = append(changes, FieldChange{Field: field, Kind: fieldScalar, Before: before, After: after})
		}
	}

	addScalar("trigger.type", left.Type, right.Type)
	addScalar("trigger.alias", left.Alias, right.Alias)
	return changes
}

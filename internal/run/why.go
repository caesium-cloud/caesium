package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// why.go is the read-side query that powers `caesium why <run> --task <t>`
// (data-plane-memory A3). It explains why a task in a given run executed, hit the
// cache, or re-ran, by:
//
//  1. classifying the verdict (cache HIT / cache MISS / cache OFF) from the
//     task-run's persisted status + cache columns;
//  2. diffing this run's persisted, canonical HashInput blob against a baseline
//     blob (the cache-origin entry for a hit; the most-recent prior run of the
//     same task for a miss) to name the discriminating input field(s) — see
//     whydiff.go; and
//  3. joining the ExecutionEvent store for trigger-side causation (what fired the
//     run and with which params).
//
// It is honestly scoped: it attributes which declared input or upstream
// data-contract output changed, NOT row/column-level data causality. Everything
// is a read of already-persisted state; nothing is recomputed.

// WhyVerdict is the high-level cache outcome for the explained task.
type WhyVerdict string

const (
	// VerdictCacheHit — the task did not execute; its identity hash matched a
	// live cache entry and the prior result was reused.
	VerdictCacheHit WhyVerdict = "CACHE_HIT"
	// VerdictCacheMiss — caching was enabled but the task's identity hash did not
	// match any live cache entry, so the task executed. The diff names what
	// changed versus the prior run (had it been unchanged, this would have
	// skipped).
	VerdictCacheMiss WhyVerdict = "CACHE_MISS"
	// VerdictCacheOff — caching was not enabled for this task, so it executed
	// unconditionally and no hit/miss attribution applies. A field diff versus a
	// prior run is still offered when a blob exists.
	VerdictCacheOff WhyVerdict = "CACHE_DISABLED"
	// VerdictUnknown — the task run is not in a terminal/decided state (e.g. still
	// pending or running), so no cache verdict can be given yet.
	VerdictUnknown WhyVerdict = "UNKNOWN"
)

// WhyTrigger captures the trigger-side causation for the run, read from the
// run row and the run_started ExecutionEvent.
type WhyTrigger struct {
	// Type is the trigger type that fired the run (e.g. "cron", "http",
	// "manual"), as recorded on the run.
	Type string `json:"type,omitempty"`
	// Alias is the trigger alias, when set.
	Alias string `json:"alias,omitempty"`
	// Params are the run parameters captured at trigger time; these feed into the
	// HashInput (RunParams), so a changed param is also a possible miss cause and
	// will appear in the diff under "runParams.<key>".
	Params map[string]string `json:"params,omitempty"`
	// FiredAt is the run's start time.
	FiredAt time.Time `json:"firedAt,omitempty"`
}

// WhyBaseline describes which run/entry the subject was diffed against, so the
// answer is auditable.
type WhyBaseline struct {
	// Kind is "cache_origin" (the run that populated the matched cache entry),
	// "prior_run" (the most-recent earlier run of the same task), or "none".
	Kind string `json:"kind"`
	// RunID is the baseline run, when applicable.
	RunID *uuid.UUID `json:"runId,omitempty"`
	// TaskRunID is the baseline task-run, when applicable.
	TaskRunID *uuid.UUID `json:"taskRunId,omitempty"`
	// StartedAt is when the baseline run started, when known.
	StartedAt *time.Time `json:"startedAt,omitempty"`
}

// WhyExplanation is the full, machine-readable answer the why service returns.
// It is rendered as JSON by the API and as both a table and JSON by the CLI.
type WhyExplanation struct {
	RunID     uuid.UUID `json:"runId"`
	JobID     uuid.UUID `json:"jobId"`
	TaskID    uuid.UUID `json:"taskId"`
	TaskName  string    `json:"taskName"`
	TaskRunID uuid.UUID `json:"taskRunId"`

	Verdict WhyVerdict `json:"verdict"`
	Status  string     `json:"status"`
	// CacheEnabled reflects whether caching applied to this task.
	CacheEnabled bool `json:"cacheEnabled"`
	// Hash is this task-run's identity hash.
	Hash string `json:"hash,omitempty"`

	// Summary is a one-line human-readable explanation, e.g.
	// "CACHE_MISS — predecessor `extract.row_count` changed 1.2M→1.4M; image,
	// command, env identical".
	Summary string `json:"summary"`

	Trigger  WhyTrigger  `json:"trigger"`
	Baseline WhyBaseline `json:"baseline"`
	Diff     *BlobDiff   `json:"diff,omitempty"`
}

// ErrTaskRunNotFound is returned when no task matching the given id/name exists
// in the run.
var ErrTaskRunNotFound = errors.New("run: task not found in run")

// WhyTask explains why the task identified by taskRef (a task UUID or a task
// name) in run runID executed / hit cache / re-ran. taskRef is matched first as
// a UUID against task_id, then as a task name within the run's job.
func (s *Store) WhyTask(ctx context.Context, runID uuid.UUID, taskRef string) (*WhyExplanation, error) {
	var jobRun models.JobRun
	if err := s.db.WithContext(ctx).First(&jobRun, "id = ?", runID).Error; err != nil {
		return nil, err
	}

	taskRun, taskName, err := s.resolveTaskRun(ctx, runID, jobRun.JobID, taskRef)
	if err != nil {
		return nil, err
	}

	exp := &WhyExplanation{
		RunID:        runID,
		JobID:        jobRun.JobID,
		TaskID:       taskRun.TaskID,
		TaskName:     taskName,
		TaskRunID:    taskRun.ID,
		Status:       taskRun.Status,
		CacheEnabled: taskRun.CacheEnabled,
		Hash:         taskRun.Hash,
		Verdict:      classifyVerdict(taskRun),
	}

	exp.Trigger = s.loadTrigger(ctx, &jobRun)

	baselineBlob, baseline, err := s.resolveBaseline(ctx, taskRun, jobRun.JobID, jobRun.StartedAt)
	if err != nil {
		return nil, err
	}
	exp.Baseline = baseline

	diff, err := DiffHashInputBlobs(taskRun.HashInputBlob, baselineBlob)
	if err != nil {
		return nil, err
	}
	exp.Diff = diff

	exp.Summary = summarize(exp)
	return exp, nil
}

// resolveTaskRun finds the task_runs row for taskRef in the run, returning the
// row and the resolved task name. taskRef is tried as a task UUID first, then as
// a task name (looked up via the job's tasks table).
func (s *Store) resolveTaskRun(ctx context.Context, runID, jobID uuid.UUID, taskRef string) (*models.TaskRun, string, error) {
	db := s.db.WithContext(ctx)

	var taskID uuid.UUID
	taskName := taskRef
	if parsed, perr := uuid.Parse(taskRef); perr == nil {
		taskID = parsed
	} else {
		// Resolve by name within the job.
		var task models.Task
		if err := db.Select("id", "name").
			Where("job_id = ? AND name = ?", jobID, taskRef).
			First(&task).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, "", ErrTaskRunNotFound
			}
			return nil, "", err
		}
		taskID = task.ID
	}

	var taskRun models.TaskRun
	if err := db.Where("job_run_id = ? AND task_id = ?", runID, taskID).
		First(&taskRun).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", ErrTaskRunNotFound
		}
		return nil, "", err
	}

	// Backfill the task name when taskRef was a UUID.
	if _, perr := uuid.Parse(taskRef); perr == nil {
		var task models.Task
		if err := db.Select("name").First(&task, "id = ?", taskID).Error; err == nil && task.Name != "" {
			taskName = task.Name
		}
	}

	return &taskRun, taskName, nil
}

func classifyVerdict(tr *models.TaskRun) WhyVerdict {
	switch TaskStatus(tr.Status) {
	case TaskStatusCached:
		return VerdictCacheHit
	case TaskStatusSucceeded, TaskStatusFailed:
		if tr.CacheEnabled {
			return VerdictCacheMiss
		}
		return VerdictCacheOff
	default:
		return VerdictUnknown
	}
}

func (s *Store) loadTrigger(ctx context.Context, jobRun *models.JobRun) WhyTrigger {
	t := WhyTrigger{
		Type:    jobRun.TriggerType,
		Alias:   jobRun.TriggerAlias,
		FiredAt: jobRun.StartedAt,
	}
	if len(jobRun.Params) > 0 {
		var params map[string]string
		if err := json.Unmarshal(jobRun.Params, &params); err == nil {
			t.Params = params
		}
	}

	// Enrich trigger type/alias from the run_started event payload when the run
	// row left them blank (older rows recorded trigger context only in the
	// event). Best-effort: a missing or unparseable event leaves the row values.
	if (t.Type == "" || t.Alias == "") && s.eventStore != nil {
		evts, err := s.eventStore.ListSince(ctx, 0, 1, event.Filter{
			RunID: jobRun.ID,
			Types: []event.Type{event.TypeRunStarted},
		})
		if err == nil && len(evts) > 0 && len(evts[0].Payload) > 0 {
			var payload struct {
				TriggerType  string            `json:"trigger_type"`
				TriggerAlias string            `json:"trigger_alias"`
				Params       map[string]string `json:"params"`
			}
			if json.Unmarshal(evts[0].Payload, &payload) == nil {
				if t.Type == "" {
					t.Type = payload.TriggerType
				}
				if t.Alias == "" {
					t.Alias = payload.TriggerAlias
				}
				if len(t.Params) == 0 && len(payload.Params) > 0 {
					t.Params = payload.Params
				}
			}
		}
	}

	return t
}

// resolveBaseline picks the blob to diff the subject against and describes it.
//
//   - Cache HIT: the cache-origin run's task blob (same task, run =
//     CacheOriginRunID). By construction its identity hash equals the subject's,
//     so the diff confirms every hashed input was identical — the proof of the
//     hit. Falls back to the live TaskCache entry's blob (keyed by the subject
//     hash) if the origin task-run row is gone.
//   - Cache MISS / OFF: the most-recent earlier run of the same task that has a
//     persisted blob, so the diff names what changed and forced the re-run.
// subjectStartedAt is the subject run's start time (already loaded by the
// caller); the prior-run lookup uses it to consider only strictly-earlier runs,
// avoiding a redundant re-query.
func (s *Store) resolveBaseline(ctx context.Context, subject *models.TaskRun, jobID uuid.UUID, subjectStartedAt time.Time) ([]byte, WhyBaseline, error) {
	db := s.db.WithContext(ctx)

	// A cached task is always a cache hit, regardless of whether
	// CacheOriginRunID is populated: try the named origin task-run first, then
	// fall back to the live cache entry keyed by the subject's hash. (A nil
	// CacheOriginRunID must not fall through to the MISS path — that would
	// mislabel a hit as a re-run.)
	if TaskStatus(subject.Status) == TaskStatusCached {
		if subject.CacheOriginRunID != nil {
			var origin models.TaskRun
			err := db.Where("job_run_id = ? AND task_id = ?", *subject.CacheOriginRunID, subject.TaskID).
				First(&origin).Error
			if err == nil {
				b := WhyBaseline{Kind: "cache_origin", RunID: subject.CacheOriginRunID, TaskRunID: &origin.ID}
				if originStart := origin.StartedAt; originStart != nil {
					b.StartedAt = originStart
				}
				if len(origin.HashInputBlob) > 0 {
					return origin.HashInputBlob, b, nil
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, WhyBaseline{}, err
			}
		}

		// Fall back to the live cache entry keyed by the subject's hash.
		if subject.Hash != "" {
			var entry models.TaskCache
			if err := db.Where("hash = ?", subject.Hash).First(&entry).Error; err == nil {
				b := WhyBaseline{Kind: "cache_origin"}
				originRunID := entry.RunID
				b.RunID = &originRunID
				if len(entry.HashInputBlob) > 0 {
					return entry.HashInputBlob, b, nil
				}
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, WhyBaseline{}, err
			}
		}

		return nil, WhyBaseline{Kind: "none"}, nil
	}

	// MISS / OFF: most-recent earlier run of the same task with a blob.
	var prior models.TaskRun
	err := db.
		Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id").
		Where("task_runs.task_id = ? AND job_runs.job_id = ? AND task_runs.job_run_id <> ? AND job_runs.started_at < ? AND task_runs.hash_input_blob IS NOT NULL",
			subject.TaskID, jobID, subject.JobRunID, subjectStartedAt).
		Order("job_runs.started_at DESC").
		First(&prior).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, WhyBaseline{Kind: "none"}, nil
		}
		return nil, WhyBaseline{}, err
	}

	b := WhyBaseline{Kind: "prior_run", RunID: &prior.JobRunID, TaskRunID: &prior.ID}
	if prior.StartedAt != nil {
		b.StartedAt = prior.StartedAt
	}
	return prior.HashInputBlob, b, nil
}

// summarize renders the one-line human-readable verdict from the structured
// explanation: the headline discriminating field for a miss, or the
// identical-inputs proof for a hit.
func summarize(exp *WhyExplanation) string {
	switch exp.Verdict {
	case VerdictCacheHit:
		if exp.Diff != nil && exp.Diff.HashEqual {
			return fmt.Sprintf("CACHE HIT — every hashed input identical to the cached run; the prior result was reused (task %q did not execute)", exp.TaskName)
		}
		return fmt.Sprintf("CACHE HIT — task %q reused a cached result", exp.TaskName)
	case VerdictCacheMiss:
		return summarizeChanged(exp, "CACHE MISS", "re-ran")
	case VerdictCacheOff:
		if exp.Diff != nil && len(exp.Diff.Changes) > 0 {
			return summarizeChanged(exp, "CACHE DISABLED", "ran")
		}
		return fmt.Sprintf("CACHE DISABLED — caching was not enabled for task %q, so it ran unconditionally", exp.TaskName)
	default:
		return fmt.Sprintf("task %q is %s — no cache verdict yet", exp.TaskName, exp.Status)
	}
}

func summarizeChanged(exp *WhyExplanation, verdict, ranVerb string) string {
	if exp.Diff == nil {
		return fmt.Sprintf("%s — task %q %s", verdict, exp.TaskName, ranVerb)
	}
	// No comparison run at all is the "first run" case — report that rather than
	// the generic degraded-blob message (the diff degrades because the baseline
	// blob is absent, but the *reason* it is absent is that there is nothing to
	// compare against, which is the more useful thing to say).
	if exp.Baseline.Kind == "none" {
		return fmt.Sprintf("%s — task %q %s; no prior run to compare against (first run of this task)", verdict, exp.TaskName, ranVerb)
	}
	if exp.Diff.Degraded != "" {
		return fmt.Sprintf("%s — task %q %s; %s", verdict, exp.TaskName, ranVerb, exp.Diff.Degraded)
	}
	if len(exp.Diff.Changes) == 0 {
		return fmt.Sprintf("%s — task %q %s; no input field differs from the prior run (cause is outside the persisted hash inputs, e.g. an expired/pruned cache entry)", verdict, exp.TaskName, ranVerb)
	}

	head := exp.Diff.Changes[0]
	detail := describeChange(head)
	if len(exp.Diff.Changes) > 1 {
		detail = fmt.Sprintf("%s (and %d other field(s))", detail, len(exp.Diff.Changes)-1)
	}
	return fmt.Sprintf("%s — %s", verdict, detail)
}

// describeChange renders a single FieldChange as a human phrase. Redacted env
// values are labeled rather than printed as if literal.
func describeChange(c FieldChange) string {
	switch {
	case c.Added:
		if c.Redacted {
			return fmt.Sprintf("`%s` was added (redacted; digest %s)", c.Field, c.After)
		}
		if c.Kind == fieldStructural {
			return fmt.Sprintf("`%s` was added", c.Field)
		}
		return fmt.Sprintf("`%s` was added (%s)", c.Field, c.After)
	case c.Removed:
		if c.Redacted {
			return fmt.Sprintf("`%s` was removed (redacted; digest %s)", c.Field, c.Before)
		}
		if c.Kind == fieldStructural {
			return fmt.Sprintf("`%s` was removed", c.Field)
		}
		return fmt.Sprintf("`%s` was removed (%s)", c.Field, c.Before)
	case c.Kind == fieldStructural:
		return fmt.Sprintf("`%s` changed", c.Field)
	case c.Redacted:
		return fmt.Sprintf("`%s` changed (redacted; digest %s→%s)", c.Field, c.Before, c.After)
	default:
		return fmt.Sprintf("`%s` changed %s→%s", c.Field, c.Before, c.After)
	}
}

package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
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
	// "prior_run" (the most-recent earlier run of the same task), "per_partition"
	// (the subject is a fanned GROUP, so the baseline is per-instance and only a
	// --partition selection can name one), or "none".
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
	RunID    uuid.UUID `json:"runId"`
	JobID    uuid.UUID `json:"jobId"`
	TaskID   uuid.UUID `json:"taskId"`
	TaskName string    `json:"taskName"`
	// TaskRunID is the explained instance's TaskRun primary key. It is nil — and
	// omitted from the JSON — for a fanned GROUP summary, which has no single
	// instance; select one with a partition to get it back.
	TaskRunID *uuid.UUID `json:"taskRunId,omitempty"`
	// Partition is the selected instance's partition value. Empty (omitted) for
	// an unfanned task and for a group summary.
	Partition string `json:"partition,omitempty"`

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

	// Group is populated ONLY for a fanned step explained without a partition
	// selector: the aggregate answer over all N instances. It is omitted for an
	// unfanned task and for a single selected instance, so unfanned output is
	// byte-identical to the pre-fan-out shape.
	Group *WhyGroup `json:"group,omitempty"`
}

// WhyGroup is the aggregate explanation for a fanned step. `caesium why --task
// <name>` on a fanned step cannot answer "why did THE task run" — there are N
// instances with N identity hashes and possibly N different verdicts — so it
// answers about the group and points at the per-instance selector.
type WhyGroup struct {
	// PartitionCount is the number of live instance rows in the group.
	PartitionCount int `json:"partitionCount"`
	// StatusCounts is the status histogram over the instances, keyed by task
	// status ("succeeded", "failed", "cached", "skipped", "running", ...).
	StatusCounts map[string]int `json:"statusCounts"`
	// CacheHits counts instances served from cache.
	CacheHits int `json:"cacheHits"`
	// Partitions lists the group's partition values in emission order, capped at
	// whyGroupPartitionListCap so a 10k-instance group does not bloat the
	// response; PartitionsTruncated reports the cap.
	Partitions          []string `json:"partitions,omitempty"`
	PartitionsTruncated bool     `json:"partitionsTruncated,omitempty"`
	// FirstFailure is the lowest-index failed instance — the one whose cause
	// explains the group's failed status. Nil when no instance failed.
	FirstFailure *WhyGroupFailure `json:"firstFailure,omitempty"`
	// StartedAt / CompletedAt are the aggregate envelope: the earliest instance
	// start and the latest instance end. DurationMS is their span (0 while the
	// group is still running).
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	DurationMS  int64      `json:"durationMs,omitempty"`
	// Notes are qualifiers about HOW the group's instance keys were computed, the
	// group-level counterpart of BlobDiff.Notes. A group carries no Diff — it has
	// N hashes and N baselines — so without this channel the flagship shape of
	// this feature (spec 5.5 puts `fanOut` AND `chain: values` on the same step)
	// would answer "6 cached, 1 succeeded" and never mention that predecessor
	// hashes were excluded from those six keys. That is the unexplainable skip
	// spec 4.3 exists to prevent, and an operator who does not yet know the
	// partition keys reaches the group form first.
	Notes []string `json:"notes,omitempty"`
}

// WhyGroupFailure names the instance a fanned group's failure is attributed to.
type WhyGroupFailure struct {
	Partition      string    `json:"partition"`
	PartitionIndex int       `json:"partitionIndex"`
	TaskRunID      uuid.UUID `json:"taskRunId"`
	Status         string    `json:"status"`
	Attempt        int       `json:"attempt"`
	Error          string    `json:"error,omitempty"`
}

// whyGroupPartitionListCap bounds the partition-value list echoed in a group
// summary and in the ErrPartitionNotFound message.
const whyGroupPartitionListCap = 50

// ErrTaskRunNotFound is returned when no task matching the given id/name exists
// in the run.
var ErrTaskRunNotFound = errors.New("run: task not found in run")

// ErrPartitionNotFound is returned when a --partition selector names a value
// that the task's fan-out group does not contain (or when the task is not fanned
// at all). Its message lists the available partition values so the operator can
// retry without a second round trip.
var ErrPartitionNotFound = errors.New("run: partition not found")

// WhyTask explains why the task identified by taskRef (a task UUID or a task
// name) in run runID executed / hit cache / re-ran. taskRef is matched first as
// a UUID against task_id, then as a task name within the run's job.
//
// On a FANNED step it returns the group summary (see WhyTaskPartition for the
// per-instance answer).
func (s *Store) WhyTask(ctx context.Context, runID uuid.UUID, taskRef string) (*WhyExplanation, error) {
	return s.WhyTaskPartition(ctx, runID, taskRef, "")
}

// WhyTaskPartition is WhyTask with an explicit fan-out instance selector.
//
//   - partition == "": an unfanned task is explained exactly as before; a fanned
//     step returns the aggregate WhyGroup summary rather than an arbitrary
//     sibling's explanation (the pre-fan-out code path `.First()`-ed one row of
//     N, so the answer depended on database row order).
//   - partition != "": the named instance is explained with the full
//     single-instance diff. ErrPartitionNotFound (listing the available values)
//     when the group has no such partition.
func (s *Store) WhyTaskPartition(ctx context.Context, runID uuid.UUID, taskRef, partition string) (*WhyExplanation, error) {
	var jobRun models.JobRun
	if err := s.db.WithContext(ctx).First(&jobRun, "id = ?", runID).Error; err != nil {
		return nil, err
	}

	instances, taskID, taskName, err := s.resolveTaskRuns(ctx, runID, jobRun.JobID, taskRef)
	if err != nil {
		return nil, err
	}

	subject, err := selectWhyInstance(instances, taskName, partition)
	if err != nil {
		return nil, err
	}

	// Fanned group, no selector: answer about the group.
	if subject == nil {
		exp := newWhyGroupExplanation(runID, jobRun.JobID, taskID, taskName, instances)
		exp.Trigger = s.loadTrigger(ctx, &jobRun)
		exp.Summary = summarize(exp)
		return exp, nil
	}

	taskRunID := subject.ID
	exp := &WhyExplanation{
		RunID:        runID,
		JobID:        jobRun.JobID,
		TaskID:       subject.TaskID,
		TaskName:     taskName,
		TaskRunID:    &taskRunID,
		Partition:    subject.PartitionValue,
		Status:       subject.Status,
		CacheEnabled: subject.CacheEnabled,
		Hash:         subject.Hash,
		Verdict:      classifyVerdict(subject),
	}

	exp.Trigger = s.loadTrigger(ctx, &jobRun)

	baselineBlob, baseline, err := s.resolveBaseline(ctx, subject, jobRun.JobID, jobRun.StartedAt)
	if err != nil {
		return nil, err
	}
	exp.Baseline = baseline

	diff, err := DiffHashInputBlobs(subject.HashInputBlob, baselineBlob)
	if err != nil {
		return nil, err
	}
	exp.Diff = diff

	exp.Summary = summarize(exp)
	return exp, nil
}

// resolveTaskRuns finds EVERY task_runs row for taskRef in the run, in stable
// partition order, plus the resolved task id and name. taskRef is tried as a
// task UUID first, then as a task name (looked up via the job's tasks table).
//
// It deliberately returns the whole group: the historic `.First()` on
// (job_run_id, task_id) silently answered for an arbitrary sibling once a task
// could fan out.
func (s *Store) resolveTaskRuns(ctx context.Context, runID, jobID uuid.UUID, taskRef string) ([]models.TaskRun, uuid.UUID, string, error) {
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
				return nil, uuid.Nil, "", ErrTaskRunNotFound
			}
			return nil, uuid.Nil, "", err
		}
		taskID = task.ID
	}

	rows, err := loadTaskRunsOrdered(db, runID, taskID)
	if err != nil {
		if isNotFound(err) {
			return nil, uuid.Nil, "", ErrTaskRunNotFound
		}
		return nil, uuid.Nil, "", err
	}

	// Backfill the task name when taskRef was a UUID.
	if _, perr := uuid.Parse(taskRef); perr == nil {
		var task models.Task
		if err := db.Select("name").First(&task, "id = ?", taskID).Error; err == nil && task.Name != "" {
			taskName = task.Name
		}
	}

	return rows, taskID, taskName, nil
}

// selectWhyInstance applies the E3 selection contract to an ordered instance
// slice. It returns (nil, nil) to mean "explain the group": a fanned step with
// no partition selector. An unfanned task always resolves to its single row, so
// its explanation is unchanged by fan-out.
func selectWhyInstance(rows []models.TaskRun, taskName, partition string) (*models.TaskRun, error) {
	if len(rows) == 0 {
		return nil, ErrTaskRunNotFound
	}

	if partition == "" {
		if isFannedGroup(rows) {
			return nil, nil
		}
		return &rows[0], nil
	}

	for i := range rows {
		if rows[i].PartitionValue == partition {
			return &rows[i], nil
		}
	}

	if !isFannedGroup(rows) {
		return nil, fmt.Errorf("%w: task %q is not fanned, so it has no partition %q", ErrPartitionNotFound, taskName, partition)
	}
	return nil, fmt.Errorf("%w: task %q has no partition %q; available partitions: %s",
		ErrPartitionNotFound, taskName, partition, strings.Join(partitionValuesCapped(rows), ", "))
}

// isFannedGroup reports whether the instance rows describe a fan-out group. A
// single-partition group (N=1) is still fanned — it carries a partition value —
// and must not be answered as if it were an ordinary task.
func isFannedGroup(rows []models.TaskRun) bool {
	if len(rows) > 1 {
		return true
	}
	return len(rows) == 1 && rows[0].PartitionValue != ""
}

// partitionValuesCapped renders the group's partition values for an operator
// message, truncated at whyGroupPartitionListCap with a "(+N more)" tail so a
// 10k-instance group cannot produce a 10k-token error string.
func partitionValuesCapped(rows []models.TaskRun) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		if i == whyGroupPartitionListCap {
			out = append(out, fmt.Sprintf("(+%d more)", len(rows)-whyGroupPartitionListCap))
			break
		}
		out = append(out, rows[i].PartitionValue)
	}
	return out
}

// newWhyGroupExplanation builds the aggregate answer for a fanned step: the
// status histogram, the cache-hit count, the first failed instance's cause, and
// the start→end envelope over all instances.
func newWhyGroupExplanation(runID, jobID, taskID uuid.UUID, taskName string, rows []models.TaskRun) *WhyExplanation {
	group := &WhyGroup{
		PartitionCount: len(rows),
		StatusCounts:   make(map[string]int, 4),
	}

	cacheEnabled := false
	allTerminal := true
	allCached := true
	// valuesMode is read from the scheduler-set column rather than by decoding N
	// instance blobs: it is the same value every instance of one step carries
	// (the chain is resolved per step, not per partition), and it is populated
	// even when an instance has no blob at all.
	valuesMode := false
	for i := range rows {
		row := &rows[i]
		if row.CacheChain == cache.ChainValues {
			valuesMode = true
		}
		group.StatusCounts[row.Status]++
		status := TaskStatus(row.Status)
		if row.CacheHit || status == TaskStatusCached {
			group.CacheHits++
		} else {
			allCached = false
		}
		if row.CacheEnabled {
			cacheEnabled = true
		}
		if !IsTerminal(status) {
			allTerminal = false
		}
		if row.StartedAt != nil && (group.StartedAt == nil || row.StartedAt.Before(*group.StartedAt)) {
			started := *row.StartedAt
			group.StartedAt = &started
		}
		if row.CompletedAt != nil && (group.CompletedAt == nil || row.CompletedAt.After(*group.CompletedAt)) {
			completed := *row.CompletedAt
			group.CompletedAt = &completed
		}
	}

	group.Partitions = partitionValuesCapped(rows)
	group.PartitionsTruncated = len(rows) > whyGroupPartitionListCap

	if failed := firstFailedTaskRun(rows); failed != nil {
		group.FirstFailure = &WhyGroupFailure{
			Partition:      failed.PartitionValue,
			PartitionIndex: failed.PartitionIndex,
			TaskRunID:      failed.ID,
			Status:         failed.Status,
			Attempt:        failed.Attempt,
			Error:          failed.Error,
		}
	}

	if group.StartedAt != nil && group.CompletedAt != nil && allTerminal {
		group.DurationMS = group.CompletedAt.Sub(*group.StartedAt).Milliseconds()
	}

	if valuesMode {
		group.Notes = append(group.Notes, predecessorHashesExcludedNote)
	}

	return &WhyExplanation{
		RunID:        runID,
		JobID:        jobID,
		TaskID:       taskID,
		TaskName:     taskName,
		Status:       string(groupStatusFromInstances(rows)),
		CacheEnabled: cacheEnabled,
		Verdict:      classifyGroupVerdict(cacheEnabled, allTerminal, allCached),
		// A group has N identity hashes and N baselines; only a --partition
		// selection can name one. Say so rather than diffing an arbitrary sibling.
		Baseline: WhyBaseline{Kind: "per_partition"},
		Group:    group,
	}
}

// classifyGroupVerdict folds the instance verdicts into one group verdict. A
// partially-cached group is a MISS: the group as a whole did re-execute work,
// and the exact split is visible in Group.CacheHits / Group.StatusCounts.
func classifyGroupVerdict(cacheEnabled, allTerminal, allCached bool) WhyVerdict {
	switch {
	case !allTerminal:
		return VerdictUnknown
	case !cacheEnabled:
		return VerdictCacheOff
	case allCached:
		return VerdictCacheHit
	default:
		return VerdictCacheMiss
	}
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
//
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
			// Scope the origin lookup to the SAME partition value: a fanned group
			// has N rows per (run, task) in the origin run too, and the cache
			// identity that produced this hit is per-instance. An unscoped
			// .First() would prove the hit against a sibling's inputs.
			var origin models.TaskRun
			err := db.Where("job_run_id = ? AND task_id = ? AND COALESCE(partition_value, '') = ?",
				*subject.CacheOriginRunID, subject.TaskID, subject.PartitionValue).
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

	// MISS / OFF: most-recent earlier run of the same task with a blob. The
	// partition predicate keeps a fanned instance diffed against the SAME
	// partition of the prior run — comparing partition "a" against partition "c"
	// would report every per-partition input as a discriminating change.
	var prior models.TaskRun
	err := db.
		Joins("JOIN job_runs ON job_runs.id = task_runs.job_run_id").
		Where("task_runs.task_id = ? AND job_runs.job_id = ? AND task_runs.job_run_id <> ? AND job_runs.started_at < ? AND task_runs.hash_input_blob IS NOT NULL AND COALESCE(task_runs.partition_value, '') = ?",
			subject.TaskID, jobID, subject.JobRunID, subjectStartedAt, subject.PartitionValue).
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
	if exp.Group != nil {
		return withNotes(exp, summarizeGroup(exp))
	}
	return withNotes(exp, summarizeVerdict(exp))
}

// withNotes appends the explanation's key-construction qualifiers to a summary
// line. Today that is the chain: values exclusion, which spec §4.3 requires
// `caesium why` to render explicitly — a consumer that stayed cached while its
// predecessor's identity moved is otherwise an unexplainable skip.
//
// It reads BOTH channels because the two answer shapes carry the note
// differently: a single instance has a Diff, a fanned group has N hashes and no
// Diff at all, so its note rides on WhyGroup.Notes. Both land in this one line,
// which is what every renderer — the CLI table, `--json`, and the Console's
// server-summary panel — shows first.
func withNotes(exp *WhyExplanation, summary string) string {
	notes := explanationNotes(exp)
	if len(notes) == 0 {
		return summary
	}
	return summary + "; " + strings.Join(notes, "; ")
}

// explanationNotes collects the key-construction notes from whichever channel
// this explanation shape uses, de-duplicated so a future explanation carrying
// both does not repeat itself.
func explanationNotes(exp *WhyExplanation) []string {
	var notes []string
	seen := make(map[string]struct{}, 2)
	add := func(candidates []string) {
		for _, n := range candidates {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			notes = append(notes, n)
		}
	}
	if exp.Diff != nil {
		add(exp.Diff.Notes)
	}
	if exp.Group != nil {
		add(exp.Group.Notes)
	}
	return notes
}

func summarizeVerdict(exp *WhyExplanation) string {
	switch exp.Verdict {
	case VerdictCacheHit:
		if exp.Diff != nil && exp.Diff.HashEqual {
			return fmt.Sprintf("CACHE HIT — every hashed input identical to the cached run; the prior result was reused (task %q did not execute)", exp.TaskName)
		}
		return fmt.Sprintf("CACHE HIT — task %q reused a cached result", exp.TaskName)
	case VerdictCacheMiss:
		return summarizeChanged(exp, "CACHE MISS", "re-ran")
	case VerdictCacheOff:
		if exp.Diff != nil && len(discriminatingChanges(exp.Diff.Changes)) > 0 {
			return summarizeChanged(exp, "CACHE DISABLED", "ran")
		}
		return fmt.Sprintf("CACHE DISABLED — caching was not enabled for task %q, so it ran unconditionally", exp.TaskName)
	default:
		return fmt.Sprintf("task %q is %s — no cache verdict yet", exp.TaskName, exp.Status)
	}
}

// summarizeGroup renders the one-line answer for a fanned step: how many
// partitions ran, the status histogram, the first failure's cause, and the
// selector that gets the per-instance explanation.
func summarizeGroup(exp *WhyExplanation) string {
	g := exp.Group
	statuses := make([]string, 0, len(g.StatusCounts))
	for status := range g.StatusCounts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, fmt.Sprintf("%d %s", g.StatusCounts[status], status))
	}

	out := fmt.Sprintf("FANNED GROUP — task %q ran %d partition(s): %s",
		exp.TaskName, g.PartitionCount, strings.Join(parts, ", "))
	if g.FirstFailure != nil {
		cause := g.FirstFailure.Error
		if cause == "" {
			cause = g.FirstFailure.Status
		}
		out += fmt.Sprintf("; first failure partition %q: %s", g.FirstFailure.Partition, cause)
	}
	return out + ". Re-run with --partition <value> for the per-instance explanation."
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
	// Excluded entries explain how the key was built; they are not fields that
	// differed, so they must not be counted or promoted to the headline.
	changes := discriminatingChanges(exp.Diff.Changes)
	if len(changes) == 0 {
		return fmt.Sprintf("%s — task %q %s; no input field differs from the prior run (cause is outside the persisted hash inputs, e.g. an expired/pruned cache entry)", verdict, exp.TaskName, ranVerb)
	}

	head := changes[0]
	detail := describeChange(head)
	if len(changes) > 1 {
		detail = fmt.Sprintf("%s (and %d other field(s))", detail, len(changes)-1)
	}
	return fmt.Sprintf("%s — %s", verdict, detail)
}

// describeChange renders a single FieldChange as a human phrase. Redacted env
// values are labeled rather than printed as if literal.
func describeChange(c FieldChange) string {
	switch {
	case c.Kind == fieldExcluded:
		return fmt.Sprintf("`%s` %s", c.Field, c.Note)
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

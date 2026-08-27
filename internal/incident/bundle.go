package incident

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Bundle is the JSON triage document an agent fetches once at session startup
// (GET /v1/agent/incidents/:id/bundle). Env injection cannot carry it — log
// tails alone exceed the ~128 KiB per-variable limit — so it is served over the
// scoped tool surface. It bundles everything the agent needs to plan within
// policy: the incident + classification, the failing task's error/scrubbed
// log/violations, the job + DAG, recent run history, the FROZEN lineage-impact
// allowlist, and the effective playbook. All attacker-influenced free text
// (the log tail) is scrubbed before it enters the bundle.
type Bundle struct {
	Incident       BundleIncident   `json:"incident"`
	Classification BundleClass      `json:"classification"`
	Failure        BundleFailure    `json:"failure"`
	Job            BundleJob        `json:"job"`
	RunHistory     []BundleRun      `json:"run_history"`
	LineageImpact  BundleImpact     `json:"lineage_impact"`
	Playbook       datatypes.JSON   `json:"playbook,omitempty"`
	Notes          []BundleNoteHint `json:"notes,omitempty"`
	GeneratedAt    time.Time        `json:"generated_at"`
}

// BundleIncident is the incident header the agent triages.
type BundleIncident struct {
	ID              uuid.UUID  `json:"id"`
	JobID           uuid.UUID  `json:"job_id"`
	RunID           *uuid.UUID `json:"run_id,omitempty"`
	TaskName        string     `json:"task_name,omitempty"`
	Status          string     `json:"status"`
	OccurrenceCount int        `json:"occurrence_count"`
	Attempt         int        `json:"attempt"`
	OpenedAt        time.Time  `json:"opened_at"`
}

// BundleClass carries the deterministic classification + its evidence.
type BundleClass struct {
	Class    string         `json:"class"`
	Evidence datatypes.JSON `json:"evidence,omitempty"`
}

// BundleFailure carries the failing task's diagnostic signals. LogTail is
// scrubbed; SchemaViolations and ExitCode come straight from the TaskRun.
//
// For a FANNED step the primary fields describe the first failed instance (the
// one the incident was classified from) and Partitions enumerates every failed
// sibling — an agent triaging "the extract step failed" must be able to see that
// 3 of 12 partitions failed and which ones, not one arbitrary row.
type BundleFailure struct {
	Error            string         `json:"error,omitempty"`
	LogTail          string         `json:"log_tail,omitempty"`
	LogTailScrubbed  bool           `json:"log_tail_scrubbed"`
	SchemaViolations datatypes.JSON `json:"schema_violations,omitempty"`
	ExitCode         *int           `json:"exit_code,omitempty"`
	Image            string         `json:"image,omitempty"`
	Result           string         `json:"result,omitempty"`
	// Partition is the fan-out instance the fields above describe. Empty for an
	// unfanned task.
	Partition string `json:"partition,omitempty"`
	// PartitionCount is the size of the fan-out group, 0 when unfanned.
	PartitionCount int `json:"partition_count,omitempty"`
	// Partitions lists every FAILED instance of the group (capped at
	// bundleFailedPartitionCap). Nil for an unfanned task.
	Partitions []BundleFailedPartition `json:"failed_partitions,omitempty"`
	// PartitionsTruncated reports that Partitions was capped.
	PartitionsTruncated bool `json:"failed_partitions_truncated,omitempty"`
}

// BundleFailedPartition is one failed fan-out instance. It carries the error and
// exit code but NOT a log tail: N scrubbed log tails would blow the bundle far
// past the size an agent session can hold, and the primary LogTail above already
// carries the representative failure. The task_run_id is the handle for fetching
// any specific instance's log from the logs endpoint.
type BundleFailedPartition struct {
	Partition string    `json:"partition"`
	Index     int       `json:"partition_index"`
	TaskRunID uuid.UUID `json:"task_run_id"`
	Attempt   int       `json:"attempt"`
	Error     string    `json:"error,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	Result    string    `json:"result,omitempty"`
}

// bundleFailedPartitionCap bounds the failed-instance list in a bundle.
const bundleFailedPartitionCap = 50

// BundleJob is the job definition + DAG topology.
type BundleJob struct {
	Alias            string          `json:"alias"`
	Paused           bool            `json:"paused"`
	SchemaValidation string          `json:"schema_validation,omitempty"`
	Tasks            []BundleTask    `json:"tasks"`
	Edges            []BundleDAGEdge `json:"edges"`
}

// BundleTask is one step in the DAG.
type BundleTask struct {
	Name        string `json:"name"`
	TriggerRule string `json:"trigger_rule,omitempty"`
	Retries     int    `json:"retries"`
	ReplaySafe  bool   `json:"replay_safe"`
}

// BundleDAGEdge is one from→to dependency edge (by task name).
type BundleDAGEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// BundleRun is one recent run with its duration for history-based reasoning
// (e.g. "this vendor has been late 4 of the last 30 days").
type BundleRun struct {
	ID          uuid.UUID  `json:"id"`
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	DurationMS  int64      `json:"duration_ms,omitempty"`
}

// BundleImpact is the FROZEN lineage-impact snapshot: the static job allowlist
// the incident manager computed at open (excluding the failing run's own
// outputs). The agent reads this instead of the live /v1/lineage/impact route,
// from which its scoped token is 403'd.
type BundleImpact struct {
	AllowedJobs []string `json:"allowed_jobs"`
	Frozen      bool     `json:"frozen"`
}

// BundleNoteHint surfaces prior timeline notes so a resumed session has context.
type BundleNoteHint struct {
	CreatedAt time.Time `json:"created_at"`
	Text      string    `json:"text"`
}

const bundleRunHistoryLimit = 20

// BuildBundle assembles the triage bundle for an incident. profile is the
// effective agent profile (may be nil), whose Playbook is surfaced so the agent
// plans within policy; when nil the Playbook is omitted. The failing task's log
// tail is scrubbed by the A5 scrubber before it enters the bundle.
func BuildBundle(ctx context.Context, db *gorm.DB, incidentID uuid.UUID, profile *models.AgentProfile) (*Bundle, error) {
	var inc models.Incident
	if err := db.WithContext(ctx).First(&inc, "id = ?", incidentID).Error; err != nil {
		return nil, err
	}

	b := &Bundle{
		Incident: BundleIncident{
			ID:              inc.ID,
			JobID:           inc.JobID,
			RunID:           inc.RunID,
			TaskName:        inc.TaskName,
			Status:          string(inc.Status),
			OccurrenceCount: inc.OccurrenceCount,
			Attempt:         inc.Attempt,
			OpenedAt:        inc.OpenedAt,
		},
		Classification: BundleClass{Class: inc.Class, Evidence: inc.Evidence},
		Failure:        BundleFailure{Error: inc.LastError},
		LineageImpact: BundleImpact{
			AllowedJobs: unmarshalAllowlist(inc.AllowedJobs),
			Frozen:      true,
		},
		GeneratedAt: time.Now().UTC(),
	}
	if profile != nil {
		b.Playbook = profile.Playbook
	}

	// Failing task detail (scrubbed log tail).
	//
	// A fanned step has N task_runs rows for one (run, task): `.First()` on that
	// predicate could hand the agent a SUCCEEDED instance's empty log as "the
	// failure". Load the whole group in partition order, describe the first
	// FAILED instance, and enumerate the rest of the failed siblings.
	if inc.RunID != nil && inc.TaskID != nil {
		var rows []models.TaskRun
		if err := db.WithContext(ctx).
			Where("job_run_id = ? AND task_id = ?", *inc.RunID, *inc.TaskID).
			Order("partition_index ASC, created_at ASC, id ASC").
			Find(&rows).Error; err == nil && len(rows) > 0 {
			tr := bundleAttributionRow(rows)
			// The resolved secret env of the run is not persisted (secrets are
			// never stored), so exact secret-value removal is not available
			// post-hoc; the high-entropy token heuristic still strips
			// credential-shaped tokens from the free text. This is the honest
			// degradation of the A5 scrubber on the bundle read path.
			scrubber := NewScrubber(nil)
			b.Failure.LogTail = scrubber.Scrub(tr.LogText)
			b.Failure.LogTailScrubbed = true
			b.Failure.SchemaViolations = tr.SchemaViolations
			b.Failure.ExitCode = tr.ExitCode
			b.Failure.Image = tr.Image
			b.Failure.Result = tr.Result
			b.Failure.Partition = tr.PartitionValue
			if tr.Error != "" {
				b.Failure.Error = tr.Error
			}
			if isFannedGroup(rows) {
				b.Failure.PartitionCount = len(rows)
				b.Failure.Partitions, b.Failure.PartitionsTruncated = failedPartitionEntries(rows)
			}
		}
	}

	// Job + DAG topology.
	job, tasks, edges, err := loadJobTopology(ctx, db, inc.JobID)
	if err != nil {
		return nil, err
	}
	b.Job = BundleJob{
		Alias:            job.Alias,
		Paused:           job.Paused,
		SchemaValidation: job.SchemaValidation,
		Tasks:            tasks,
		Edges:            edges,
	}

	// Recent run history + durations.
	b.RunHistory, err = loadRunHistory(ctx, db, inc.JobID)
	if err != nil {
		return nil, err
	}

	// Prior timeline notes (for resumed sessions).
	b.Notes = loadNoteHints(ctx, db, inc.ID)

	return b, nil
}

// bundleAttributionRow picks the instance the bundle describes: the first FAILED
// row in partition order, falling back to the first row when nothing failed (the
// incident may be run-level, or the group may still be settling).
func bundleAttributionRow(rows []models.TaskRun) *models.TaskRun {
	for i := range rows {
		if rows[i].Status == taskStatusFailed {
			return &rows[i]
		}
	}
	return &rows[0]
}

// isFannedGroup reports whether the rows describe a fan-out group. A
// single-instance group still carries a partition value and is still fanned.
func isFannedGroup(rows []models.TaskRun) bool {
	if len(rows) > 1 {
		return true
	}
	return len(rows) == 1 && rows[0].PartitionValue != ""
}

// failedPartitionEntries renders every failed instance of a fanned group, capped
// at bundleFailedPartitionCap. The bool reports truncation.
func failedPartitionEntries(rows []models.TaskRun) ([]BundleFailedPartition, bool) {
	out := make([]BundleFailedPartition, 0, len(rows))
	total := 0
	for i := range rows {
		if rows[i].Status != taskStatusFailed {
			continue
		}
		total++
		if len(out) >= bundleFailedPartitionCap {
			continue
		}
		out = append(out, BundleFailedPartition{
			Partition: rows[i].PartitionValue,
			Index:     rows[i].PartitionIndex,
			TaskRunID: rows[i].ID,
			Attempt:   rows[i].Attempt,
			Error:     rows[i].Error,
			ExitCode:  rows[i].ExitCode,
			Result:    rows[i].Result,
		})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, total > len(out)
}

func loadJobTopology(ctx context.Context, db *gorm.DB, jobID uuid.UUID) (models.Job, []BundleTask, []BundleDAGEdge, error) {
	var job models.Job
	if err := db.WithContext(ctx).First(&job, "id = ?", jobID).Error; err != nil {
		return job, nil, nil, err
	}

	var taskRows []models.Task
	if err := db.WithContext(ctx).
		Where("job_id = ?", jobID).
		Order("position ASC").
		Find(&taskRows).Error; err != nil {
		return job, nil, nil, err
	}
	nameByID := make(map[uuid.UUID]string, len(taskRows))
	tasks := make([]BundleTask, 0, len(taskRows))
	for i := range taskRows {
		t := taskRows[i]
		nameByID[t.ID] = t.Name
		tasks = append(tasks, BundleTask{
			Name:        t.Name,
			TriggerRule: t.TriggerRule,
			Retries:     t.Retries,
			ReplaySafe:  t.ReplaySafe,
		})
	}

	var edgeRows []models.TaskEdge
	if err := db.WithContext(ctx).Where("job_id = ?", jobID).Find(&edgeRows).Error; err != nil {
		return job, nil, nil, err
	}
	edges := make([]BundleDAGEdge, 0, len(edgeRows))
	for i := range edgeRows {
		e := edgeRows[i]
		// Skip an edge whose endpoint task is missing (e.g. soft-deleted) so a
		// dangling reference can't produce an edge with an empty task name.
		from, fromOK := nameByID[e.FromTaskID]
		to, toOK := nameByID[e.ToTaskID]
		if !fromOK || !toOK {
			continue
		}
		edges = append(edges, BundleDAGEdge{From: from, To: to})
	}

	return job, tasks, edges, nil
}

func loadRunHistory(ctx context.Context, db *gorm.DB, jobID uuid.UUID) ([]BundleRun, error) {
	var runs []models.JobRun
	if err := db.WithContext(ctx).
		Where("job_id = ?", jobID).
		Order("created_at DESC").
		Limit(bundleRunHistoryLimit).
		Find(&runs).Error; err != nil {
		return nil, err
	}
	out := make([]BundleRun, 0, len(runs))
	for i := range runs {
		r := runs[i]
		br := BundleRun{
			ID:          r.ID,
			Status:      r.Status,
			Error:       r.Error,
			StartedAt:   r.StartedAt,
			CompletedAt: r.CompletedAt,
		}
		if r.CompletedAt != nil && !r.StartedAt.IsZero() {
			br.DurationMS = r.CompletedAt.Sub(r.StartedAt).Milliseconds()
		}
		out = append(out, br)
	}
	return out, nil
}

// loadNoteHints returns prior agent notes recorded on the incident timeline
// (AgentAction rows of type "note"), best-effort.
func loadNoteHints(ctx context.Context, db *gorm.DB, incidentID uuid.UUID) []BundleNoteHint {
	var actions []models.AgentAction
	if err := db.WithContext(ctx).
		Where("incident_id = ? AND type = ?", incidentID, AgentActionTypeNote).
		Order("created_at ASC").
		Find(&actions).Error; err != nil {
		return nil
	}
	hints := make([]BundleNoteHint, 0, len(actions))
	for i := range actions {
		hints = append(hints, BundleNoteHint{
			CreatedAt: actions[i].CreatedAt,
			Text:      noteTextFromParams(actions[i].Params),
		})
	}
	return hints
}

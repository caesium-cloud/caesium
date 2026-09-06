package freshness

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// taskRunTerminalSucceeded is the TaskRun.Status value for a terminal success.
// Duplicated here (rather than importing internal/run) so the freshness package
// stays free of a run-store dependency — run wires this subscriber, not the
// reverse.
const taskRunTerminalSucceeded = "succeeded"

// Capturer hooks the run lifecycle path (NOT a poll): it subscribes to the run
// lifecycle events and, on run_completed, for each producing step's non-cached
// success, advances the dataset it declares — calling Store.Advance with the
// emitted watermark value (or refreshing verified_at in degraded mode when the
// step declares no watermark key or emits none). It also snapshots each produced
// dataset's consumed-input watermarks so "is my output up to date with my
// inputs" is a pure row comparison.
//
// That consumed snapshot is taken at the run's START, not its completion, so an
// input that advances mid-run is not credited to a run that never saw it. See
// consumedForRun for the three sources, strongest first.
//
// It reads the declared registry (dataset_declarations, freshness A2) to know
// which output key is a watermark, and the run's task_runs for the emitted
// ##caesium::output values. Backfill runs never advance (the monotonic guard is
// enforced in Store.Advance).
//
// Wiring belongs to the freshness evaluator bootstrap (Stream C, cmd/start),
// gated by CAESIUM_FRESHNESS_ENABLED; the Capturer itself is inert until Start
// is called.
type Capturer struct {
	bus       event.Bus
	db        *gorm.DB
	store     *Store
	namespace *string // v1: always nil (dataset identity keys on name)

	// mu guards startSnapshots.
	mu sync.Mutex
	// startSnapshots holds each in-flight run's consumed-input watermark view as
	// observed at run_started, so a completion records the inputs the run
	// actually consumed rather than a mid-run advance it never saw.
	//
	// Deliberately in-memory and bounded three ways: every terminal run event
	// (completed / failed / cancelled) evicts its entry, entries older than
	// startSnapshotTTL are swept on insert, and the map is hard-capped at
	// maxStartSnapshots (oldest evicted first). A snapshot lost to a restart or
	// a cap eviction degrades to the old completion-time read, never to a wrong
	// or missing dataset row — which is why this needs no persisted table.
	startSnapshots map[uuid.UUID]runStartSnapshot
}

// runStartSnapshot is one run's start-time consumed-input view.
type runStartSnapshot struct {
	consumed map[string]string
	takenAt  time.Time
}

const (
	// maxStartSnapshots caps the in-flight snapshot map. Each entry is a small
	// map of this job's declared inputs, so the ceiling is a few MB even when
	// every slot is taken by a wide job.
	maxStartSnapshots = 4096
	// startSnapshotTTL bounds a snapshot whose terminal event never arrived
	// (a lost event, a run outliving a leader change). Swept lazily on insert.
	startSnapshotTTL = 24 * time.Hour
)

// NewCapturer constructs a Capturer over the event bus and DB connection.
func NewCapturer(bus event.Bus, db *gorm.DB) *Capturer {
	return &Capturer{
		bus:            bus,
		db:             db,
		store:          NewStore(db),
		startSnapshots: make(map[uuid.UUID]runStartSnapshot),
	}
}

// Start subscribes to the run lifecycle and drives watermark capture until the
// context is cancelled. It mirrors the lineage subscriber's lifecycle shape.
func (c *Capturer) Start(ctx context.Context) error {
	return c.StartWithReady(ctx, nil)
}

// StartWithReady is Start with a readiness signal for deterministic tests.
//
// run_started is subscribed alongside the terminal events so the consumed-input
// snapshot is taken when the run BEGINS (the view it actually consumed) rather
// than when it ends. run_failed / run_cancelled carry no capture of their own —
// they exist only to evict that run's snapshot.
func (c *Capturer) StartWithReady(ctx context.Context, ready chan<- struct{}) error {
	ch, err := c.bus.Subscribe(ctx, event.Filter{Types: []event.Type{
		event.TypeRunStarted,
		event.TypeRunCompleted,
		event.TypeRunFailed,
		event.TypeRunCancelled,
	}})
	if err != nil {
		return err
	}
	if ready != nil {
		close(ready)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case evt, ok := <-ch:
			if !ok {
				return nil
			}
			switch evt.Type {
			case event.TypeRunStarted:
				c.handleRunStarted(ctx, evt)
			case event.TypeRunCompleted:
				c.handleRunCompleted(ctx, evt)
			case event.TypeRunFailed, event.TypeRunCancelled:
				c.dropStartSnapshot(evt.RunID)
			}
		}
	}
}

// handleRunStarted records the run's consumed-input watermarks as of its START.
// It is a no-op for a job that neither produces nor consumes a declared dataset,
// so the common case costs exactly one indexed read of dataset_declarations.
func (c *Capturer) handleRunStarted(ctx context.Context, evt event.Event) {
	if evt.RunID == uuid.Nil || evt.JobID == uuid.Nil {
		return
	}

	var decls []models.DatasetDeclaration
	if err := c.db.WithContext(ctx).Where("job_id = ?", evt.JobID).Find(&decls).Error; err != nil {
		log.Error("freshness: capture failed to load declarations at run start",
			"job_id", evt.JobID, "run_id", evt.RunID, "error", err)
		return
	}

	produces := false
	consumedNames := make([]string, 0, len(decls))
	for i := range decls {
		switch decls[i].Direction {
		case models.DatasetDirectionProduces:
			produces = true
		case models.DatasetDirectionConsumes:
			consumedNames = append(consumedNames, decls[i].Name)
		}
	}
	// Nothing produced means the completion handler returns before it ever wants
	// a snapshot; nothing consumed means there is no input view to freeze.
	if !produces || len(consumedNames) == 0 {
		return
	}

	c.putStartSnapshot(evt.RunID, c.consumedSnapshot(ctx, consumedNames))
}

// putStartSnapshot stores one run's start-time view, sweeping expired entries
// and enforcing the hard cap (oldest first) so the map cannot grow unbounded
// when a terminal event is lost.
func (c *Capturer) putStartSnapshot(runID uuid.UUID, consumed map[string]string) {
	now := time.Now().UTC()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.startSnapshots == nil {
		c.startSnapshots = make(map[uuid.UUID]runStartSnapshot)
	}

	for id, snap := range c.startSnapshots {
		if now.Sub(snap.takenAt) > startSnapshotTTL {
			delete(c.startSnapshots, id)
		}
	}
	for len(c.startSnapshots) >= maxStartSnapshots {
		oldestID, oldestAt := uuid.Nil, time.Time{}
		for id, snap := range c.startSnapshots {
			if oldestAt.IsZero() || snap.takenAt.Before(oldestAt) {
				oldestID, oldestAt = id, snap.takenAt
			}
		}
		if oldestID == uuid.Nil {
			break
		}
		delete(c.startSnapshots, oldestID)
	}

	c.startSnapshots[runID] = runStartSnapshot{consumed: consumed, takenAt: now}
}

// takeStartSnapshot removes and returns a run's start-time view. The second
// result distinguishes "snapshot taken, no input had a watermark yet" (an
// authoritative empty view) from "no snapshot for this run".
func (c *Capturer) takeStartSnapshot(runID uuid.UUID) (map[string]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap, ok := c.startSnapshots[runID]
	if !ok {
		return nil, false
	}
	delete(c.startSnapshots, runID)
	return snap.consumed, true
}

// dropStartSnapshot evicts a non-completing run's snapshot.
func (c *Capturer) dropStartSnapshot(runID uuid.UUID) {
	if runID == uuid.Nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.startSnapshots, runID)
}

// handleRunCompleted is the per-event capture. run_completed fires only for a
// succeeded run, so every producing step observed here completed the run
// successfully; per-step cache-hit and success are still checked so a cached
// step never re-advances a watermark.
func (c *Capturer) handleRunCompleted(ctx context.Context, evt event.Event) {
	if evt.RunID == uuid.Nil || evt.JobID == uuid.Nil {
		return
	}

	// Take (and evict) the start-time view up front so a run that bails out of
	// this handler for any reason below still releases its map slot.
	startConsumed, hasStartConsumed := c.takeStartSnapshot(evt.RunID)

	info, err := c.runInfo(ctx, evt.RunID)
	if err != nil {
		log.Error("freshness: capture failed to read run timing", "run_id", evt.RunID, "error", err)
		return
	}
	backfill, completedAt := info.backfill, info.completedAt

	var decls []models.DatasetDeclaration
	if err := c.db.WithContext(ctx).Where("job_id = ?", evt.JobID).Find(&decls).Error; err != nil {
		log.Error("freshness: capture failed to load declarations", "job_id", evt.JobID, "error", err)
		return
	}

	produced := make([]models.DatasetDeclaration, 0, len(decls))
	consumedNames := make([]string, 0, len(decls))
	for i := range decls {
		switch decls[i].Direction {
		case models.DatasetDirectionProduces:
			produced = append(produced, decls[i])
		case models.DatasetDirectionConsumes:
			consumedNames = append(consumedNames, decls[i].Name)
		}
	}
	if len(produced) == 0 {
		return
	}

	steps, err := c.stepOutputs(ctx, evt.RunID)
	if err != nil {
		log.Error("freshness: capture failed to load task outputs", "run_id", evt.RunID, "error", err)
		return
	}

	// Snapshot the consumed-input watermarks once for the whole run, taken as of
	// the run's START — the view it actually consumed. A completion-time read
	// would record an input that advanced mid-run and this run never saw, making
	// the freshness comparison over-report the output as caught-up.
	consumed := c.consumedForRun(ctx, info.params, startConsumed, hasStartConsumed, consumedNames)

	for i := range produced {
		p := &produced[i]
		step, ok := steps[p.StepName]
		if !ok || !step.succeededNonCached {
			// The producing step didn't run non-cached to success (cache hit,
			// skipped, or a different step): its state is already correct.
			continue
		}
		watermark := ""
		if p.WatermarkKey != "" {
			val, emitted := step.output[p.WatermarkKey]
			// A declared watermark key that is absent OR emitted as an empty
			// value means the run did not produce its required output. This is
			// NOT degraded mode (that is only for datasets with no declared
			// key) — refreshing verified_at here would mark a stale value fresh.
			// Leave state untouched and record the miss.
			if !emitted || strings.TrimSpace(val) == "" {
				log.Warn("freshness: producing step omitted its declared watermark output",
					"dataset", p.Name, "step", p.StepName, "watermark_key", p.WatermarkKey, "run_id", evt.RunID)
				continue
			}
			watermark = val
		}
		// Consumed rides the Advance transaction (AdvanceInput.Consumed) so the
		// snapshot is written atomically with — and tied to — the accepted
		// advance/verify. A separate follow-up write could let an overlapping
		// run's later snapshot land on top of another run's winning watermark;
		// folding it in means a run that loses the watermark race also does not
		// write its input snapshot.
		res, err := c.store.Advance(ctx, AdvanceInput{
			Namespace:   p.Namespace,
			Name:        p.Name,
			Watermark:   watermark,
			RunID:       evt.RunID,
			RunOrder:    completedAt,
			CompletedAt: completedAt,
			Backfill:    backfill,
			Consumed:    consumed,
		})
		if err != nil {
			log.Error("freshness: advance failed", "dataset", p.Name, "run_id", evt.RunID, "error", err)
			continue
		}
		switch res.Outcome {
		case OutcomeRegressionDropped, OutcomeOutOfOrderDropped:
			log.Warn("freshness: watermark write dropped",
				"dataset", p.Name, "outcome", string(res.Outcome), "run_id", evt.RunID, "watermark", watermark)
		case OutcomeAdvanced, OutcomeVerified:
			// Publish AFTER the Advance commits so the evaluator reacting to this
			// event reads post-advance state. RunID feeds the evaluator's
			// _trigger_depth propagation. Regression/out-of-order/backfill drops
			// leave state unchanged, so they must not wake a reactive derivation.
			c.publishDatasetAdvanced(p.Namespace, p.Name, evt.JobID, evt.RunID)
		}
	}
}

// publishDatasetAdvanced notifies the freshness evaluator that a dataset's
// watermark moved, so it can reactively re-derive downstream consumers off
// post-advance state. Payload carries the {namespace, name} dataset identity.
func (c *Capturer) publishDatasetAdvanced(namespace *string, name string, jobID, runID uuid.UUID) {
	if c.bus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"namespace": nsValue(namespace),
		"name":      name,
	})
	c.bus.Publish(event.Event{
		Type:      event.TypeDatasetAdvanced,
		JobID:     jobID,
		RunID:     runID,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	})
}

// capturedRun is the job_runs projection the completion path needs: whether the
// run is a backfill, its effective completion time, and its start params.
type capturedRun struct {
	backfill    bool
	completedAt time.Time
	params      map[string]string
}

// runInfo reports whether the run is a backfill, its effective completion time
// (falling back to started_at, then now), and the params it was started with.
func (c *Capturer) runInfo(ctx context.Context, runID uuid.UUID) (capturedRun, error) {
	var row struct {
		BackfillID  *uuid.UUID
		CompletedAt *time.Time
		StartedAt   time.Time
		Params      datatypes.JSON
	}
	if err := c.db.WithContext(ctx).Table("job_runs").
		Select("backfill_id", "completed_at", "started_at", "params").
		Where("id = ?", runID).Take(&row).Error; err != nil {
		return capturedRun{}, err
	}
	completedAt := time.Now().UTC()
	if row.CompletedAt != nil && !row.CompletedAt.IsZero() {
		completedAt = row.CompletedAt.UTC()
	} else if !row.StartedAt.IsZero() {
		completedAt = row.StartedAt.UTC()
	}
	return capturedRun{
		backfill:    row.BackfillID != nil,
		completedAt: completedAt,
		params:      decodeParamsJSON(row.Params),
	}, nil
}

// consumedForRun resolves the consumed-input watermark snapshot to record
// against this run's produced datasets, preferring views taken at the run's
// START in this order:
//
//  1. The run's own _consumed_watermarks param. A freshness-derived run carries
//     the evaluator's start-time view of exactly the inputs its derivation
//     decision was made on — durable (it is on the job_runs row) and therefore
//     the strongest signal, surviving a restart the in-memory map would not.
//  2. The run_started snapshot this Capturer took for the run.
//  3. A completion-time read, the legacy behaviour. Only reached when the run
//     is not freshness-derived AND the process missed its run_started event
//     (restart mid-run, leader change, cap eviction). Degraded, never wrong in
//     a new way — it is exactly what every run recorded before this fix.
func (c *Capturer) consumedForRun(
	ctx context.Context,
	params map[string]string,
	startConsumed map[string]string,
	hasStartConsumed bool,
	consumedNames []string,
) map[string]string {
	if raw, ok := params[freshnessConsumedWatermarksParam]; ok && strings.TrimSpace(raw) != "" {
		var derived map[string]string
		if err := json.Unmarshal([]byte(raw), &derived); err == nil {
			return derived
		}
		log.Warn("freshness: run carried an undecodable consumed-watermark param; falling back",
			"param", freshnessConsumedWatermarksParam)
	}
	if hasStartConsumed {
		return startConsumed
	}
	return c.consumedSnapshot(ctx, consumedNames)
}

type stepOutput struct {
	succeededNonCached bool
	output             map[string]string
}

// stepOutputs maps each step name in the run to its terminal non-cached success
// output (the ##caesium::output key/value pairs). A step present but only as a
// cache hit is recorded with succeededNonCached=false.
func (c *Capturer) stepOutputs(ctx context.Context, runID uuid.UUID) (map[string]stepOutput, error) {
	var rows []struct {
		TaskName string
		Status   string
		CacheHit bool
		Output   datatypes.JSON
	}
	if err := c.db.WithContext(ctx).Table("task_runs").
		Select("tasks.name as task_name, task_runs.status, task_runs.cache_hit, task_runs.output").
		Joins("join tasks on tasks.id = task_runs.task_id").
		Where("task_runs.job_run_id = ?", runID).
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := make(map[string]stepOutput, len(rows))
	for i := range rows {
		r := &rows[i]
		if r.Status != taskRunTerminalSucceeded {
			continue
		}
		// A non-cached success wins over any prior cache-hit row for the step.
		cur, seen := out[r.TaskName]
		if seen && cur.succeededNonCached {
			continue
		}
		so := stepOutput{succeededNonCached: !r.CacheHit}
		if !r.CacheHit {
			so.output = decodeOutput(r.Output)
		} else if !seen {
			so.output = decodeOutput(r.Output)
		}
		out[r.TaskName] = so
	}
	return out, nil
}

// consumedSnapshot reads the current watermark of every consumed dataset in a
// single query (no per-name N+1), keyed on the nil→” namespace mapping.
//
// It is a point-in-time read of whenever it is called: handleRunStarted calls it
// to freeze the run's input view, and consumedForRun calls it only as the
// degraded fallback for a run whose start was never observed.
func (c *Capturer) consumedSnapshot(ctx context.Context, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	// Dedupe before the IN query.
	seen := make(map[string]struct{}, len(names))
	uniq := make([]string, 0, len(names))
	for _, n := range names {
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		uniq = append(uniq, n)
	}

	var rows []models.DatasetState
	if err := c.db.WithContext(ctx).
		Where("namespace = ? AND name IN ?", nsValue(c.namespace), uniq).
		Find(&rows).Error; err != nil {
		log.Error("freshness: capture failed to read consumed state", "error", err)
		return nil
	}
	if len(rows) == 0 {
		return nil
	}
	snapshot := make(map[string]string, len(rows))
	for i := range rows {
		snapshot[rows[i].Name] = rows[i].Watermark
	}
	return snapshot
}

// decodeOutput parses a task run's ##caesium::output blob into string values.
// It decodes with json.Number (UseNumber) rather than json.Unmarshal so a large
// integer watermark (e.g. a nanosecond timestamp beyond float64's exact range)
// keeps its precise string form instead of being rounded through float64.
func decodeOutput(raw datatypes.JSON) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var typed map[string]interface{}
	if err := dec.Decode(&typed); err != nil {
		return nil
	}
	out := make(map[string]string, len(typed))
	for k, v := range typed {
		switch val := v.(type) {
		case string:
			out[k] = val
		case json.Number:
			out[k] = val.String()
		case bool:
			if val {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		}
	}
	return out
}

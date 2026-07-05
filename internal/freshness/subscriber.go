package freshness

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
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

// Capturer hooks the run-completion lifecycle path (NOT a poll): it subscribes
// to run_completed events and, for each producing step's non-cached success,
// advances the dataset it declares — calling Store.Advance with the emitted
// watermark value (or refreshing verified_at in degraded mode when the step
// declares no watermark key or emits none). It also snapshots each produced
// dataset's consumed-input watermarks so "is my output up to date with my
// inputs" is a pure row comparison.
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
}

// NewCapturer constructs a Capturer over the event bus and DB connection.
func NewCapturer(bus event.Bus, db *gorm.DB) *Capturer {
	return &Capturer{
		bus:   bus,
		db:    db,
		store: NewStore(db),
	}
}

// Start subscribes to run-completion events and drives watermark capture until
// the context is cancelled. It mirrors the lineage subscriber's lifecycle shape.
func (c *Capturer) Start(ctx context.Context) error {
	return c.StartWithReady(ctx, nil)
}

// StartWithReady is Start with a readiness signal for deterministic tests.
func (c *Capturer) StartWithReady(ctx context.Context, ready chan<- struct{}) error {
	ch, err := c.bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeRunCompleted}})
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
			c.handleRunCompleted(ctx, evt)
		}
	}
}

// handleRunCompleted is the per-event capture. run_completed fires only for a
// succeeded run, so every producing step observed here completed the run
// successfully; per-step cache-hit and success are still checked so a cached
// step never re-advances a watermark.
func (c *Capturer) handleRunCompleted(ctx context.Context, evt event.Event) {
	if evt.RunID == uuid.Nil || evt.JobID == uuid.Nil {
		return
	}

	backfill, completedAt, err := c.runTiming(ctx, evt.RunID)
	if err != nil {
		log.Error("freshness: capture failed to read run timing", "run_id", evt.RunID, "error", err)
		return
	}

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

	// Snapshot the consumed-input watermarks once for the whole run.
	//
	// KNOWN LIMITATION (v1, per plan B3 — "capture the consumed-watermark set at
	// run completion"): this reads each input's CURRENT watermark at completion
	// time, not the input view the run actually consumed at start. If an input
	// advances mid-run, the produced dataset records the newer input watermark
	// even though this run never saw it, which can make a freshness comparison
	// over-report the output as caught-up. This field is WRITE-ONLY today — its
	// only reader is the freshness evaluator (Stream C), which does not exist
	// yet. The correct input-view sourcing (snapshot input watermarks at
	// TypeRunStarted, keyed by run, and read them back here) belongs to the
	// evaluator stream, where the read semantics and the run-start seam live;
	// until then completion-time capture is the accepted v1 behavior. See
	// docs/exec-plans/active/freshness-scheduling.md (Stream C, C2 at-risk).
	consumed := c.consumedSnapshot(ctx, consumedNames)

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

// runTiming reports whether the run is a backfill and its effective completion
// time (falling back to started_at, then now).
func (c *Capturer) runTiming(ctx context.Context, runID uuid.UUID) (backfill bool, completedAt time.Time, err error) {
	var row struct {
		BackfillID  *uuid.UUID
		CompletedAt *time.Time
		StartedAt   time.Time
	}
	if err = c.db.WithContext(ctx).Table("job_runs").
		Select("backfill_id", "completed_at", "started_at").
		Where("id = ?", runID).Take(&row).Error; err != nil {
		return false, time.Time{}, err
	}
	completedAt = time.Now().UTC()
	if row.CompletedAt != nil && !row.CompletedAt.IsZero() {
		completedAt = row.CompletedAt.UTC()
	} else if !row.StartedAt.IsZero() {
		completedAt = row.StartedAt.UTC()
	}
	return row.BackfillID != nil, completedAt, nil
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
// This is a completion-time read: it reflects each input's watermark now, not
// as-of the producing run's start. See the KNOWN LIMITATION note at the call
// site — the input-view-at-consumption refinement is owned by the evaluator
// stream (Stream C), the field's only reader.
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

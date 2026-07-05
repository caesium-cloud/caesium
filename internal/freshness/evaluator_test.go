package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictest "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type fakeRunAdmitter struct {
	t       *testing.T
	db      *gorm.DB
	handled bool
	err     error
	calls   int
	runIDs  []uuid.UUID
}

func (f *fakeRunAdmitter) AdmitRun(jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (*runstorage.JobRun, bool, error) {
	f.calls++
	if f.err != nil {
		return nil, true, f.err
	}
	if !f.handled {
		return nil, false, nil
	}

	var startOpts runstorage.StartOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&startOpts)
		}
	}
	params, err := json.Marshal(startOpts.Params)
	if err != nil {
		f.t.Fatalf("marshal params: %v", err)
	}
	now := time.Now().UTC()
	runID := uuid.New()
	row := models.JobRun{
		ID:        runID,
		JobID:     jobID,
		Status:    string(runstorage.StatusRunning),
		Params:    datatypes.JSON(params),
		StartedAt: now,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if triggerID != nil {
		row.TriggerID = *triggerID
	}
	if err := f.db.Create(&row).Error; err != nil {
		f.t.Fatalf("create admitted run: %v", err)
	}
	f.runIDs = append(f.runIDs, runID)
	return &runstorage.JobRun{ID: runID, JobID: jobID, Status: runstorage.StatusRunning, Params: startOpts.Params}, true, nil
}

func TestEvaluatorLeaderGateSkipsNonLeader(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	jobID := seedFreshnessJob(t, db, "leader-gate")
	seedDeclarations(t, db,
		produceDecl(jobID, "out", "1h", ""),
	)

	admitter := &fakeRunAdmitter{t: t, db: db, handled: true}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              admitter,
		MaxDerivationsPerTick: 50,
		LeaderCheck: func(context.Context) (bool, error) {
			return false, nil
		},
		Now: func() time.Time { return t0.Add(2 * time.Hour) },
	})

	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if admitter.calls != 0 {
		t.Fatalf("non-leader admitted %d runs, want 0", admitter.calls)
	}
	var derivations int64
	if err := db.Model(&models.DatasetDerivation{}).Count(&derivations).Error; err != nil {
		t.Fatalf("count derivations: %v", err)
	}
	if derivations != 0 {
		t.Fatalf("non-leader wrote %d derivations, want 0", derivations)
	}
}

func TestEvaluatorStatusComputation(t *testing.T) {
	now := t0.Add(3 * time.Hour)

	cases := []struct {
		name          string
		freshness     string
		maxStaleness  string
		outputAt      time.Time
		consumedAtRun map[string]string
		inputs        map[string]string
		wantStatus    string
		wantDecision  string
		wantMetric    bool
	}{
		{
			name:         "fresh",
			freshness:    "1h",
			outputAt:     now.Add(-30 * time.Minute),
			wantStatus:   models.DatasetStatusFresh,
			wantDecision: models.DatasetDecisionSkippedFresh,
			wantMetric:   true,
		},
		{
			name:          "stale",
			freshness:     "1h",
			outputAt:      now.Add(-2 * time.Hour),
			consumedAtRun: map[string]string{"raw": "10"},
			inputs:        map[string]string{"raw": "11"},
			wantStatus:    models.DatasetStatusStale,
			wantDecision:  models.DatasetDecisionSkippedAdmission,
		},
		{
			name:          "stale-upstream",
			freshness:     "1h",
			outputAt:      now.Add(-2 * time.Hour),
			consumedAtRun: map[string]string{"raw": "10"},
			inputs:        map[string]string{"raw": "10"},
			wantStatus:    models.DatasetStatusStaleUpstream,
			wantDecision:  models.DatasetDecisionSkippedUpstream,
		},
		{
			name:         "violated",
			freshness:    "1h",
			maxStaleness: "2h",
			outputAt:     now.Add(-3 * time.Hour),
			wantStatus:   models.DatasetStatusViolated,
			wantDecision: models.DatasetDecisionSkippedAdmission,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openRegistryDB(t)
			ctx := context.Background()
			jobID := seedFreshnessJob(t, db, "status-"+tc.name)
			decls := []models.DatasetDeclaration{produceDecl(jobID, "out", tc.freshness, tc.maxStaleness)}
			if tc.consumedAtRun != nil || tc.inputs != nil {
				decls = append(decls, consumeDecl(jobID, "raw"))
			}
			seedDeclarations(t, db, decls...)
			seedState(t, db, "out", "100", tc.outputAt, tc.consumedAtRun)
			for name, watermark := range tc.inputs {
				seedState(t, db, name, watermark, tc.outputAt.Add(time.Hour), nil)
			}

			bus := event.New()
			events, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeFreshnessViolated, event.TypeSLAMissed}})
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}

			admitter := &fakeRunAdmitter{t: t, db: db, handled: false}
			beforeDecision := metrictest.CounterValue(t, metrics.DatasetDerivationsTotal, "out", tc.wantDecision)
			beforeViolation := metrictest.CounterValue(t, metrics.FreshnessViolationsTotal, "out", models.DatasetStatusViolated)
			eval := NewEvaluator(Config{
				DB:                    db,
				Bus:                   bus,
				RunStore:              admitter,
				MaxDerivationsPerTick: 50,
				Now:                   func() time.Time { return now },
			})
			if err := eval.EvaluateOnce(ctx); err != nil {
				t.Fatalf("evaluate: %v", err)
			}

			state, ok, err := NewStore(db).Get(ctx, nil, "out")
			if err != nil {
				t.Fatalf("get state: %v", err)
			}
			if !ok {
				t.Fatalf("state row missing")
			}
			if state.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (reason: %s)", state.Status, tc.wantStatus, state.Reason)
			}
			if got := metrictest.CounterValue(t, metrics.DatasetDerivationsTotal, "out", tc.wantDecision); got != beforeDecision+1 {
				t.Fatalf("derivation metric = %v, want %v", got, beforeDecision+1)
			}
			if tc.wantMetric {
				if got := metrictest.GaugeValue(t, metrics.DatasetStalenessSeconds.WithLabelValues("out")); got != 1800 {
					t.Fatalf("staleness gauge = %v, want 1800", got)
				}
			}
			if tc.wantStatus == models.DatasetStatusViolated {
				if got := metrictest.CounterValue(t, metrics.FreshnessViolationsTotal, "out", models.DatasetStatusViolated); got != beforeViolation+1 {
					t.Fatalf("violation metric = %v, want %v", got, beforeViolation+1)
				}
				requireEventType(t, events, event.TypeFreshnessViolated)
				requireEventType(t, events, event.TypeSLAMissed)
			}
		})
	}
}

func TestEvaluatorFanInDerivesOneRunAndDedupesActiveWatermarks(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "fanin")
	seedDeclarations(t, db,
		produceDecl(jobID, "out", "1h", ""),
		consumeDecl(jobID, "raw.a"),
		consumeDecl(jobID, "raw.b"),
		consumeDecl(jobID, "raw.c"),
	)
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), map[string]string{
		"raw.a": "1",
		"raw.b": "1",
		"raw.c": "1",
	})
	seedState(t, db, "raw.a", "2", now.Add(-time.Minute), nil)
	seedState(t, db, "raw.b", "2", now.Add(-time.Minute), nil)
	seedState(t, db, "raw.c", "2", now.Add(-time.Minute), nil)

	admitter := &fakeRunAdmitter{t: t, db: db, handled: true}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              admitter,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate 1: %v", err)
	}
	if admitter.calls != 1 {
		t.Fatalf("admit calls after first eval = %d, want 1", admitter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 1)

	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate 2: %v", err)
	}
	if admitter.calls != 1 {
		t.Fatalf("admit calls after dedupe eval = %d, want 1", admitter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedActiveRun, 1)
}

func TestEvaluatorAdmissionErrorsAreRecorded(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "admission")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	admitter := &fakeRunAdmitter{t: t, db: db, handled: true, err: runstorage.ErrRunQueued}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              admitter,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)

	admitter.err = errors.New("boom")
	if err := eval.EvaluateOnce(ctx); err == nil {
		t.Fatalf("expected non-admission error to propagate")
	}
}

// TestEvaluatorReactsToDatasetAdvancedPostState proves the reactive path keys
// off POST-advance state: a downstream consumer only derives once its upstream's
// watermark has actually advanced past the consumed snapshot. This is the race
// fix — dataset_advanced is published AFTER the capturer's Advance commits, so
// the evaluator never reads pre-advance state and derives a redundant run.
func TestEvaluatorReactsToDatasetAdvancedPostState(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)

	p1 := seedFreshnessJob(t, db, "stage-producer")
	p2 := seedFreshnessJob(t, db, "mart-producer")
	seedDeclarations(t, db,
		produceDecl(p1, "staging", "1h", ""),
		produceDecl(p2, "mart", "1h", ""),
		consumeDecl(p2, "staging"),
	)
	// mart is stale and last consumed staging="1". staging is currently fresh but
	// still at watermark "1" (pre-advance) — equal to mart's consumed snapshot.
	seedState(t, db, "mart", "500", now.Add(-2*time.Hour), map[string]string{"staging": "1"})
	seedState(t, db, "staging", "1", now.Add(-30*time.Minute), nil)

	admitter := &fakeRunAdmitter{t: t, db: db, handled: true}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              admitter,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})

	advanced := datasetAdvancedEvent(t, "", "staging", uuid.Nil)

	// Pre-advance: staging watermark "1" == mart's consumed "1", so mart is
	// stale-upstream (waiting) and must NOT derive a redundant producer run.
	if err := eval.EvaluateEvent(ctx, advanced); err != nil {
		t.Fatalf("evaluate pre-advance: %v", err)
	}
	if admitter.calls != 0 {
		t.Fatalf("pre-advance derived %d runs, want 0", admitter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedUpstream, 1)

	// The capturer's Advance commits before it publishes dataset_advanced. Move
	// staging past mart's consumed snapshot.
	if _, err := NewStore(db).Advance(ctx, AdvanceInput{
		Name: "staging", Watermark: "2", RunID: uuid.New(), CompletedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("advance staging: %v", err)
	}

	// Post-advance: mart now sees staging advanced past its consumed snapshot and
	// derives exactly one run.
	if err := eval.EvaluateEvent(ctx, advanced); err != nil {
		t.Fatalf("evaluate post-advance: %v", err)
	}
	if admitter.calls != 1 {
		t.Fatalf("post-advance derived %d runs, want 1", admitter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 1)
}

// TestArrivalAdvanceTriggersReactiveEvaluation proves the flagship external
// event -> derive downstream flow: an arrival advance publishes dataset_advanced,
// and feeding that event to the evaluator derives the downstream consumer
// without waiting for a timer tick.
func TestArrivalAdvanceTriggersReactiveEvaluation(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)

	seedArrivalSource(t, db, "raw.vendor_x", "s3:ObjectCreated", "$.key")
	p2 := seedFreshnessJob(t, db, "arrival-mart")
	seedDeclarations(t, db,
		produceDecl(p2, "mart", "1h", ""),
		consumeDecl(p2, "raw.vendor_x"),
	)
	// mart is stale and last consumed raw.vendor_x="v1".
	seedState(t, db, "mart", "500", now.Add(-2*time.Hour), map[string]string{"raw.vendor_x": "v1"})

	bus := event.New()
	events, err := bus.Subscribe(ctx, event.Filter{Types: []event.Type{event.TypeDatasetAdvanced}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	obs := NewArrivalObserver(db)
	obs.SetBus(bus)

	ingested := &models.IngestedEvent{
		ID:        uuid.New(),
		Type:      "s3:ObjectCreated",
		Data:      datatypes.JSON([]byte(`{"key":"v2"}`)),
		CreatedAt: now.Add(-time.Minute),
	}
	res, err := obs.Observe(ctx, ingested)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if len(res.Advances) != 1 || res.Advances[0].Outcome != OutcomeAdvanced {
		t.Fatalf("arrival advances = %+v, want one OutcomeAdvanced", res.Advances)
	}

	// The arrival advance must have published dataset_advanced for the source.
	var advanced event.Event
	select {
	case advanced = <-events:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for dataset_advanced from arrival")
	}
	if advanced.Type != event.TypeDatasetAdvanced {
		t.Fatalf("event type = %q, want %q", advanced.Type, event.TypeDatasetAdvanced)
	}
	id, ok := datasetIdentityFromPayload(advanced.Payload)
	if !ok || id.name != "raw.vendor_x" {
		t.Fatalf("dataset_advanced payload = %s (parsed=%+v ok=%v)", advanced.Payload, id, ok)
	}

	// Feeding the reactive event to the evaluator derives the downstream mart.
	admitter := &fakeRunAdmitter{t: t, db: db, handled: true}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              admitter,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateEvent(ctx, advanced); err != nil {
		t.Fatalf("evaluate reactive: %v", err)
	}
	if admitter.calls != 1 {
		t.Fatalf("reactive derived %d runs, want 1", admitter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 1)
}

func datasetAdvancedEvent(t *testing.T, namespace, name string, runID uuid.UUID) event.Event {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"namespace": namespace, "name": name})
	if err != nil {
		t.Fatalf("marshal dataset_advanced payload: %v", err)
	}
	return event.Event{Type: event.TypeDatasetAdvanced, RunID: runID, Payload: payload}
}

func seedArrivalSource(t *testing.T, db *gorm.DB, name, eventType, watermarkPath string) {
	t.Helper()
	binding, err := json.Marshal(map[string]any{
		"event":     map[string]any{"type": eventType},
		"watermark": watermarkPath,
	})
	if err != nil {
		t.Fatalf("marshal arrival binding: %v", err)
	}
	decl := models.DatasetDeclaration{
		ID:             uuid.New(),
		JobID:          uuid.New(),
		JobAlias:       "arrival-source",
		StepName:       "",
		Name:           name,
		Direction:      models.DatasetDirectionSource,
		External:       true,
		ExpectedEvery:  "24h",
		ArrivalBinding: datatypes.JSON(binding),
		CreatedAt:      t0,
		UpdatedAt:      t0,
	}
	if err := db.Create(&decl).Error; err != nil {
		t.Fatalf("create arrival source: %v", err)
	}
}

func seedFreshnessJob(t *testing.T, db *gorm.DB, alias string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	trigger := models.Trigger{
		ID:            triggerID,
		Alias:         alias + "-trigger",
		Type:          models.TriggerTypeCron,
		Configuration: `{"expression":"0 0 31 2 *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := db.Create(&trigger).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	jobID := uuid.New()
	job := models.Job{
		ID:        jobID,
		Alias:     alias,
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	return jobID
}

func seedDeclarations(t *testing.T, db *gorm.DB, decls ...models.DatasetDeclaration) {
	t.Helper()
	if len(decls) == 0 {
		return
	}
	if err := db.Create(&decls).Error; err != nil {
		t.Fatalf("create declarations: %v", err)
	}
}

func produceDecl(jobID uuid.UUID, name, freshness, maxStaleness string) models.DatasetDeclaration {
	now := t0
	return models.DatasetDeclaration{
		ID:           uuid.New(),
		JobID:        jobID,
		JobAlias:     "job-" + jobID.String(),
		StepName:     "produce",
		Name:         name,
		Direction:    models.DatasetDirectionProduces,
		Freshness:    freshness,
		MaxStaleness: maxStaleness,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func consumeDecl(jobID uuid.UUID, name string) models.DatasetDeclaration {
	now := t0
	return models.DatasetDeclaration{
		ID:        uuid.New(),
		JobID:     jobID,
		JobAlias:  "job-" + jobID.String(),
		StepName:  "produce",
		Name:      name,
		Direction: models.DatasetDirectionConsumes,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func seedState(t *testing.T, db *gorm.DB, name, watermark string, freshAt time.Time, consumed map[string]string) {
	t.Helper()
	blob, err := json.Marshal(consumed)
	if err != nil {
		t.Fatalf("marshal consumed: %v", err)
	}
	state := models.DatasetState{
		ID:                 uuid.New(),
		Namespace:          "",
		Name:               name,
		Watermark:          watermark,
		AdvancedAt:         &freshAt,
		Status:             models.DatasetStatusUnknown,
		ConsumedWatermarks: datatypes.JSON(blob),
		CreatedAt:          freshAt,
		UpdatedAt:          freshAt,
	}
	if err := db.Create(&state).Error; err != nil {
		t.Fatalf("create state %s: %v", name, err)
	}
}

func assertDerivationCount(t *testing.T, db *gorm.DB, decision string, want int64) {
	t.Helper()
	var count int64
	if err := db.Model(&models.DatasetDerivation{}).Where("decision = ?", decision).Count(&count).Error; err != nil {
		t.Fatalf("count derivations: %v", err)
	}
	if count != want {
		t.Fatalf("derivation count for %s = %d, want %d", decision, count, want)
	}
}

func requireEventType(t *testing.T, events <-chan event.Event, typ event.Type) {
	t.Helper()
	select {
	case evt := <-events:
		if evt.Type != typ {
			t.Fatalf("event type = %q, want %q", evt.Type, typ)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for event %q", typ)
	}
}

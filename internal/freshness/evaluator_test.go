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

type fakeRunStarter struct {
	t  *testing.T
	db *gorm.DB
	// decline makes StartWithContext return (nil, nil) — the defensive
	// "the store created nothing and said nothing" branch.
	decline bool
	// commitThenFail commits the run row and THEN returns err, reproducing a
	// start that failed after the run was already live (Store.startRun publishes
	// run_started and takes the lease before it reads the record back).
	commitThenFail bool
	// cancelOnStart, when set, cancels the caller's context from inside
	// StartWithContext — the shutdown-lands-mid-admission case.
	cancelOnStart context.CancelFunc
	err           error
	calls         int
	runIDs        []uuid.UUID
	launched      []uuid.UUID
}

func (f *fakeRunStarter) StartWithContext(_ context.Context, jobID uuid.UUID, triggerID *uuid.UUID, opts ...runstorage.StartOption) (*runstorage.JobRun, error) {
	f.calls++
	if f.err != nil && !f.commitThenFail {
		return nil, f.err
	}
	if f.decline {
		return nil, nil
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
		f.t.Fatalf("create started run: %v", err)
	}
	f.runIDs = append(f.runIDs, runID)
	if f.cancelOnStart != nil {
		f.cancelOnStart()
	}
	if f.commitThenFail {
		// The run is committed and live, but the caller only learns about the
		// failure — exactly what a post-commit read cancellation looks like.
		return nil, f.err
	}
	return &runstorage.JobRun{ID: runID, JobID: jobID, Status: runstorage.StatusRunning, Params: startOpts.Params}, nil
}

// launch stands in for the production RunLauncher (cmd/start's job.Run
// dispatch). Recording the run ids it receives is how these tests prove a
// derived run is actually handed to an executor rather than left admitted and
// stranded (issue #501).
func (f *fakeRunStarter) launch(_ context.Context, r *runstorage.JobRun) {
	if r == nil {
		f.t.Fatalf("run launcher received a nil run")
	}
	f.launched = append(f.launched, r.ID)
}

func TestEvaluatorLeaderGateSkipsNonLeader(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	jobID := seedFreshnessJob(t, db, "leader-gate")
	seedDeclarations(t, db,
		produceDecl(jobID, "out", "1h", ""),
	)

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		LeaderCheck: func(context.Context) (bool, error) {
			return false, nil
		},
		Now: func() time.Time { return t0.Add(2 * time.Hour) },
	})

	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("non-leader started %d runs, want 0", starter.calls)
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
			// A stale, upstream-ready output whose job is freshness-triggered
			// derives a run — with or without a concurrency policy (issue #501).
			wantDecision: models.DatasetDecisionDerived,
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
			wantDecision: models.DatasetDecisionDerived,
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

			starter := &fakeRunStarter{t: t, db: db}
			beforeDecision := metrictest.CounterValue(t, metrics.DatasetDerivationsTotal, "out", tc.wantDecision)
			beforeViolation := metrictest.CounterValue(t, metrics.FreshnessViolationsTotal, "out", models.DatasetStatusViolated)
			eval := NewEvaluator(Config{
				DB:                    db,
				Bus:                   bus,
				RunStore:              starter,
				LaunchRun:             starter.launch,
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

// TestEvaluatorDispatchesDerivedRun is issue #501's unit-level guard: a
// derivation must hand the run it created to an executor. Recording only a
// `derived` row (or admitting a run nothing dispatches) is what left a job with
// `metadata.concurrency` sitting at `running` with zero task rows.
func TestEvaluatorDispatchesDerivedRun(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "dispatch")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(starter.runIDs) != 1 {
		t.Fatalf("started runs = %v, want exactly 1", starter.runIDs)
	}
	if len(starter.launched) != 1 || starter.launched[0] != starter.runIDs[0] {
		t.Fatalf("launched runs = %v, want the derived run %v", starter.launched, starter.runIDs)
	}

	var derivation models.DatasetDerivation
	if err := db.Where("decision = ?", models.DatasetDecisionDerived).Take(&derivation).Error; err != nil {
		t.Fatalf("load derivation: %v", err)
	}
	if derivation.RunID == nil || *derivation.RunID != starter.runIDs[0] {
		t.Fatalf("derivation run id = %v, want %v", derivation.RunID, starter.runIDs[0])
	}
}

// TestEvaluatorDoesNotDerivePausedJob proves pause reaches the one scheduler
// path that creates AND executes a run without going through a trigger object.
// Cron, HTTP, event and webhook all check models.Job.Paused before job.Run, and
// the manual-run controller answers 409; neither run admission nor job.Run
// re-checks it, so before this guard a paused freshness job executed on every
// arrival.
func TestEvaluatorDoesNotDerivePausedJob(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "paused")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)
	if err := db.Model(&models.Job{}).Where("id = ?", jobID).Update("paused", true).Error; err != nil {
		t.Fatalf("pause job: %v", err)
	}

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("paused job started %d runs, want 0", starter.calls)
	}
	if len(starter.launched) != 0 {
		t.Fatalf("paused job launched %v runs, want none", starter.launched)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 0)
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)

	var derivation models.DatasetDerivation
	if err := db.Where("decision = ?", models.DatasetDecisionSkippedAdmission).Take(&derivation).Error; err != nil {
		t.Fatalf("load derivation: %v", err)
	}
	if derivation.Reason != "job is paused" {
		t.Fatalf("derivation reason = %q, want %q", derivation.Reason, "job is paused")
	}

	// Unpausing restores derivation on the very next evaluation.
	if err := db.Model(&models.Job{}).Where("id = ?", jobID).Update("paused", false).Error; err != nil {
		t.Fatalf("unpause job: %v", err)
	}
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate after unpause: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("start calls after unpause = %d, want 1", starter.calls)
	}
	if len(starter.launched) != 1 {
		t.Fatalf("launched runs after unpause = %v, want 1", starter.launched)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 1)
}

// TestEvaluatorRefusesToDeriveWithoutLauncher proves the evaluator fails closed:
// with no executor wired it must not create a run it cannot dispatch, because a
// stranded `running` row with no tasks is worse than an explicit skip.
func TestEvaluatorRefusesToDeriveWithoutLauncher(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "no-launcher")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("started %d runs without a launcher, want 0", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 0)
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)
}

// TestEvaluatorAdoptsRunCommittedByAFailedStart is the regression for a run
// stranded by a start that failed AFTER committing. Store.startRun publishes
// run_started and takes the run lease before it reads the record back, so a
// cancellation (server shutdown mid-tick) or any post-commit read failure hands
// the evaluator an error for a run that is already live. Returning that error
// would leave a `running` row with no tasks and no engine: skipped_active_run
// forever, and a maxRuns policy occupied for good.
func TestEvaluatorAdoptsRunCommittedByAFailedStart(t *testing.T) {
	db := openRegistryDB(t)
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "post-commit")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	// The tick context dies from inside the admission call, after the run row is
	// committed — the shutdown-mid-admission case. Recovery must not depend on
	// that context.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	starter := &fakeRunStarter{
		t:              t,
		db:             db,
		commitThenFail: true,
		err:            context.Canceled,
		cancelOnStart:  cancel,
	}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})

	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("the fault injection must actually have cancelled the tick context")
	}

	if len(starter.runIDs) != 1 {
		t.Fatalf("committed runs = %v, want exactly 1", starter.runIDs)
	}
	if len(starter.launched) != 1 || starter.launched[0] != starter.runIDs[0] {
		t.Fatalf("launched runs = %v, want the committed run %v", starter.launched, starter.runIDs)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 1)

	var derivation models.DatasetDerivation
	if err := db.Where("decision = ?", models.DatasetDecisionDerived).Take(&derivation).Error; err != nil {
		t.Fatalf("load derivation: %v", err)
	}
	if derivation.RunID == nil || *derivation.RunID != starter.runIDs[0] {
		t.Fatalf("derivation run id = %v, want %v", derivation.RunID, starter.runIDs[0])
	}
}

// TestEvaluatorPropagatesStartErrorThatCommittedNothing proves the recovery
// above does not swallow a genuine admission failure: when no run was
// committed, the error still reaches the caller.
func TestEvaluatorPropagatesStartErrorThatCommittedNothing(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "no-commit")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	starter := &fakeRunStarter{t: t, db: db, err: errors.New("boom")}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err == nil {
		t.Fatal("a start failure that committed nothing must propagate")
	}
	if len(starter.launched) != 0 {
		t.Fatalf("launched %v runs for a start that committed nothing", starter.launched)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 0)
}

// TestEvaluatorRecordsDeclinedAdmission covers the defensive branch: a run store
// that creates nothing and reports no error must be recorded as an admission
// skip, never launched.
func TestEvaluatorRecordsDeclinedAdmission(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "declined")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	starter := &fakeRunStarter{t: t, db: db, decline: true}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("start calls = %d, want 1", starter.calls)
	}
	if len(starter.launched) != 0 {
		t.Fatalf("launched %v runs on a declined admission, want none", starter.launched)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 0)
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

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate 1: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("start calls after first eval = %d, want 1", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 1)

	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate 2: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("start calls after dedupe eval = %d, want 1", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedActiveRun, 1)
}

func TestEvaluatorDoesNotDeriveCronTriggeredJob(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJobWithTriggerType(t, db, "cron-owned", models.TriggerTypeCron)
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("cron-triggered job started %d runs, want 0", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 0)
}

func TestEvaluatorSkipsJobWithSoftDeletedFreshnessTrigger(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJobWithTriggerType(t, db, "soft-deleted", models.TriggerTypeFreshness)
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	// Soft-delete the job's freshness trigger. A plain join would still match it;
	// the deleted_at filter must exclude it so the job is no longer treated as
	// freshness-triggered and derives nothing.
	var job models.Job
	if err := db.First(&job, "id = ?", jobID).Error; err != nil {
		t.Fatalf("load job: %v", err)
	}
	if err := db.Delete(&models.Trigger{}, "id = ?", job.TriggerID).Error; err != nil {
		t.Fatalf("soft-delete trigger: %v", err)
	}

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("soft-deleted-trigger job started %d runs, want 0", starter.calls)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)
	assertDerivationCount(t, db, models.DatasetDecisionDerived, 0)
}

func TestEvaluatorAdmissionErrorsAreRecorded(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	now := t0.Add(3 * time.Hour)
	jobID := seedFreshnessJob(t, db, "admission")
	seedDeclarations(t, db, produceDecl(jobID, "out", "1h", ""))
	seedState(t, db, "out", "100", now.Add(-2*time.Hour), nil)

	starter := &fakeRunStarter{t: t, db: db, err: runstorage.ErrRunQueued}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateOnce(ctx); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	assertDerivationCount(t, db, models.DatasetDecisionSkippedAdmission, 1)

	starter.err = errors.New("boom")
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

	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})

	advanced := datasetAdvancedEvent(t, "", "staging", uuid.Nil)

	// Pre-advance: staging watermark "1" == mart's consumed "1", so mart is
	// stale-upstream (waiting) and must NOT derive a redundant producer run.
	if err := eval.EvaluateEvent(ctx, advanced); err != nil {
		t.Fatalf("evaluate pre-advance: %v", err)
	}
	if starter.calls != 0 {
		t.Fatalf("pre-advance derived %d runs, want 0", starter.calls)
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
	if starter.calls != 1 {
		t.Fatalf("post-advance derived %d runs, want 1", starter.calls)
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
	starter := &fakeRunStarter{t: t, db: db}
	eval := NewEvaluator(Config{
		DB:                    db,
		RunStore:              starter,
		LaunchRun:             starter.launch,
		MaxDerivationsPerTick: 50,
		Now:                   func() time.Time { return now },
	})
	if err := eval.EvaluateEvent(ctx, advanced); err != nil {
		t.Fatalf("evaluate reactive: %v", err)
	}
	if starter.calls != 1 {
		t.Fatalf("reactive derived %d runs, want 1", starter.calls)
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
	return seedFreshnessJobWithTriggerType(t, db, alias, models.TriggerTypeFreshness)
}

func seedFreshnessJobWithTriggerType(t *testing.T, db *gorm.DB, alias string, triggerType models.TriggerType) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	trigger := models.Trigger{
		ID:            triggerID,
		Alias:         alias + "-trigger",
		Type:          triggerType,
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

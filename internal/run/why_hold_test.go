package run

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// F5: `caesium why` names the dataset hold behind a hold-skipped run, from what
// admission recorded on the run. These tests drive the real admission path
// (Store.Start) and read the answer back through Store.WhyTask.

func seedProducerRun(t *testing.T, db *gorm.DB, namespace string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{
		ID: id, JobID: uuid.New(), Namespace: namespace, Status: string(StatusSucceeded),
		TriggerType: "cron", StartedAt: time.Now().UTC(),
	}).Error)
	return id
}

// holdVerdicts renders the hold row's verdict JSON for one min breach.
func holdVerdicts(dataset string, observed float64) datatypes.JSON {
	return datatypes.JSON(fmt.Sprintf(
		`[{"dataset":%q,"metric":"rowCount","assertion":"min","observed":%v,"bound":1000,"message":"observed %v is below the declared min 1000"}]`,
		dataset, observed, observed))
}

// seedBrokenHold opens a hold the way a breaching producer run leaves it.
func seedBrokenHold(t *testing.T, db *gorm.DB, dataset string, opener, breach *uuid.UUID, verdicts datatypes.JSON) *models.DatasetHold {
	t.Helper()
	hold := seedActiveHold(t, db, dataset)
	require.NoError(t, db.Model(hold).Updates(map[string]any{
		"held_by_run_id":     opener,
		"last_breach_run_id": breach,
		"violations":         verdicts,
	}).Error)
	require.NoError(t, db.First(hold, "id = ?", hold.ID).Error)
	return hold
}

// whySkipped starts the consumer, expecting the gate to refuse it, and returns
// the explanation of its "read" step for the reader in ctx.
func whySkipped(t *testing.T, ctx context.Context, store *Store, db *gorm.DB, jobID uuid.UUID) (uuid.UUID, *WhyExplanation) {
	t.Helper()
	_, err := store.Start(jobID, nil)
	require.ErrorIs(t, err, ErrRunHeldUpstream)
	var run models.JobRun
	require.NoError(t, db.Where("job_id = ?", jobID).Order("created_at DESC").First(&run).Error)
	exp, err := store.WhyTask(ctx, run.ID, "read")
	require.NoError(t, err)
	return run.ID, exp
}

func newHoldWhyStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return NewStore(db), db
}

func TestWhyHoldSkippedRunNamesTheHold(t *testing.T) {
	store, db := newHoldWhyStore(t)
	dataset := "warehouse/orders"
	opener := seedProducerRun(t, db, "default")
	hold := seedBrokenHold(t, db, dataset, &opener, &opener, holdVerdicts(dataset, 12))
	jobID := seedConsumerJob(t, db, "consumer", dataset, "")

	runID, exp := whySkipped(t, context.Background(), store, db, jobID)

	// The fanned step of the same run is one unexpanded template row; it reads
	// the same as the plain step.
	fanned, err := store.WhyTask(context.Background(), runID, "fanned")
	require.NoError(t, err)
	assert.Equal(t, VerdictSkipped, fanned.Verdict)
	assert.Equal(t, exp.Summary, fanned.Summary)

	assert.Equal(t, VerdictSkipped, exp.Verdict)
	assert.Equal(t, "skipped", exp.Status)
	assert.Equal(t, fmt.Sprintf("SKIPPED — dataset /%s held since run %s (assertion rowCount.min: observed 12 vs bound 1000; hold %s)",
		dataset, opener, hold.ID), exp.Summary)
	assert.Contains(t, exp.SkipReason, "dataset_hold:/"+dataset)
	assert.Equal(t, "none", exp.Baseline.Kind)
	assert.Nil(t, exp.Diff, "a skipped task has no input diff")

	require.NotNil(t, exp.Trigger.Hold)
	assert.Equal(t, HoldDispositionSkipped, exp.Trigger.Hold.Disposition)
	require.Len(t, exp.Trigger.Hold.Holds, 1)
	got := exp.Trigger.Hold.Holds[0]
	assert.Equal(t, hold.ID, got.HoldID)
	assert.Equal(t, dataset, got.Dataset)
	assert.Equal(t, HoldDetailRecorded, got.Detail)
	require.NotNil(t, got.HeldSinceRunID)
	assert.Equal(t, opener, *got.HeldSinceRunID)
	require.Len(t, got.Violations, 1)
	require.NotNil(t, got.Violations[0].Observed)
	assert.InDelta(t, 12, *got.Violations[0].Observed, 0.001)
}

// A repeat breach replaces the hold's verdicts and a release closes it; neither
// may change why an older run was skipped, and a run skipped after the repeat
// breach carries the new verdicts.
func TestWhyHoldProvenanceIsReadAtSkipTimeNotLive(t *testing.T) {
	store, db := newHoldWhyStore(t)
	dataset := "warehouse/orders"
	opener := seedProducerRun(t, db, "default")
	hold := seedBrokenHold(t, db, dataset, &opener, &opener, holdVerdicts(dataset, 12))
	jobID := seedConsumerJob(t, db, "consumer", dataset, "")
	ctx := context.Background()

	firstRun, first := whySkipped(t, ctx, store, db, jobID)

	// A second breach folds into the same hold and replaces its verdicts.
	breach := seedProducerRun(t, db, "default")
	require.NoError(t, db.Model(hold).Updates(map[string]any{
		"violations":         holdVerdicts(dataset, 7),
		"last_breach_run_id": breach,
		"occurrence_count":   2,
	}).Error)
	_, second := whySkipped(t, ctx, store, db, jobID)
	assert.Contains(t, second.Summary, "observed 7 vs bound 1000")
	assert.Contains(t, second.Summary, "since run "+opener.String(), "held-since stays the opener")

	// Release closes the hold.
	_, err := store.ReleaseHold(ctx, hold.ID, ReleaseHoldParams{ReleasedBy: "user:ada"})
	require.NoError(t, err)

	again, err := store.WhyTask(ctx, firstRun, "read")
	require.NoError(t, err)
	assert.Equal(t, first.Summary, again.Summary, "neither the repeat breach nor the release may rewrite an old skip")
	assert.Contains(t, again.Summary, "observed 12 vs bound 1000")
	assert.Equal(t, first.Trigger.Hold, again.Trigger.Hold)
}

func TestWhyHoldSeveralHeldDatasets(t *testing.T) {
	store, db := newHoldWhyStore(t)
	opener := seedProducerRun(t, db, "default")
	first := seedBrokenHold(t, db, "warehouse/orders", &opener, &opener, holdVerdicts("warehouse/orders", 12))
	// Opened a moment later, so it sorts second.
	second := seedBrokenHold(t, db, "warehouse/users", &opener, &opener, holdVerdicts("warehouse/users", 3))
	require.NoError(t, db.Model(second).Update("opened_at", first.OpenedAt.Add(time.Second)).Error)
	jobID := seedConsumerJob(t, db, "consumer", "warehouse/orders", "")
	require.NoError(t, db.Create(&models.DatasetDeclaration{
		ID: uuid.New(), JobID: jobID, JobAlias: "consumer", StepName: "read",
		Name: "warehouse/users", Direction: models.DatasetDirectionConsumes,
	}).Error)

	_, exp := whySkipped(t, context.Background(), store, db, jobID)

	assert.Contains(t, exp.Summary, "dataset /warehouse/orders held since run")
	assert.Contains(t, exp.Summary, fmt.Sprintf("; also held: /warehouse/users (hold %s)", second.ID))
	require.NotNil(t, exp.Trigger.Hold)
	require.Len(t, exp.Trigger.Hold.Holds, 2, "every hold stays in the JSON")
	assert.Equal(t, first.ID, exp.Trigger.Hold.Holds[0].HoldID)
	assert.Equal(t, second.ID, exp.Trigger.Hold.Holds[1].HoldID)
	assert.Equal(t, "warehouse/users", exp.Trigger.Hold.Holds[1].Dataset)
}

// A confined reader sees the dataset and the hold id (its own job declares the
// dataset, and its task rows already carry both) but nothing owned by the
// namespace that produced the verdicts.
func TestWhyHoldTenantRule(t *testing.T) {
	dataset := "warehouse/orders"
	marketing := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})

	cases := []struct {
		name                     string
		openerNS, breachNS       string
		ctx                      context.Context
		wantSince, wantAssertion bool
		wantDetail               string
		// rewrite edits the snapshot after admission, for namespaces a run row
		// cannot be created with.
		rewrite func(*runHoldRef)
	}{
		{name: "unrestricted reader sees everything", openerNS: "finance", breachNS: "finance", ctx: context.Background(),
			wantSince: true, wantAssertion: true, wantDetail: HoldDetailRecorded},
		{name: "reader of both namespaces sees everything", openerNS: "marketing", breachNS: "marketing", ctx: marketing,
			wantSince: true, wantAssertion: true, wantDetail: HoldDetailRecorded},
		{name: "foreign opener and breach are withheld", openerNS: "finance", breachNS: "finance", ctx: marketing,
			wantDetail: HoldDetailWithheld},
		{name: "foreign opener, readable latest breach", openerNS: "finance", breachNS: "marketing", ctx: marketing,
			wantAssertion: true, wantDetail: HoldDetailWithheld},
		{name: "readable opener, foreign latest breach", openerNS: "marketing", breachNS: "finance", ctx: marketing,
			wantSince: true, wantDetail: HoldDetailWithheld},
		{name: "unattributable runs count as foreign", openerNS: "marketing", breachNS: "marketing", ctx: marketing,
			rewrite:    func(ref *runHoldRef) { ref.HeldByRunNamespace, ref.BreachRunNamespace = nil, nil },
			wantDetail: HoldDetailWithheld},
		{name: "a legacy blank run namespace reads as default", openerNS: "marketing", breachNS: "marketing",
			ctx:       authz.WithAllowedNamespaces(context.Background(), []string{"default"}),
			rewrite:   func(ref *runHoldRef) { ref.HeldByRunNamespace, ref.BreachRunNamespace = new(string), new(string) },
			wantSince: true, wantAssertion: true, wantDetail: HoldDetailRecorded},
		{name: "a legacy blank run namespace is not marketing's", openerNS: "marketing", breachNS: "marketing", ctx: marketing,
			rewrite:    func(ref *runHoldRef) { ref.HeldByRunNamespace, ref.BreachRunNamespace = new(string), new(string) },
			wantDetail: HoldDetailWithheld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, db := newHoldWhyStore(t)
			opener := seedProducerRun(t, db, tc.openerNS)
			breach := seedProducerRun(t, db, tc.breachNS)
			hold := seedBrokenHold(t, db, dataset, &opener, &breach, holdVerdicts(dataset, 12))
			jobID := seedConsumerJob(t, db, "consumer", dataset, "")
			_, err := store.Start(jobID, nil)
			require.ErrorIs(t, err, ErrRunHeldUpstream)
			var run models.JobRun
			require.NoError(t, db.Where("job_id = ?", jobID).First(&run).Error)
			// Pruning the producing runs after the skip must not change the
			// answer: the namespaces were read at admission.
			require.NoError(t, db.Delete(&models.JobRun{}, "id IN ?", []uuid.UUID{opener, breach}).Error)
			if tc.rewrite != nil {
				var stored runHoldDecision
				require.NoError(t, json.Unmarshal(run.HoldDecision, &stored))
				tc.rewrite(&stored.Holds[0])
				raw, err := json.Marshal(stored)
				require.NoError(t, err)
				require.NoError(t, db.Model(&run).Update("hold_decision", datatypes.JSON(raw)).Error)
			}

			exp, err := store.WhyTask(tc.ctx, run.ID, "read")
			require.NoError(t, err)
			got := exp.Trigger.Hold.Holds[0]

			assert.Equal(t, tc.wantDetail, got.Detail)
			assert.Equal(t, hold.ID, got.HoldID, "the hold id is on the reader's own task rows already")
			assert.Equal(t, dataset, got.Dataset)
			assert.Equal(t, tc.wantSince, got.HeldSinceRunID != nil)
			assert.Equal(t, tc.wantAssertion, len(got.Violations) > 0)

			body, err := json.Marshal(exp)
			require.NoError(t, err)
			if !tc.wantAssertion {
				assert.NotContains(t, exp.Summary, "rowCount")
				assert.NotContains(t, exp.Summary, "observed")
				assert.NotContains(t, string(body), "rowCount")
				assert.NotContains(t, string(body), `"observed"`)
				assert.NotContains(t, string(body), `"bound"`)
			}
			if !tc.wantSince {
				assert.NotContains(t, exp.Summary, opener.String())
				assert.NotContains(t, string(body), opener.String())
			}
			assert.Contains(t, exp.Summary, "hold "+hold.ID.String())
			assert.Contains(t, exp.Summary, "SKIPPED — dataset /"+dataset+" held")
		})
	}
}

func TestWhyHoldRedactedSummaryForAConfinedReader(t *testing.T) {
	store, db := newHoldWhyStore(t)
	dataset := "warehouse/orders"
	finance := seedProducerRun(t, db, "finance")
	hold := seedBrokenHold(t, db, dataset, &finance, &finance, holdVerdicts(dataset, 12))
	jobID := seedConsumerJob(t, db, "consumer", dataset, "")

	_, exp := whySkipped(t, authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), store, db, jobID)

	assert.Equal(t, fmt.Sprintf("SKIPPED — dataset /%s held (assertion detail withheld; hold %s)", dataset, hold.ID), exp.Summary)
}

// A delta verdict's bound comes from a baseline; one not computed over the
// breaching run's own namespace is never shown to a confined reader.
func TestWhyHoldWithholdsAnUnownedBaselineBound(t *testing.T) {
	store, db := newHoldWhyStore(t)
	dataset := "warehouse/orders"
	run := seedProducerRun(t, db, "marketing")
	verdicts := datatypes.JSON(`[{"dataset":"warehouse/orders","metric":"rowCount","assertion":"deltaFromBaseline","observed":10,"bound":15668.5,"baseline_median":31337,"baseline_samples":5,"message":"x"}]`)
	seedBrokenHold(t, db, dataset, &run, &run, verdicts)
	jobID := seedConsumerJob(t, db, "consumer", dataset, "")
	_, err := store.Start(jobID, nil)
	require.ErrorIs(t, err, ErrRunHeldUpstream)
	var skipped models.JobRun
	require.NoError(t, db.Where("job_id = ?", jobID).First(&skipped).Error)

	confined, err := store.WhyTask(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), skipped.ID, "read")
	require.NoError(t, err)
	assert.Contains(t, confined.Summary, "rowCount.deltaFromBaseline: observed 10 vs bound withheld")
	assert.Equal(t, HoldDetailWithheld, confined.Trigger.Hold.Holds[0].Detail)
	body, err := json.Marshal(confined)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "15668")
	assert.NotContains(t, string(body), "31337")

	open, err := store.WhyTask(context.Background(), skipped.ID, "read")
	require.NoError(t, err)
	assert.Contains(t, open.Summary, "observed 10 vs bound 15668.5")
}

// A hold-skipped run recorded before F5 has no snapshot. It degrades to what
// its skip reason and task rows still say, not to a blank dataset and not to an
// error.
func TestWhyHoldSkippedRunWithoutProvenanceDegrades(t *testing.T) {
	store, db := newHoldWhyStore(t)
	holdID := uuid.New()

	seed := func(skipReason, taskError string) (uuid.UUID, string) {
		runID, jobID, taskID := uuid.New(), uuid.New(), uuid.New()
		now := time.Now().UTC()
		require.NoError(t, db.Create(&models.JobRun{
			ID: runID, JobID: jobID, Status: string(StatusSkipped), SkipReason: skipReason,
			TriggerType: "cron", StartedAt: now, CompletedAt: &now,
		}).Error)
		require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, Name: "read"}).Error)
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`,
			Status: string(TaskStatusSkipped), Error: taskError, Attempt: 1,
			CreatedAt: now, UpdatedAt: now,
		}).Error)
		return runID, "read"
	}

	t.Run("named dataset and hold id", func(t *testing.T) {
		runID, task := seed("dataset_hold:/warehouse/orders", "dataset_hold:/warehouse/orders hold="+holdID.String())
		exp, err := store.WhyTask(context.Background(), runID, task)
		require.NoError(t, err)
		assert.Equal(t, VerdictSkipped, exp.Verdict)
		assert.Equal(t, fmt.Sprintf("SKIPPED — dataset /warehouse/orders held (no assertion detail was recorded for this run; hold %s)", holdID), exp.Summary)
		require.NotNil(t, exp.Trigger.Hold)
		assert.Equal(t, HoldDetailUnrecorded, exp.Trigger.Hold.Holds[0].Detail)
	})
	t.Run("no hold id on the row", func(t *testing.T) {
		runID, task := seed("dataset_hold:/warehouse/orders", "dataset_hold:/warehouse/orders")
		exp, err := store.WhyTask(context.Background(), runID, task)
		require.NoError(t, err)
		assert.Equal(t, "SKIPPED — dataset /warehouse/orders held (no assertion detail was recorded for this run)", exp.Summary)
	})
	t.Run("unparseable reason falls back to the recorded text", func(t *testing.T) {
		runID, task := seed("dataset_hold:", "dataset_hold: hold="+holdID.String())
		exp, err := store.WhyTask(context.Background(), runID, task)
		require.NoError(t, err)
		assert.Equal(t, "SKIPPED — dataset_hold: hold="+holdID.String(), exp.Summary)
		assert.Nil(t, exp.Trigger.Hold)
	})
}

// A trigger-rule skip is SKIPPED too, carrying the row's own reason; it used to
// read UNKNOWN.
func TestWhyTaskSkippedByTriggerRule(t *testing.T) {
	store, db := newHoldWhyStore(t)
	jobID, runID := uuid.New(), uuid.New()
	taskID, _ := seedUnfannedWhyTask(t, db, jobID, runID, "report")
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runID).Updates(map[string]any{
		"status": string(TaskStatusSkipped), "error": `trigger rule "all_success" not satisfied`,
	}).Error)

	exp, err := store.WhyTask(context.Background(), runID, taskID.String())
	require.NoError(t, err)

	assert.Equal(t, VerdictSkipped, exp.Verdict)
	assert.Equal(t, `SKIPPED — trigger rule "all_success" not satisfied`, exp.Summary)
	assert.Equal(t, `trigger rule "all_success" not satisfied`, exp.SkipReason)
	assert.Nil(t, exp.Trigger.Hold)

	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runID).Update("error", "").Error)
	exp, err = store.WhyTask(context.Background(), runID, taskID.String())
	require.NoError(t, err)
	assert.Equal(t, `SKIPPED — task "report" was skipped and no reason was recorded`, exp.Summary)
}

// A fanned step whose every instance was skipped answers SKIPPED at group level
// with the same reason; a group that ran some instances keeps its cache verdict.
func TestWhyTaskFannedGroupSkipped(t *testing.T) {
	store, db := newHoldWhyStore(t)
	jobID := uuid.New()
	reason := `trigger rule "all_success" not satisfied`

	runID, _, _ := seedFanOutWhy(t, db, jobID, []TaskStatus{TaskStatusSkipped, TaskStatusSkipped, TaskStatusSkipped})
	require.NoError(t, db.Model(&models.TaskRun{}).Where("job_run_id = ?", runID).Update("error", reason).Error)
	exp, err := store.WhyTask(context.Background(), runID, "process")
	require.NoError(t, err)
	assert.Equal(t, VerdictSkipped, exp.Verdict)
	assert.Equal(t, "SKIPPED — "+reason, exp.Summary)
	require.NotNil(t, exp.Group)
	assert.Equal(t, 3, exp.Group.StatusCounts["skipped"])

	mixedRun, _, _ := seedFanOutWhy(t, db, uuid.New(), []TaskStatus{TaskStatusSkipped, TaskStatusSucceeded, TaskStatusSucceeded})
	mixed, err := store.WhyTask(context.Background(), mixedRun, "process")
	require.NoError(t, err)
	assert.NotEqual(t, VerdictSkipped, mixed.Verdict)
	assert.Contains(t, mixed.Summary, "FANNED GROUP")
}

// metadata.onUpstreamHold: run admits the run onto held data; `why` says so,
// including under a concurrency policy, whose admission inserts the row with a
// raw statement.
func TestWhyHoldAdmittedPastAHold(t *testing.T) {
	for _, policy := range []string{"", "queue"} {
		t.Run("policy="+policy, func(t *testing.T) {
			store, db := newHoldWhyStore(t)
			dataset := "warehouse/orders"
			opener := seedProducerRun(t, db, "default")
			hold := seedBrokenHold(t, db, dataset, &opener, &opener, holdVerdicts(dataset, 12))
			jobID := seedConsumerJob(t, db, "opted-out", dataset, jobdef.OnUpstreamHoldRun)
			if policy != "" {
				raw, err := json.Marshal(&jobdef.Concurrency{MaxRuns: 3, Strategy: policy})
				require.NoError(t, err)
				require.NoError(t, db.Model(&models.Job{}).Where("id = ?", jobID).Update("concurrency", datatypes.JSON(raw)).Error)
			}

			created, err := store.Start(jobID, nil)
			require.NoError(t, err)
			require.NotNil(t, created)

			var tasks []models.Task
			require.NoError(t, db.Where("job_id = ?", jobID).Find(&tasks).Error)
			require.NoError(t, db.Create(&models.TaskRun{
				ID: uuid.New(), JobRunID: created.ID, TaskID: tasks[0].ID, AtomID: tasks[0].AtomID,
				Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["true"]`,
				Status: string(TaskStatusSucceeded), Attempt: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
			}).Error)

			exp, err := store.WhyTask(context.Background(), created.ID, tasks[0].ID.String())
			require.NoError(t, err)
			require.NotNil(t, exp.Trigger.Hold)
			assert.Equal(t, HoldDispositionAdmitted, exp.Trigger.Hold.Disposition)
			assert.Contains(t, exp.Summary, fmt.Sprintf(
				"admitted by metadata.onUpstreamHold: run on dataset /%s held since run %s (assertion rowCount.min: observed 12 vs bound 1000; hold %s)",
				dataset, opener, hold.ID))
			assert.NotEqual(t, VerdictSkipped, exp.Verdict)
		})
	}
}

// A run no hold touched has no hold block: the JSON is what it was before F5.
func TestWhyWithoutHoldOmitsTheBlock(t *testing.T) {
	store, db := newHoldWhyStore(t)
	jobID, runID := uuid.New(), uuid.New()
	seedUnfannedWhyTask(t, db, jobID, runID, "extract")

	exp, err := store.WhyTask(context.Background(), runID, "extract")
	require.NoError(t, err)
	assert.Nil(t, exp.Trigger.Hold)
	body, err := json.Marshal(exp)
	require.NoError(t, err)
	assert.NotContains(t, string(body), `"hold"`)
	assert.NotContains(t, string(body), "skipReason")
}

package dataset

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// tenancyFixture is a catalog with two producers in two namespaces:
//
//   - "orders": produced by the marketing job, observed (state last run in
//     marketing, reason written by the marketing producer);
//   - "ledger": produced by the finance job, declared but never observed
//     (ownership before the first run);
//   - "shared": a legacy co-produced dataset (both jobs declare it; the
//     single-producer validation would refuse that today, so it is seeded),
//     whose state was last written by the finance run;
//   - "raw.vendor": a source the marketing job declares (owns nothing);
//   - "orphan": a state with no declaration at all.
//
// Declarations carry a NULL dataset namespace, exactly as the registry
// writes them, while states and holds carry ”.
type tenancyFixture struct {
	db             *gorm.DB
	trigger        uuid.UUID
	mktJob, finJob uuid.UUID
	mktRun, finRun uuid.UUID
	atom           uuid.UUID
	mktTask        uuid.UUID
	finTask        uuid.UUID
	now            time.Time
}

func newTenancyFixture(t *testing.T) *tenancyFixture {
	t.Helper()
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })
	f := &tenancyFixture{db: conn, now: time.Now().UTC().Add(-time.Hour)}
	f.trigger = uuid.New()
	require.NoError(t, conn.Create(&models.Trigger{ID: f.trigger, Type: models.TriggerTypeCron}).Error)
	f.mktJob = f.job(t, "mkt-producer", "marketing")
	f.finJob = f.job(t, "fin-producer", "finance")
	f.atom = uuid.New()
	require.NoError(t, conn.Create(&models.Atom{ID: f.atom, Engine: models.AtomEngineDocker, Image: "alpine:3.23"}).Error)
	f.mktTask, f.finTask = uuid.New(), uuid.New()
	require.NoError(t, conn.Create(&models.Task{ID: f.mktTask, JobID: f.mktJob, AtomID: f.atom, Name: "load"}).Error)
	require.NoError(t, conn.Create(&models.Task{ID: f.finTask, JobID: f.finJob, AtomID: f.atom, Name: "load"}).Error)
	f.mktRun = f.run(t, f.mktJob, "marketing")
	f.finRun = f.run(t, f.finJob, "finance")

	f.declare(t, f.mktJob, "mkt-producer", "orders", models.DatasetDirectionProduces)
	f.declare(t, f.finJob, "fin-producer", "ledger", models.DatasetDirectionProduces)
	f.declare(t, f.mktJob, "mkt-producer", "shared", models.DatasetDirectionProduces)
	f.declare(t, f.finJob, "fin-producer", "shared", models.DatasetDirectionProduces)
	f.declare(t, f.mktJob, "mkt-producer", "raw.vendor", models.DatasetDirectionSource)

	marketing, finance := "marketing", "finance"
	f.state(t, models.DatasetState{Name: "orders", Status: models.DatasetStatusStaleUpstream, Reason: "waiting on raw.mkt", ReasonNamespace: &marketing,
		LastRunID: &f.mktRun, ConsumedRunID: &f.mktRun, ConsumedWatermarks: datatypes.JSON(`{"raw.mkt":"7"}`)})
	f.state(t, models.DatasetState{Name: "shared", Status: models.DatasetStatusStaleUpstream, Reason: "waiting on raw.fin", ReasonNamespace: &finance,
		LastRunID: &f.finRun, ConsumedRunID: &f.finRun, ConsumedWatermarks: datatypes.JSON(`{"raw.fin":"9"}`)})
	f.state(t, models.DatasetState{Name: "orphan", Status: models.DatasetStatusFresh})
	return f
}

func (f *tenancyFixture) job(t *testing.T, alias, namespace string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, f.db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: f.trigger}).Error)
	return id
}

func (f *tenancyFixture) run(t *testing.T, jobID uuid.UUID, namespace string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, f.db.Create(&models.JobRun{ID: id, JobID: jobID, TriggerID: f.trigger, Namespace: namespace, Status: "succeeded", StartedAt: f.now}).Error)
	return id
}

func (f *tenancyFixture) declare(t *testing.T, jobID uuid.UUID, alias, name, direction string) {
	t.Helper()
	require.NoError(t, f.db.Create(&models.DatasetDeclaration{ID: uuid.New(), JobID: jobID, JobAlias: alias, StepName: "load", Name: name,
		Direction: direction, Freshness: "1h", CreatedAt: f.now, UpdatedAt: f.now}).Error)
}

func (f *tenancyFixture) state(t *testing.T, state models.DatasetState) {
	t.Helper()
	state.ID = uuid.New()
	state.CreatedAt, state.UpdatedAt = f.now, f.now
	require.NoError(t, f.db.Create(&state).Error)
}

func (f *tenancyFixture) service(ctx context.Context) *Service {
	return NewWithDatabase(ctx, f.db)
}

func scoped(namespaces ...string) context.Context {
	return authz.WithAllowedNamespaces(context.Background(), namespaces)
}

func listedDatasets(t *testing.T, svc *Service) ([]string, map[string]State, int64) {
	t.Helper()
	result, err := svc.List(ListParams{Limit: 200})
	require.NoError(t, err)
	names := make([]string, 0, len(result.Datasets))
	byName := map[string]State{}
	for _, state := range result.Datasets {
		names = append(names, state.Name)
		byName[state.Name] = state
	}
	slices.Sort(names)
	return names, byName, result.Total
}

func TestTenancyListFollowsDeclaredProducers(t *testing.T) {
	f := newTenancyFixture(t)

	names, _, total := listedDatasets(t, f.service(context.Background()))
	require.Equal(t, []string{"ledger", "orders", "orphan", "raw.vendor", "shared"}, names, "an unrestricted caller lists every dataset")
	require.EqualValues(t, 5, total)

	names, byName, total := listedDatasets(t, f.service(scoped("marketing")))
	require.Equal(t, []string{"orders", "shared"}, names, "a source declaration and an undeclared state own nothing")
	require.EqualValues(t, 2, total, "the total counts only what the caller sees")
	require.Equal(t, "waiting on raw.mkt", byName["orders"].Reason)
	require.Equal(t, f.mktRun, *byName["orders"].LastRunID)
	require.NotEmpty(t, byName["orders"].ConsumedWatermarks)
	shared := byName["shared"]
	require.Nil(t, shared.LastRunID, "the last run is finance's")
	require.Empty(t, shared.ConsumedWatermarks)
	require.Empty(t, shared.Reason, "finance's producer wrote the reason")
	require.Equal(t, models.DatasetStatusStaleUpstream, shared.Status, "dataset-level fields stay")

	names, _, _ = listedDatasets(t, f.service(scoped("finance")))
	require.Equal(t, []string{"ledger", "shared"}, names, "a declared, never observed dataset is its producer's from the start")

	names, _, total = listedDatasets(t, f.service(scoped()))
	require.Empty(t, names)
	require.Zero(t, total)

	// Observed-only status filter takes the same predicate.
	result, err := f.service(scoped("marketing")).List(ListParams{Status: models.DatasetStatusFresh})
	require.NoError(t, err)
	require.Empty(t, result.Datasets, "orphan is fresh but cluster-only")
}

func TestTenancyDetailNamesOnlyAReadableProducer(t *testing.T) {
	f := newTenancyFixture(t)
	detail, err := f.service(scoped("marketing")).Get("", "shared")
	require.NoError(t, err)
	require.Equal(t, "mkt-producer", detail.Producing.Alias)
	body, err := json.Marshal(detail)
	require.NoError(t, err)
	for _, foreign := range []string{"fin-producer", f.finJob.String(), f.finRun.String(), "raw.fin"} {
		require.NotContains(t, string(body), foreign)
	}

	detail, err = f.service(scoped("finance")).Get("", "shared")
	require.NoError(t, err)
	require.Equal(t, "fin-producer", detail.Producing.Alias)
	require.Equal(t, "waiting on raw.fin", detail.State.Reason)

	// A source declarer owns nothing: a restricted lookup finds no producer.
	_, err = f.service(scoped("marketing")).Get("", "raw.vendor")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// TestTenancyReasonFollowsTheEvaluatingProducer: a read right after the
// producer moved (before the evaluator re-ran) must not hand the reason the
// old namespace's evaluation wrote to the new namespace.
func TestTenancyReasonFollowsTheEvaluatingProducer(t *testing.T) {
	f := newTenancyFixture(t)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.mktJob).Update("namespace", "finance").Error)
	detail, err := f.service(scoped("finance")).Get("", "orders")
	require.NoError(t, err)
	require.Empty(t, detail.State.Reason, "the reason was written for marketing")
	require.Nil(t, detail.State.LastRunID, "the last run is persisted in marketing")
	names, _, _ := listedDatasets(t, f.service(scoped("marketing")))
	require.Empty(t, names, "both producers now live in finance")

	// A legacy row (no reason owner) hides its reason from every confined reader.
	require.NoError(t, f.db.Model(&models.DatasetState{}).Where("name = ?", "orders").Update("reason_namespace", nil).Error)
	detail, err = f.service(scoped("finance", "marketing")).Get("", "orders")
	require.NoError(t, err)
	require.Empty(t, detail.State.Reason)
	detail, err = f.service(context.Background()).Get("", "orders")
	require.NoError(t, err)
	require.Equal(t, "waiting on raw.mkt", detail.State.Reason, "an unrestricted caller sees it")
}

func (f *tenancyFixture) derivation(t *testing.T, name, decision string, at time.Duration, producer *uuid.UUID, namespace *string, run *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, f.db.Create(&models.DatasetDerivation{ID: id, Name: name, Decision: decision, ProducerJobID: producer, JobNamespace: namespace,
		RunID: run, CreatedAt: f.now.Add(at)}).Error)
	return id
}

func derivationIDs(t *testing.T, svc *Service, name string) []uuid.UUID {
	t.Helper()
	result, err := svc.Derivations(DerivationsParams{Name: name})
	require.NoError(t, err)
	require.EqualValues(t, len(result.Derivations), result.Total)
	ids := make([]uuid.UUID, 0, len(result.Derivations))
	for _, row := range result.Derivations {
		ids = append(ids, row.ID)
	}
	return ids
}

func TestTenancyDerivationsAreOwnedByTheirStampedNamespace(t *testing.T) {
	f := newTenancyFixture(t)
	marketing, finance := "marketing", "finance"
	mktSkip := f.derivation(t, "shared", models.DatasetDecisionSkippedAdmission, 1*time.Minute, &f.mktJob, &marketing, nil)
	finDerived := f.derivation(t, "shared", models.DatasetDecisionDerived, 2*time.Minute, &f.finJob, &finance, &f.finRun)
	legacy := f.derivation(t, "shared", models.DatasetDecisionSkippedFresh, 3*time.Minute, nil, nil, nil)
	partial := f.derivation(t, "shared", models.DatasetDecisionSkippedFresh, 4*time.Minute, &f.finJob, nil, nil)
	// A marketing decision pointing at a covering run persisted in finance.
	covering := f.derivation(t, "shared", models.DatasetDecisionSkippedActiveRun, 5*time.Minute, &f.mktJob, &marketing, &f.finRun)

	require.Equal(t, []uuid.UUID{covering, partial, legacy, finDerived, mktSkip}, derivationIDs(t, f.service(context.Background()), "shared"),
		"an unrestricted caller reads every row, newest first")
	require.Equal(t, []uuid.UUID{covering, mktSkip}, derivationIDs(t, f.service(scoped("marketing")), "shared"),
		"a run-less skip is its producer's namespace's; a legacy row needs every producer namespace")
	require.Equal(t, []uuid.UUID{covering, legacy, finDerived, mktSkip}, derivationIDs(t, f.service(scoped("finance", "marketing")), "shared"),
		"a partially stamped row is hidden from every confined reader")

	result, err := f.service(scoped("marketing")).Derivations(DerivationsParams{Name: "shared"})
	require.NoError(t, err)
	require.Nil(t, result.Derivations[0].RunID, "the covering run is persisted in finance")
	detail, err := f.service(scoped("marketing")).Get("", "shared")
	require.NoError(t, err)
	require.Equal(t, covering, detail.LastDecision.ID)
	require.Nil(t, detail.LastDecision.RunID)
	detail, err = f.service(scoped("finance")).Get("", "shared")
	require.NoError(t, err)
	require.Equal(t, finDerived, detail.LastDecision.ID, "the latest decision finance may read")

	// Single producer: legacy rows follow the one producer namespace.
	ordersLegacy := f.derivation(t, "orders", models.DatasetDecisionSkippedFresh, time.Minute, nil, nil, nil)
	require.Equal(t, []uuid.UUID{ordersLegacy}, derivationIDs(t, f.service(scoped("marketing")), "orders"))
	require.Empty(t, derivationIDs(t, f.service(scoped("finance")), "orders"))
}

// TestTenancyUnrestrictedBodiesAreUnchanged is the * / no-policy oracle: the
// derivation and state bodies of an unrestricted caller carry exactly the
// pre-B6 keys, including for newly stamped decisions.
func TestTenancyUnrestrictedBodiesAreUnchanged(t *testing.T) {
	f := newTenancyFixture(t)
	marketing := "marketing"
	f.derivation(t, "orders", models.DatasetDecisionDerived, time.Minute, &f.mktJob, &marketing, &f.mktRun)
	f.derivation(t, "orders", models.DatasetDecisionSkippedFresh, 2*time.Minute, nil, nil, nil)
	result, err := f.service(context.Background()).Derivations(DerivationsParams{Name: "orders"})
	require.NoError(t, err)
	require.Len(t, result.Derivations, 2)
	keys := func(v any) []string {
		body, err := json.Marshal(v)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(body, &m))
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	require.Equal(t, []string{"created_at", "decision", "id", "name", "run_id"}, keys(result.Derivations[1]), "stamped row")
	require.Equal(t, []string{"created_at", "decision", "id", "name"}, keys(result.Derivations[0]), "legacy row")
	detail, err := f.service(context.Background()).Get("", "orders")
	require.NoError(t, err)
	require.Equal(t, []string{"consumed_watermarks", "created_at", "id", "last_run_id", "name", "reason", "status", "updated_at", "watermark"}, keys(detail.State))
	// No history scope is attached for an unrestricted principal, so every
	// read above runs today's queries.
	cluster := &auth.Principal{Grants: policy.Grants{models.ClusterNamespace: models.RoleAdmin}}
	require.False(t, authz.Restricted(authz.WithHistoryScope(context.Background(), cluster)))
}

func (f *tenancyFixture) metricSample(t *testing.T, runID, taskID uuid.UUID, value float64, at time.Duration) uuid.UUID {
	t.Helper()
	taskRun := models.TaskRun{ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: f.atom, Engine: models.AtomEngineDocker,
		Image: "alpine:3.23", Command: "[]", Status: "succeeded"}
	// task_runs is unique on (job_run_id, task_id): one task run per sample
	// needs its own task row.
	task := models.Task{ID: uuid.New(), JobID: f.mktJob, AtomID: f.atom, Name: "load-" + uuid.NewString()}
	require.NoError(t, f.db.Create(&task).Error)
	taskRun.TaskID = task.ID
	require.NoError(t, f.db.Create(&taskRun).Error)
	id := uuid.New()
	require.NoError(t, f.db.Create(&models.DatasetMetric{ID: id, TaskRunID: taskRun.ID, Name: "orders", Metric: "rowCount", Value: value,
		CreatedAt: f.now.Add(at)}).Error)
	return id
}

func TestTenancyMetricsFollowTheProducingRun(t *testing.T) {
	f := newTenancyFixture(t)
	// History that stayed in finance after the producer moved, with a
	// distinctive value, then marketing's own samples.
	foreign := []uuid.UUID{f.metricSample(t, f.finRun, f.finTask, 9999, time.Minute), f.metricSample(t, f.finRun, f.finTask, 9999, 2*time.Minute)}
	own := []uuid.UUID{
		f.metricSample(t, f.mktRun, f.mktTask, 10, 3*time.Minute),
		f.metricSample(t, f.mktRun, f.mktTask, 12, 4*time.Minute),
		f.metricSample(t, f.mktRun, f.mktTask, 14, 5*time.Minute),
	}

	all, err := f.service(context.Background()).Metrics("", "orders", "rowCount", MetricsParams{})
	require.NoError(t, err)
	require.EqualValues(t, 5, all.Total)
	require.Equal(t, 5, all.Baseline.Samples)

	mine, err := f.service(scoped("marketing")).Metrics("", "orders", "rowCount", MetricsParams{})
	require.NoError(t, err)
	require.EqualValues(t, 3, mine.Total)
	require.Len(t, mine.Series, 3)
	for i, sample := range mine.Series {
		require.Equal(t, own[i], sample.ID)
		require.True(t, sample.InBaseline)
		require.NotContains(t, foreign, sample.ID)
	}
	require.Equal(t, 3, mine.Baseline.Samples)
	require.InDelta(t, 12, mine.Baseline.Median, 0.001)
	require.Equal(t, []float64{10, 12, 14}, mine.Baseline.Values)

	theirs, err := f.service(scoped("finance")).Metrics("", "orders", "rowCount", MetricsParams{})
	require.NoError(t, err, "finance is not a producer, but the service shapes whatever the middleware let through")
	require.EqualValues(t, 2, theirs.Total)
	require.InDelta(t, 9999, theirs.Baseline.Median, 0.001)
}

func TestTenancyHoldsAreShapedByOwner(t *testing.T) {
	f := newTenancyFixture(t)
	activeKey := "/shared"
	shared := models.DatasetHold{ID: uuid.New(), Name: "shared", Status: models.DatasetHoldStatusActive, ActiveKey: &activeKey, Reason: "min",
		HeldByJobID: f.finJob, HeldByJobAlias: "fin-producer", HeldByRunID: &f.finRun, HeldByStepName: "load", LastBreachRunID: &f.finRun,
		Violations: datatypes.JSON(`[{"baseline_median":9999}]`), Impact: datatypes.JSON(`{"downstream":[{"job":"fin-report"}]}`),
		OpenedAt: f.now.Add(2 * time.Minute), CreatedAt: f.now, UpdatedAt: f.now}
	released := f.now.Add(time.Minute)
	orders := models.DatasetHold{ID: uuid.New(), Name: "orders", Status: models.DatasetHoldStatusReleased, Reason: "min",
		HeldByJobID: f.mktJob, HeldByJobAlias: "mkt-producer", HeldByRunID: &f.mktRun, Violations: datatypes.JSON(`[{"baseline_median":10}]`),
		ReleaseRunID: &f.finRun, ReleasedAt: &released, ReleasedBy: "system", OpenedAt: f.now.Add(time.Minute), CreatedAt: f.now, UpdatedAt: f.now}
	orphanKey := "/orphan"
	orphan := models.DatasetHold{ID: uuid.New(), Name: "orphan", Status: models.DatasetHoldStatusActive, ActiveKey: &orphanKey, HeldByJobID: uuid.New(),
		OpenedAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}
	for _, hold := range []*models.DatasetHold{&shared, &orders, &orphan} {
		require.NoError(t, f.db.Create(hold).Error)
	}
	holds := func(ctx context.Context) map[string]models.DatasetHold {
		t.Helper()
		result, err := f.service(ctx).Holds(HoldsParams{Status: "all"})
		require.NoError(t, err)
		require.EqualValues(t, len(result.Holds), result.Total)
		out := map[string]models.DatasetHold{}
		for _, hold := range result.Holds {
			out[hold.Name] = hold
		}
		return out
	}

	all := holds(context.Background())
	require.Len(t, all, 3)
	require.Equal(t, shared.Impact, all["shared"].Impact)
	require.Equal(t, f.finRun, *all["orders"].ReleaseRunID)

	mkt := holds(scoped("marketing"))
	require.Len(t, mkt, 2, "the orphan hold is cluster-only")
	require.Equal(t, uuid.Nil, mkt["shared"].HeldByJobID)
	require.Empty(t, mkt["shared"].HeldByJobAlias)
	require.Nil(t, mkt["shared"].HeldByRunID)
	require.Nil(t, mkt["shared"].LastBreachRunID)
	require.Nil(t, mkt["shared"].Violations, "the breach baseline is finance's")
	require.Nil(t, mkt["shared"].Impact)
	require.Equal(t, "mkt-producer", mkt["orders"].HeldByJobAlias)
	require.NotNil(t, mkt["orders"].Violations)
	require.Nil(t, mkt["orders"].ReleaseRunID, "the clearing run is persisted in finance")
	body, err := json.Marshal(mkt)
	require.NoError(t, err)
	for _, foreign := range []string{"fin-producer", f.finJob.String(), f.finRun.String(), "9999", "fin-report"} {
		require.NotContains(t, string(body), foreign)
	}

	fin := holds(scoped("finance"))
	require.Len(t, fin, 1)
	require.Equal(t, "fin-producer", fin["shared"].HeldByJobAlias)
	require.NotNil(t, fin["shared"].Violations)
	require.Nil(t, fin["shared"].Impact, "the impact cone is never shown to a confined reader")

	// The detail's active hold is shaped the same way.
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
	t.Cleanup(func() { t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false"); require.NoError(t, env.Process()) })
	detail, err := f.service(scoped("marketing")).Get("", "shared")
	require.NoError(t, err)
	require.NotNil(t, detail.Hold)
	require.Empty(t, detail.Hold.HeldByJobAlias)
	require.Nil(t, detail.Hold.Impact)
}

func confinedPrincipal(grants policy.Grants) *auth.Principal {
	return &auth.Principal{Kind: auth.PrincipalAPIKey, Subject: "test", Grants: grants}
}

func swapHook(t *testing.T, hook func()) {
	t.Helper()
	prev := beforeGuardedWrite
	beforeGuardedWrite = hook
	t.Cleanup(func() { beforeGuardedWrite = prev })
}

func TestTenancyAdvanceRechecksProducersInTheWrite(t *testing.T) {
	f := newTenancyFixture(t)
	mktRunner := confinedPrincipal(policy.Grants{"marketing": models.RoleRunner})
	svc := f.service(context.Background())

	result, err := svc.Advance(AdvanceParams{Name: "orders", Watermark: "100", Principal: mktRunner})
	require.NoError(t, err)
	require.Equal(t, "100", result.State.Watermark)
	require.Equal(t, "waiting on raw.mkt", result.State.Reason, "marketing's own reason")

	_, err = svc.Advance(AdvanceParams{Name: "shared", Watermark: "100", Principal: mktRunner})
	var denial *authz.Denial
	require.ErrorAs(t, err, &denial, "a co-produced dataset needs runner in every producer namespace")
	require.Equal(t, []string{"finance", "marketing"}, denial.Namespaces)

	// The producer moves to finance after the middleware's check: the
	// in-transaction re-check refuses, and nothing is written.
	swapHook(t, func() {
		require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.mktJob).Update("namespace", "finance").Error)
	})
	_, err = svc.Advance(AdvanceParams{Name: "orders", Watermark: "200", Principal: mktRunner})
	require.ErrorAs(t, err, &denial)
	var state models.DatasetState
	require.NoError(t, f.db.Where("name = ?", "orders").Take(&state).Error)
	require.Equal(t, "100", state.Watermark, "a refused advance writes nothing")

	// Unconfined: no guard at all, as before.
	cluster := confinedPrincipal(policy.Grants{models.ClusterNamespace: models.RoleRunner})
	_, err = svc.Advance(AdvanceParams{Name: "shared", Watermark: "300", Principal: cluster})
	require.NoError(t, err)
	_, err = svc.Advance(AdvanceParams{Name: "orphan", Watermark: "300"})
	require.NoError(t, err, "auth off")
}

// TestTenancyManualAdvanceKeepsHidingAMovedProducersInputs: a manual advance
// clears last_run_id but keeps the previous run's consumed snapshot; after
// the producer moved, that snapshot is still the old namespace's.
func TestTenancyManualAdvanceKeepsHidingAMovedProducersInputs(t *testing.T) {
	f := newTenancyFixture(t)
	finance := "finance"
	f.state(t, models.DatasetState{Name: "ledger", Status: models.DatasetStatusFresh, Watermark: "1", LastRunID: &f.finRun, ConsumedRunID: &f.finRun,
		ConsumedWatermarks: datatypes.JSON(`{"raw.fin":"3"}`), Reason: "fresh", ReasonNamespace: &finance})
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.finJob).Update("namespace", "marketing").Error)
	mktRunner := confinedPrincipal(policy.Grants{"marketing": models.RoleRunner})
	result, err := f.service(context.Background()).Advance(AdvanceParams{Name: "ledger", Watermark: "2", Principal: mktRunner})
	require.NoError(t, err)
	require.Nil(t, result.State.LastRunID)
	require.Empty(t, result.State.ConsumedWatermarks, "finance's inputs are not marketing's")
	var stored models.DatasetState
	require.NoError(t, f.db.Where("name = ?", "ledger").Take(&stored).Error)
	require.Nil(t, stored.LastRunID)
	require.NotEmpty(t, stored.ConsumedWatermarks, "precondition: the snapshot survives a manual advance")
	detail, err := f.service(scoped("marketing")).Get("", "ledger")
	require.NoError(t, err)
	require.Empty(t, detail.State.ConsumedWatermarks)
	require.Empty(t, detail.State.Reason)
	names, byName, _ := listedDatasets(t, f.service(scoped("marketing")))
	require.Contains(t, names, "ledger")
	require.Empty(t, byName["ledger"].ConsumedWatermarks)
}

// TestTenancyConsumedSnapshotKeepsItsOwnRun: a run of the moved producer that
// declares no inputs moves last_run_id but leaves the old run's snapshot.
func TestTenancyConsumedSnapshotKeepsItsOwnRun(t *testing.T) {
	f := newTenancyFixture(t)
	require.NoError(t, f.db.Model(&models.DatasetState{}).Where("name = ?", "orders").Update("consumed_run_id", f.finRun).Error)
	detail, err := f.service(scoped("marketing")).Get("", "orders")
	require.NoError(t, err)
	require.Equal(t, f.mktRun, *detail.State.LastRunID, "the last run is marketing's")
	require.Empty(t, detail.State.ConsumedWatermarks, "the snapshot is the finance run's")
	require.NoError(t, f.db.Model(&models.DatasetState{}).Where("name = ?", "orders").Update("consumed_run_id", nil).Error)
	detail, err = f.service(scoped("marketing")).Get("", "orders")
	require.NoError(t, err)
	require.Empty(t, detail.State.ConsumedWatermarks, "an unattributed snapshot is hidden")
	detail, err = f.service(context.Background()).Get("", "orders")
	require.NoError(t, err)
	require.NotEmpty(t, detail.State.ConsumedWatermarks, "an unrestricted caller sees it")
}

// TestTenancyWriteRefusedWhenItsProducerIsDeleted: a producer deleted between
// the middleware's check and the write leaves the dataset cluster-only.
func TestTenancyWriteRefusedWhenItsProducerIsDeleted(t *testing.T) {
	f := newTenancyFixture(t)
	swapHook(t, func() {
		require.NoError(t, f.db.Delete(&models.Job{}, "id = ?", f.mktJob).Error)
	})
	_, err := f.service(context.Background()).Advance(AdvanceParams{Name: "orders", Watermark: "9",
		Principal: confinedPrincipal(policy.Grants{"marketing": models.RoleRunner})})
	var denial *authz.Denial
	require.ErrorAs(t, err, &denial)
}

func TestTenancyAdvanceRetriesWhenAProducerAppears(t *testing.T) {
	f := newTenancyFixture(t)
	mktRunner := confinedPrincipal(policy.Grants{"marketing": models.RoleRunner})
	calls := 0
	swapHook(t, func() {
		calls++
		if calls == 1 {
			// A second marketing producer appears between the lock read and
			// the write: its alias was not locked, so the write starts over.
			second := f.job(t, "mkt-second", "marketing")
			f.declare(t, second, "mkt-second", "orders", models.DatasetDirectionProduces)
		}
	})
	_, err := f.service(context.Background()).Advance(AdvanceParams{Name: "orders", Watermark: "5", Principal: mktRunner})
	require.NoError(t, err)
	require.Equal(t, 2, calls, "the write re-read and re-locked the new producer set")

	// A producer set that changes on every attempt is a 409, never a pass.
	swapHook(t, func() {
		extra := f.job(t, "mkt-"+uuid.NewString(), "marketing")
		f.declare(t, extra, "mkt-extra", "orders", models.DatasetDirectionProduces)
	})
	_, err = f.service(context.Background()).Advance(AdvanceParams{Name: "orders", Watermark: "6", Principal: mktRunner})
	require.ErrorIs(t, err, ErrProducersChanged)
}

func TestTenancyReleaseRechecksProducersInTheWrite(t *testing.T) {
	f := newTenancyFixture(t)
	prev := storeFor
	storeFor = runstore.NewStore
	t.Cleanup(func() { storeFor = prev })
	hold := func(name string) uuid.UUID {
		key := "/" + name
		id := uuid.New()
		require.NoError(t, f.db.Create(&models.DatasetHold{ID: id, Name: name, Status: models.DatasetHoldStatusActive, ActiveKey: &key,
			HeldByJobID: f.mktJob, HeldByRunID: &f.finRun, HeldByJobAlias: "mkt-producer", Impact: datatypes.JSON(`{"x":1}`),
			OpenedAt: f.now, CreatedAt: f.now, UpdatedAt: f.now}).Error)
		return id
	}
	status := func(id uuid.UUID) string {
		var row models.DatasetHold
		require.NoError(t, f.db.Select("status").Take(&row, "id = ?", id).Error)
		return row.Status
	}
	mktOperator := confinedPrincipal(policy.Grants{"marketing": models.RoleOperator})
	svc := f.service(context.Background())
	params := func(id uuid.UUID, caller *auth.Principal) ReleaseHoldParams {
		return ReleaseHoldParams{HoldID: id, Principal: "op", Reason: "ack", Caller: caller}
	}

	sharedHold := hold("shared")
	_, err := svc.ReleaseHold(params(sharedHold, mktOperator))
	var denial *authz.Denial
	require.ErrorAs(t, err, &denial)
	require.Equal(t, models.DatasetHoldStatusActive, status(sharedHold))

	ordersHold := hold("orders")
	result, err := svc.ReleaseHold(params(ordersHold, mktOperator))
	require.NoError(t, err)
	require.Equal(t, models.DatasetHoldStatusReleased, result.Hold.Status)
	require.Nil(t, result.Hold.Impact, "the response is shaped for the confined caller")
	require.Nil(t, result.Hold.HeldByRunID, "the opening run is persisted in finance")

	// The producer moves to finance between the check and the release.
	movedHold := hold("ledger")
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.finJob).Update("namespace", "marketing").Error)
	swapHook(t, func() {
		require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", f.finJob).Update("namespace", "finance").Error)
	})
	_, err = svc.ReleaseHold(params(movedHold, mktOperator))
	require.ErrorAs(t, err, &denial)
	require.Equal(t, models.DatasetHoldStatusActive, status(movedHold), "a refused release changes nothing")
	var audits []models.AuditLog
	require.NoError(t, f.db.Where("resource_id = ?", movedHold.String()).Find(&audits).Error)
	require.Len(t, audits, 1)
	require.Equal(t, "failure", audits[0].Outcome)
	require.False(t, errors.Is(err, ErrProducersChanged))
}

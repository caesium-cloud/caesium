package incident

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	fixturejson "github.com/caesium-cloud/caesium/internal/testutil"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setJobNamespace(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace string) {
	t.Helper()
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", namespace).Error)
}

func setRunNamespace(t *testing.T, db *gorm.DB, runID uuid.UUID, namespace string) {
	t.Helper()
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Update("namespace", namespace).Error)
}

// namespacedLineage builds marketing job-a whose trusted output D1 is consumed
// by marketing job-b and finance job-f, and returns the ids.
func namespacedLineage(t *testing.T, db *gorm.DB) (jobA, jobB, jobF, failRun uuid.UUID) {
	t.Helper()
	tr := mkTrigger(t, db)
	jobA = mkJob(t, db, tr, "mkt-a")
	setJobNamespace(t, db, jobA, "marketing")
	trusted := mkRun(t, db, jobA, tr)
	setRunNamespace(t, db, trusted, "marketing")
	mkDataset(t, db, mkTaskRun(t, db, trusted), "output", "ns", "D1")
	failRun = mkRun(t, db, jobA, tr)
	setRunNamespace(t, db, failRun, "marketing")

	jobB = mkJob(t, db, tr, "mkt-b")
	setJobNamespace(t, db, jobB, "marketing")
	taskB := mkTaskRun(t, db, mkRun(t, db, jobB, tr))
	mkDataset(t, db, taskB, "input", "ns", "D1")
	mkDataset(t, db, taskB, "output", "ns", "D2")

	jobF = mkJob(t, db, tr, "fin-f")
	setJobNamespace(t, db, jobF, "finance")
	taskF := mkTaskRun(t, db, mkRun(t, db, jobF, tr))
	mkDataset(t, db, taskF, "input", "ns", "D1")
	mkDataset(t, db, taskF, "output", "ns", "D3")
	return jobA, jobB, jobF, failRun
}

// TestFreezeAllowlistKeepsOnlyIncidentNamespace: the lineage walk reaches a
// finance consumer, which is counted as external and never named (A8).
func TestFreezeAllowlistKeepsOnlyIncidentNamespace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	jobA, _, _, failRun := namespacedLineage(t, db)

	frozen, external := FreezeAllowlist(ctx, db, jobA, &failRun, "marketing")
	require.Equal(t, []string{"mkt-a", "mkt-b"}, frozen)
	require.Equal(t, 1, external)

	// The own job follows the same rule: once it moved out, nothing it reaches
	// in marketing is lost, but it is no longer named itself.
	setJobNamespace(t, db, jobA, "finance")
	frozen, external = FreezeAllowlist(ctx, db, jobA, &failRun, "marketing")
	require.Equal(t, []string{"mkt-b"}, frozen)
	require.Equal(t, 2, external)

	// The open path persists the count with the list.
	setJobNamespace(t, db, jobA, "marketing")
	inc, _, err := NewStore(db).OpenOrAppend(ctx, OpenParams{JobID: jobA, RunID: &failRun, TaskName: "t", Class: ClassUnknown})
	require.NoError(t, err)
	require.Equal(t, "marketing", inc.Namespace)
	require.Equal(t, 1, inc.AllowedJobsExternal)
	require.JSONEq(t, `["mkt-a","mkt-b"]`, string(inc.AllowedJobs))
	raw, err := json.Marshal(inc)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "fin-f")
}

// TestFreezeAllowlistSingleNamespaceUnchanged: with every job in one namespace
// the list is the whole walk and the count stays out of the JSON.
func TestFreezeAllowlistSingleNamespaceUnchanged(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	jobA, _, _, failRun := namespacedLineage(t, db)
	require.NoError(t, db.Model(&models.Job{}).Where("1 = 1").Update("namespace", "default").Error)
	setRunNamespace(t, db, failRun, "default")
	inc, _, err := NewStore(db).OpenOrAppend(ctx, OpenParams{JobID: jobA, RunID: &failRun, TaskName: "t", Class: ClassUnknown})
	require.NoError(t, err)
	require.JSONEq(t, `["fin-f","mkt-a","mkt-b"]`, string(inc.AllowedJobs))
	raw, err := json.Marshal(inc)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "allowed_jobs_external")
}

func incidentIn(t *testing.T, db *gorm.DB, jobID uuid.UUID, runID *uuid.UUID, namespace string, allowed ...string) *models.Incident {
	t.Helper()
	encoded, err := json.Marshal(allowed)
	require.NoError(t, err)
	now := time.Now().UTC()
	inc := &models.Incident{
		ID: uuid.New(), Namespace: namespace, JobID: jobID, RunID: runID, TaskName: "t",
		Class: string(ClassUnknown), Status: models.IncidentStatusOpen, DedupeKey: uuid.NewString(),
		AllowedJobs: datatypes.JSON(encoded), OpenedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(inc).Error)
	return inc
}

// TestEffectiveAllowlistDropsMovedJobs: a frozen alias whose job moved out of
// the incident's namespace is dropped at serve time; an unknown alias too.
func TestEffectiveAllowlistDropsMovedJobs(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	jobA, jobB, _, failRun := namespacedLineage(t, db)
	inc := incidentIn(t, db, jobA, &failRun, "marketing", "mkt-a", "mkt-b", "fin-f", "gone")

	allowed, err := EffectiveAllowlist(ctx, db, inc)
	require.NoError(t, err)
	require.Equal(t, []string{"mkt-a", "mkt-b"}, allowed)

	setJobNamespace(t, db, jobB, "finance")
	allowed, err = EffectiveAllowlist(ctx, db, inc)
	require.NoError(t, err)
	require.Equal(t, []string{"mkt-a"}, allowed)
}

// TestBundleConfinedToIncidentNamespace: the bundle names no job outside the
// incident's namespace, reports the external count, lists only runs recorded
// in the namespace, and shows no DAG once the own job moved out.
func TestBundleConfinedToIncidentNamespace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	jobA, _, _, failRun := namespacedLineage(t, db)
	inc := incidentIn(t, db, jobA, &failRun, "marketing", "mkt-a", "mkt-b", "fin-f")
	require.NoError(t, db.Model(inc).Update("allowed_jobs_external", 1).Error)
	foreignRun := mkRun(t, db, jobA, mkTrigger(t, db))
	setRunNamespace(t, db, foreignRun, "finance")
	require.NoError(t, db.Create(&models.Task{ID: uuid.New(), JobID: jobA, Name: "step"}).Error)

	b, err := BuildBundle(ctx, db, inc.ID, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"mkt-a", "mkt-b"}, b.LineageImpact.AllowedJobs)
	require.Equal(t, 1, b.LineageImpact.External)
	for _, r := range b.RunHistory {
		require.NotEqual(t, foreignRun, r.ID, "a run recorded in finance stays with finance")
	}
	require.NotEmpty(t, b.RunHistory)
	require.Len(t, b.Job.Tasks, 1)
	require.False(t, b.Job.NamespaceMoved)
	raw, err := json.Marshal(b)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "fin-f")

	setJobNamespace(t, db, jobA, "finance")
	b, err = BuildBundle(ctx, db, inc.ID, nil)
	require.NoError(t, err)
	require.True(t, b.Job.NamespaceMoved)
	require.Empty(t, b.Job.Tasks)
	require.Empty(t, b.Job.Edges)
	require.Equal(t, []string{"mkt-b"}, b.LineageImpact.AllowedJobs)
}

// TestBundleReadsTopologyAndOwnershipFromOneSnapshot: a job moves to finance
// (taking a finance DAG) after the bundle reads the job row, and back to
// marketing (with a new DAG) after it reads the tasks. The bundle's job, tasks
// and edges come from one snapshot, so it never returns the intermediate
// finance topology under the restored marketing namespace.
func TestBundleReadsTopologyAndOwnershipFromOneSnapshot(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "catalog.db") + "?_journal_mode=WAL&_busy_timeout=5000"
	reader, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { testutil.CloseDB(reader) })
	require.NoError(t, reader.AutoMigrate(models.All...))
	writer, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() { testutil.CloseDB(writer) })
	ctx := context.Background()

	jobA, _, _, failRun := namespacedLineage(t, writer)
	inc := incidentIn(t, writer, jobA, &failRun, "marketing", "mkt-a")
	require.NoError(t, writer.Create(&models.Task{ID: uuid.New(), JobID: jobA, Name: "marketing-step"}).Error)
	replaceDAG := func(namespace, step string) {
		require.NoError(t, writer.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`UPDATE jobs SET namespace = ? WHERE id = ?`, namespace, jobA).Error; err != nil {
				return err
			}
			if err := tx.Exec(`DELETE FROM tasks WHERE job_id = ?`, jobA).Error; err != nil {
				return err
			}
			return tx.Create(&models.Task{ID: uuid.New(), JobID: jobA, Name: step}).Error
		}))
	}

	moves := 0
	require.NoError(t, reader.Callback().Query().After("gorm:query").Register("a8-two-moves", func(tx *gorm.DB) {
		switch {
		case moves == 0 && tx.Statement.Table == "jobs" && tx.Statement.Schema != nil && len(tx.Statement.Selects) == 0:
			moves++
			replaceDAG("finance", "finance-step")
		case moves == 1 && tx.Statement.Table == "tasks":
			moves++
			replaceDAG("marketing", "marketing-step-v2")
		}
	}))
	t.Cleanup(func() { _ = reader.Callback().Query().Remove("a8-two-moves") })

	b, err := BuildBundle(ctx, reader, inc.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 2, moves, "both moves committed during the bundle's reads")
	require.False(t, b.Job.NamespaceMoved)
	require.Len(t, b.Job.Tasks, 1)
	require.Equal(t, "marketing-step", b.Job.Tasks[0].Name, "the DAG the snapshot's marketing owner had; never the intermediate finance DAG")

	// Moved out for good: the next bundle withholds the DAG.
	replaceDAG("finance", "finance-step")
	b, err = BuildBundle(ctx, reader, inc.ID, nil)
	require.NoError(t, err)
	require.True(t, b.Job.NamespaceMoved)
	require.Empty(t, b.Job.Tasks)
}

// TestResolvePlaybookMovedJobIgnoresRemediation: a job that moved out of the
// incident's namespace resolves exactly as a job that declares no remediation
// (the deployment default), so its new owner's block never reaches the agent.
func TestResolvePlaybookMovedJobIgnoresRemediation(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	seedNamespacedProfile(t, db, "mkt-default", "marketing", ActionTypeEscalate)
	seedNamespacedProfile(t, db, "mkt-wide", "marketing", ActionTypePauseJob, ActionTypeRerunWithParams)
	tr := mkTrigger(t, db)
	withBlock := mkJob(t, db, tr, "with-block")
	plain := mkJob(t, db, tr, "plain")
	block := `{"profile":"mkt-wide","autonomy":{"allow":["pause_job","rerun_with_params"],"paramOverrides":{"secret_param":["a","b"]}}}`
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", withBlock).Update("remediation", block).Error)
	for _, id := range []uuid.UUID{withBlock, plain} {
		setJobNamespace(t, db, id, "marketing")
	}
	incBlock := incidentIn(t, db, withBlock, nil, "marketing")
	incPlain := incidentIn(t, db, plain, nil, "marketing")

	none := ResolvePlaybook(ctx, db, incPlain.ID, "mkt-default")
	inPlace := ResolvePlaybook(ctx, db, incBlock.ID, "mkt-default")
	require.NotEqual(t, string(none.Document()), string(inPlace.Document()), "in place, the job's block applies")
	require.Contains(t, string(inPlace.Document()), "secret_param")
	require.Equal(t, "mkt-wide", ResolveProfile(ctx, db, incBlock.ID, "mkt-default").Name)

	setJobNamespace(t, db, withBlock, "finance")
	moved := ResolvePlaybook(ctx, db, incBlock.ID, "mkt-default")
	require.Equal(t, string(none.Document()), string(moved.Document()), "a moved job resolves like a job with no remediation block")
	require.NotContains(t, string(moved.Document()), "secret_param")
	require.Equal(t, "mkt-default", ResolveProfile(ctx, db, incBlock.ID, "mkt-default").Name)
}

// TestWithholdLegacyAgentContent: sessions and agent rows from before
// namespace confinement lose their content; confined and non-agent rows keep
// theirs.
func TestWithholdLegacyAgentContent(t *testing.T) {
	sessions := []models.AgentSession{
		{SessionLog: "legacy transcript"},
		{SessionLog: "confined transcript", NamespaceConfined: true},
	}
	actions := []models.AgentAction{
		{Actor: models.AgentActionActorAgent, Params: datatypes.JSON(`{"text":"legacy"}`), Result: datatypes.JSON(`{}`)},
		{Actor: models.AgentActionActorAgent, Params: datatypes.JSON(`{"text":"new"}`), NamespaceConfined: true},
		{Actor: models.AgentActionActorPolicy, Params: datatypes.JSON(`{"run":"x"}`)},
		{Actor: models.AgentActionActorHuman, Params: datatypes.JSON(`{"by":"op"}`)},
	}
	WithholdLegacyAgentContent(sessions, actions)
	require.Empty(t, sessions[0].SessionLog)
	require.True(t, sessions[0].SessionLogWithheld)
	require.Equal(t, "confined transcript", sessions[1].SessionLog)
	require.False(t, sessions[1].SessionLogWithheld)
	require.Nil(t, actions[0].Params)
	require.Nil(t, actions[0].Result)
	require.True(t, actions[0].ContentWithheld)
	for _, a := range actions[1:] {
		require.NotNil(t, a.Params)
		require.False(t, a.ContentWithheld)
	}
}

// TestBundleNotesWithholdLegacyForConfinedReader: a confined reader gets only
// notes written by namespace-confined sessions.
func TestBundleNotesWithholdLegacyForConfinedReader(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	tr := mkTrigger(t, db)
	job := mkJob(t, db, tr, "noted")
	inc := incidentIn(t, db, job, nil, "default")
	legacy, err := RecordNote(ctx, db, inc.ID, nil, &inc.Namespace, "legacy finance quote")
	require.NoError(t, err)
	require.NoError(t, db.Model(legacy).Update("namespace_confined", false).Error)
	_, err = RecordNote(ctx, db, inc.ID, nil, &inc.Namespace, "confined note")
	require.NoError(t, err)

	all, err := BuildBundleFor(ctx, db, inc.ID, nil, ReaderUnrestricted)
	require.NoError(t, err)
	require.Len(t, all.Notes, 2)
	confined, err := BuildBundleFor(ctx, db, inc.ID, nil, ReaderConfined)
	require.NoError(t, err)
	require.Len(t, confined.Notes, 1)
	require.Equal(t, "confined note", confined.Notes[0].Text)
}

// TestExecuteApprovedRefusesJobMovedOutOfIncidentNamespace: an approved action
// whose incident job has since moved to another namespace is refused and
// recorded failed; nothing is dispatched (A8).
func TestExecuteApprovedRefusesJobMovedOutOfIncidentNamespace(t *testing.T) {
	enableAuthMode(t)
	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "transform", "")

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Params:     ActionParams{Definition: json.RawMessage(`{"apiVersion":"v1"}`)},
	})
	require.NoError(t, err)
	approveAction(t, db, action.ID, "carol@example.com", "")
	setJobNamespace(t, db, inc.JobID, "finance")

	executed, err := exec.ExecuteApproved(context.Background(), action.ID)
	require.ErrorIs(t, err, ErrNamespaceMoved)
	require.Equal(t, models.AgentActionStatusFailed, executed.Status)
	require.Empty(t, ops.applyPatch, "nothing is rendered or applied for a job that moved out")
	require.Empty(t, ops.escalate)
	require.Contains(t, string(executed.Result), "namespace_moved")
}

// TestReserveAgentSessionPersistsConfinement: the capped reservation insert
// stores the session's namespace-confinement mark (A8), and a legacy session
// stays unmarked.
func TestReserveAgentSessionPersistsConfinement(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	jobA, _, _, failRun := namespacedLineage(t, db)
	inc := incidentIn(t, db, jobA, &failRun, "marketing")
	now := time.Now().UTC()
	for _, confined := range []bool{true, false} {
		session := &models.AgentSession{ID: uuid.New(), Namespace: &inc.Namespace, IncidentID: inc.ID, Engine: models.AtomEngineDocker,
			State: models.AgentSessionStatePending, NamespaceConfined: confined, CreatedAt: now, UpdatedAt: now}
		capHit, err := NewStore(db).ReserveAgentSession(ctx, session, jobA, 10, 10)
		require.NoError(t, err)
		require.Equal(t, CapNone, capHit)
		var stored models.AgentSession
		require.NoError(t, db.First(&stored, "id = ?", session.ID).Error)
		require.Equal(t, confined, stored.NamespaceConfined)
	}
}

// TestAgentRowsTakeTheAuthorsConfinement: an agent action or note written
// with a legacy credential stays unmarked, so confined readers never see it;
// policy rows are always marked.
func TestAgentRowsTakeTheAuthorsConfinement(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	inc := incidentIn(t, db, mkJob(t, db, mkTrigger(t, db), "authored"), nil, "default")
	e := &Executor{}
	require.False(t, e.newAction(inc, ActionRequest{Type: "note"}, 0, models.AgentActionActorAgent).NamespaceConfined)
	require.True(t, e.newAction(inc, ActionRequest{Type: "note", NamespaceConfined: true}, 0, models.AgentActionActorAgent).NamespaceConfined)
	require.True(t, e.newAction(inc, ActionRequest{Type: "note"}, 0, models.AgentActionActorPolicy).NamespaceConfined)

	legacy, err := RecordNoteBy(ctx, db, inc.ID, nil, &inc.Namespace, "legacy", false)
	require.NoError(t, err)
	confined, err := RecordNoteBy(ctx, db, inc.ID, nil, &inc.Namespace, "confined", true)
	require.NoError(t, err)
	var rows []models.AgentAction
	require.NoError(t, db.Where("id IN ?", []uuid.UUID{legacy.ID, confined.ID}).Find(&rows).Error)
	for _, row := range rows {
		require.Equal(t, row.ID == confined.ID, row.NamespaceConfined)
	}
}

// TestVerifyNamespaceBoundaryRefusesForeignRun: a run recorded in another
// namespace is refused even while the job is in place; a missing run is left
// to the action itself.
func TestVerifyNamespaceBoundaryRefusesForeignRun(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()
	jobA, _, _, failRun := namespacedLineage(t, db)
	inc := incidentIn(t, db, jobA, &failRun, "marketing")

	require.NoError(t, verifyNamespaceBoundary(ctx, db, inc, failRun))
	require.NoError(t, verifyNamespaceBoundary(ctx, db, inc, uuid.New()))
	setRunNamespace(t, db, failRun, "finance")
	require.ErrorIs(t, verifyNamespaceBoundary(ctx, db, inc, failRun), ErrNamespaceMoved)
	setRunNamespace(t, db, failRun, "marketing")
	setJobNamespace(t, db, jobA, "finance")
	require.ErrorIs(t, verifyNamespaceBoundary(ctx, db, inc, uuid.Nil), ErrNamespaceMoved)
}

// TestApplyJobdefPatchRefusalNeverQuotesAMovedJobsPolicy: a move to finance
// that commits after dispatch's namespace check but before the patch reads
// the job's remediation is refused on the row the remediation comes from, so
// the refusal recorded on the marketing incident never quotes finance's
// policy.
func TestApplyJobdefPatchRefusalNeverQuotesAMovedJobsPolicy(t *testing.T) {
	enableAuthMode(t)
	db, store, ops, exec := newExecutorTest(t)
	inc, _, _ := seedJobIncident(t, db, store, "gate", "")
	require.NoError(t, db.Model(&models.Incident{}).Where("id = ?", inc.ID).Update("namespace", "marketing").Error)
	setJobNamespace(t, db, inc.JobID, "marketing")
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).
		Update("remediation", datatypes.JSON(fixturejson.MustJSONBytes(t, triageOnlyBlock()))).Error)
	var job models.Job
	require.NoError(t, db.First(&job, "id = ?", inc.JobID).Error)

	action, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeApplyJobdefPatch,
		Params:     ActionParams{Definition: remediationPatch(job.Alias, triageOnlyBlock())},
	})
	require.NoError(t, err)
	approveAction(t, db, action.ID, "erin@example.com", "")

	// The boundary check reads only the job's namespace; the move lands right
	// after it, replacing the remediation with finance's.
	financePolicy := `{"profile":"fin-wide","autonomy":{"paramOverrides":{"fin_secret_param":["a"]}}}`
	moved := false
	require.NoError(t, db.Callback().Query().After("gorm:query").Register("a8-move-after-boundary", func(tx *gorm.DB) {
		if moved || tx.Statement.Table != "jobs" || len(tx.Statement.Selects) != 2 || tx.Statement.Selects[1] != "namespace" {
			return
		}
		moved = true
		require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Exec(
			`UPDATE jobs SET namespace = 'finance', remediation = ? WHERE id = ?`, financePolicy, inc.JobID).Error)
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove("a8-move-after-boundary") })

	_, err = exec.ExecuteApproved(context.Background(), action.ID)
	require.True(t, moved, "the move ran after the boundary check")
	require.ErrorIs(t, err, ErrNamespaceMoved)
	require.NotContains(t, err.Error(), "fin_secret_param")
	require.Empty(t, ops.applyPatch)
	require.Empty(t, ops.escalate)

	var recorded models.AgentAction
	require.NoError(t, db.First(&recorded, "id = ?", action.ID).Error)
	require.Equal(t, models.AgentActionStatusFailed, recorded.Status)
	require.NotContains(t, string(recorded.Result), "fin_secret_param")
	require.NotContains(t, string(recorded.Result), "fin-wide")
}

package incident

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/lineage"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// enableDataAssertions turns the master gate on for one test. The cleanup is
// registered before t.Setenv so it runs after the variable is restored.
func enableDataAssertions(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { _ = env.Process() })
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	require.NoError(t, env.Process())
}

// holdWorld is one job with a green run of its "load" step, as a hold finds it.
type holdWorld struct {
	t         *testing.T
	db        *gorm.DB
	sub       *Subscriber
	jobID     uuid.UUID
	alias     string
	runID     uuid.UUID
	taskID    uuid.UUID
	taskRunID uuid.UUID
}

func newHoldWorld(t *testing.T, namespace string, cooldown time.Duration) *holdWorld {
	t.Helper()
	enableDataAssertions(t)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	w := &holdWorld{t: t, db: db, sub: NewSubscriber(nil, db, nil, cooldown), jobID: uuid.New(), alias: "orders-" + uuid.NewString()[:8], taskID: uuid.New()}
	require.NoError(t, db.Create(&models.Job{ID: w.jobID, Alias: w.alias, Namespace: namespace}).Error)
	require.NoError(t, db.Create(&models.Task{ID: w.taskID, JobID: w.jobID, AtomID: uuid.New(), Name: "load"}).Error)
	w.runID, w.taskRunID = w.greenRun(namespace)
	return w
}

// greenRun records a run whose "load" task succeeded.
func (w *holdWorld) greenRun(namespace string) (runID, taskRunID uuid.UUID) {
	w.t.Helper()
	now := time.Now().UTC()
	runID, taskRunID = uuid.New(), uuid.New()
	require.NoError(w.t, w.db.Create(&models.JobRun{ID: runID, JobID: w.jobID, Namespace: namespace, Status: "succeeded", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(w.t, w.db.Create(&models.TaskRun{
		ID: taskRunID, JobRunID: runID, TaskID: w.taskID, AtomID: uuid.New(),
		Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "sh",
		Status: "succeeded", Result: "success", LogText: "loaded 12 rows",
	}).Error)
	return runID, taskRunID
}

// hold records an active hold on dataset, returning its id.
func (w *holdWorld) hold(dataset string, impact datatypes.JSON) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	key := models.DatasetHoldKey("", dataset)
	require.NoError(w.t, w.db.Create(&models.DatasetHold{
		ID: id, Name: dataset, Status: models.DatasetHoldStatusActive, ActiveKey: &key,
		Reason: "min", HeldByJobID: w.jobID, Impact: impact, OccurrenceCount: 1, OpenedAt: time.Now().UTC(),
	}).Error)
	return id
}

// heldEvent is the dataset_held event for hold, carrying what the store writes.
func (w *holdWorld) heldEvent(holdID uuid.UUID, dataset string) event.Event {
	w.t.Helper()
	payload, err := json.Marshal(map[string]any{
		"hold_id": holdID, "namespace": "", "dataset": dataset, "status": "active", "reason": "min",
		"job_alias": w.alias, "step": "load", "task_id": w.taskID, "task_run_id": w.taskRunID,
		"impacted_datasets": 1,
		"violations": []map[string]any{{
			"dataset": dataset, "metric": "rowCount", "assertion": "min", "observed": 12, "bound": 1000,
			"baseline_median": 5000, "baseline_samples": 9, "baseline_scope": "namespace",
			"message": "rowCount 12 is below min 1000",
		}},
	})
	require.NoError(w.t, err)
	return event.Event{Type: event.TypeDatasetHeld, JobID: w.jobID, RunID: w.runID, Payload: payload}
}

// release marks the hold released and delivers dataset_released.
func (w *holdWorld) release(ctx context.Context, holdID uuid.UUID) {
	w.t.Helper()
	require.NoError(w.t, w.db.Model(&models.DatasetHold{}).Where("id = ?", holdID).
		Updates(map[string]any{"status": models.DatasetHoldStatusReleased, "active_key": nil}).Error)
	payload, err := json.Marshal(map[string]any{
		"hold_id": holdID, "dataset": "x", "status": "released", "release_reason": "manual_ack", "released_by": "user:ada",
	})
	require.NoError(w.t, err)
	w.sub.handle(ctx, event.Event{Type: event.TypeDatasetReleased, JobID: w.jobID, Payload: payload})
}

func (w *holdWorld) incidents() []models.Incident {
	w.t.Helper()
	var rows []models.Incident
	require.NoError(w.t, w.db.Where("job_id = ?", w.jobID).Order("opened_at, id").Find(&rows).Error)
	return rows
}

func (w *holdWorld) only() models.Incident {
	w.t.Helper()
	rows := w.incidents()
	require.Len(w.t, rows, 1)
	return rows[0]
}

func (w *holdWorld) lineageRow(taskRunID uuid.UUID, name, direction string) {
	w.t.Helper()
	require.NoError(w.t, w.db.Create(&models.LineageDataset{
		ID: uuid.New(), TaskRunID: taskRunID, Namespace: "ol", Name: name, Direction: direction, CreatedAt: time.Now().UTC(),
	}).Error)
}

// consumer records a job in the world's namespace that read input and wrote
// <alias>-out.
func (w *holdWorld) consumer(alias, input string) {
	w.t.Helper()
	jobID, runID, taskRunID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	require.NoError(w.t, w.db.Create(&models.Job{ID: jobID, Alias: alias, Namespace: "finance"}).Error)
	require.NoError(w.t, w.db.Create(&models.JobRun{ID: runID, JobID: jobID, Namespace: "finance", Status: "succeeded", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(w.t, w.db.Create(&models.TaskRun{ID: taskRunID, JobRunID: runID, TaskID: uuid.New(), AtomID: uuid.New(), Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "sh", Status: "succeeded"}).Error)
	w.lineageRow(taskRunID, input, "input")
	w.lineageRow(taskRunID, alias+"-out", "output")
}

func coneNode(namespace, dataset string) lineage.ImpactNode {
	return lineage.ImpactNode{DatasetName: dataset, JobAlias: dataset + "-job", JobNamespace: namespace, RunNamespace: namespace}
}

func storedCone(t *testing.T, nodes ...lineage.ImpactNode) datatypes.JSON {
	t.Helper()
	raw, err := lineage.EncodeStoredImpact(&lineage.ImpactResult{RootName: "orders", Downstream: nodes})
	require.NoError(t, err)
	return raw
}

func TestClassifyDatasetHeldIsDataQualityHold(t *testing.T) {
	sig := Signal{EventType: string(event.TypeDatasetHeld), LogTail: "permission denied", ExitCode: intPtr(137)}
	require.Equal(t, ClassDataQualityHold, NewClassifier().Classify(sig))
}

func TestHoldOpensIncidentWithEvidence(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)

	w.sub.handle(ctx, w.heldEvent(holdID, "orders"))

	inc := w.only()
	require.Equal(t, string(ClassDataQualityHold), inc.Class)
	require.Equal(t, models.IncidentStatusOpen, inc.Status)
	require.Equal(t, "finance", inc.Namespace, "the tenant is the producing run's, not the dataset's empty namespace")
	require.Equal(t, w.runID, *inc.RunID)
	require.Equal(t, w.taskID, *inc.TaskID)
	require.Equal(t, "load", inc.TaskName)
	require.Equal(t, 1, inc.OccurrenceCount)
	require.JSONEq(t, fmt.Sprintf("[%q]", w.alias), string(inc.AllowedJobs))
	require.Equal(t, `dataset "orders" metric "rowCount" assertion "min": rowCount 12 is below min 1000`, inc.LastError)

	var evidence map[string]any
	require.NoError(t, json.Unmarshal(inc.Evidence, &evidence))
	require.Equal(t, "data_quality_hold", evidence["class"])
	require.Equal(t, holdID.String(), evidence["hold_id"])
	require.Equal(t, "orders", evidence["dataset"])
	require.Equal(t, "", evidence["dataset_namespace"])
	require.Equal(t, w.taskRunID.String(), evidence["task_run_id"])
	violation := evidence["violations"].([]any)[0].(map[string]any)
	require.Equal(t, "min", violation["assertion"])
	require.EqualValues(t, 12, violation["observed"])
	require.EqualValues(t, 1000, violation["bound"])
	require.EqualValues(t, 5000, violation["baseline_median"])
	require.EqualValues(t, 9, violation["baseline_samples"])
	require.NotContains(t, evidence, "impact", "no recorded lineage is no cone, not an empty one")
}

func TestHoldIncidentOverTheBus(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	bus := event.New()
	startSubscriber(t, bus, w.db, 0)
	holdID := w.hold("orders", nil)

	bus.Publish(w.heldEvent(holdID, "orders"))
	waitForIncidents(t, w.db, 1)

	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", holdID).Update("status", models.DatasetHoldStatusReleased).Error)
	payload, err := json.Marshal(map[string]any{"hold_id": holdID, "release_reason": "clean_run", "released_by": "system"})
	require.NoError(t, err)
	bus.Publish(event.Event{Type: event.TypeDatasetReleased, JobID: w.jobID, Payload: payload})

	require.Eventually(t, func() bool {
		return w.only().Status == models.IncidentStatusClosed
	}, 3*time.Second, 10*time.Millisecond, "dataset_released must be subscribed")
}

// The task succeeded when the hold opened, so its own success events arrive
// next, and a green rerun of a release: manual hold changes nothing either.
func TestHoldIncidentStaysOpenThroughGreenRunsUntilRelease(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(holdID, "orders"))

	green := func(runID uuid.UUID) {
		w.sub.handle(ctx, event.Event{Type: event.TypeTaskSucceeded, JobID: w.jobID, RunID: runID, TaskID: w.taskID})
		w.sub.handle(ctx, event.Event{Type: event.TypeRunCompleted, JobID: w.jobID, RunID: runID})
	}
	green(w.runID)
	require.Equal(t, models.IncidentStatusOpen, w.only().Status, "the producing run's own success")

	rerun, _ := w.greenRun("finance")
	green(rerun)
	require.Equal(t, models.IncidentStatusOpen, w.only().Status, "a green rerun while the hold is active")

	w.release(ctx, holdID)
	closed := w.only()
	require.Equal(t, models.IncidentStatusClosed, closed.Status)
	require.Nil(t, closed.ActiveDedupeKey)
	require.Equal(t, "dataset hold released: manual_ack by user:ada", closed.ResolutionSummary)
}

// A failure incident on the same task keeps its green-run close.
func TestGreenRunStillClosesFailureBesideHoldIncident(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	w.sub.handle(ctx, w.heldEvent(w.hold("orders", nil), "orders"))
	_, _, err := w.sub.store.OpenOrAppend(ctx, OpenParams{JobID: w.jobID, RunID: &w.runID, TaskName: "load", Class: ClassUnknown})
	require.NoError(t, err)

	w.sub.handle(ctx, event.Event{Type: event.TypeTaskSucceeded, JobID: w.jobID, RunID: w.runID, TaskID: w.taskID})

	for _, inc := range w.incidents() {
		if inc.Class == string(ClassDataQualityHold) {
			require.Equal(t, models.IncidentStatusOpen, inc.Status)
		} else {
			require.Equal(t, models.IncidentStatusClosed, inc.Status)
		}
	}
}

func TestNewHoldAfterReleaseOpensItsOwnIncidentInsideCooldown(t *testing.T) {
	w := newHoldWorld(t, "finance", time.Hour)
	ctx := t.Context()
	first := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(first, "orders"))
	w.release(ctx, first)

	second := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(second, "orders"))

	rows := w.incidents()
	require.Len(t, rows, 2, "the cooldown must not swallow a new hold")
	require.NotEqual(t, rows[0].DedupeKey, rows[1].DedupeKey)
	require.Equal(t, models.IncidentStatusClosed, rows[0].Status)
	require.Equal(t, models.IncidentStatusOpen, rows[1].Status)
}

func TestTwoDatasetsHeldByOneStepAreTwoIncidents(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	orders, refunds := w.hold("orders", nil), w.hold("refunds", nil)
	w.sub.handle(ctx, w.heldEvent(orders, "orders"))
	w.sub.handle(ctx, w.heldEvent(refunds, "refunds"))
	require.Len(t, w.incidents(), 2)

	w.release(ctx, orders)

	statuses := map[string]models.IncidentStatus{}
	for _, inc := range w.incidents() {
		var evidence struct {
			Dataset string `json:"dataset"`
		}
		require.NoError(t, json.Unmarshal(inc.Evidence, &evidence))
		statuses[evidence.Dataset] = inc.Status
	}
	require.Equal(t, map[string]models.IncidentStatus{"orders": models.IncidentStatusClosed, "refunds": models.IncidentStatusOpen}, statuses)
}

func TestHoldEventRedeliveryChangesNothing(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	evt := w.heldEvent(holdID, "orders")

	w.sub.handle(ctx, evt)
	w.sub.handle(ctx, evt)
	require.Equal(t, 1, w.only().OccurrenceCount, "a redelivery is not a second breach")

	// Past the hold row's lifetime too: only the incident can say it was opened.
	_, err := w.sub.store.Remediate(ctx, w.only().ID, "closed")
	require.NoError(t, err)
	require.NoError(t, w.db.Delete(&models.DatasetHold{}, "id = ?", holdID).Error)
	w.sub.handle(ctx, evt)
	require.Equal(t, models.IncidentStatusClosed, w.only().Status)
}

// A delayed dataset_held can arrive after its release was handled. An incident
// opened then would never see a release again.
func TestHoldReleasedBeforeItsEventOpensNoIncident(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	holdID := w.hold("orders", nil)
	require.NoError(t, w.db.Model(&models.DatasetHold{}).Where("id = ?", holdID).Update("status", models.DatasetHoldStatusReleased).Error)

	w.sub.handle(t.Context(), w.heldEvent(holdID, "orders"))

	require.Empty(t, w.incidents())
}

func TestHoldEvidenceCollapsesForeignNodes(t *testing.T) {
	// A blank owner reads as "default" to the redactor, so default is the
	// namespace a cone that lost its owners would leak to.
	for _, tenant := range []string{"finance", models.DefaultNamespace} {
		t.Run(tenant, func(t *testing.T) {
			w := newHoldWorld(t, tenant, 0)
			foreignJob := uuid.New()
			cone := storedCone(t,
				lineage.ImpactNode{DatasetName: "own-rollup", JobAlias: "own-job", JobNamespace: tenant, RunNamespace: tenant, Depth: 0},
				lineage.ImpactNode{DatasetName: "secret-foreign", JobAlias: "secret-job", JobID: foreignJob, JobNamespace: "marketing", RunNamespace: "marketing", Depth: 0},
				lineage.ImpactNode{DatasetName: "secret-moved", JobAlias: "secret-moved-job", JobNamespace: tenant, RunNamespace: "marketing", Depth: 1},
			)
			// A cone persisted before owners were recorded.
			legacy, err := json.Marshal(lineage.ImpactResult{RootName: "orders", Downstream: []lineage.ImpactNode{
				{DatasetName: "own-rollup", JobAlias: "own-job", JobNamespace: tenant, RunNamespace: tenant},
				{DatasetName: "secret-legacy", JobAlias: "secret-legacy-job", Depth: 1},
			}})
			require.NoError(t, err)

			for name, impact := range map[string]datatypes.JSON{"orders": cone, "refunds": legacy} {
				w.sub.handle(t.Context(), w.heldEvent(w.hold(name, impact), name))
			}

			rows := w.incidents()
			require.Len(t, rows, 2)
			for _, inc := range rows {
				require.Equal(t, tenant, inc.Namespace)
				require.NotContains(t, string(inc.Evidence), "secret", "a foreign dataset or job name in the evidence")
				require.NotContains(t, string(inc.Evidence), foreignJob.String())
				require.NotContains(t, string(inc.Evidence), "marketing")

				var evidence struct {
					Dataset string `json:"dataset"`
					Impact  struct {
						Downstream []struct {
							DatasetName string `json:"dataset_name"`
							Kind        string `json:"kind"`
						} `json:"downstream"`
						DownstreamTotal int `json:"downstream_total"`
					} `json:"impact"`
				}
				require.NoError(t, json.Unmarshal(inc.Evidence, &evidence))
				if evidence.Dataset == "orders" {
					require.Len(t, evidence.Impact.Downstream, 3)
					require.Equal(t, "own-rollup", evidence.Impact.Downstream[0].DatasetName)
					require.Equal(t, "external", evidence.Impact.Downstream[1].Kind)
					require.Equal(t, "external", evidence.Impact.Downstream[2].Kind)
				} else {
					require.Len(t, evidence.Impact.Downstream, 2)
					require.Equal(t, "external", evidence.Impact.Downstream[0].Kind, "an unknown owner is foreign, even an own job's")
					require.Equal(t, "external", evidence.Impact.Downstream[1].Kind, "an unknown owner is foreign")
				}
			}
		})
	}
}

func TestHoldEvidenceCapsTheCone(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	nodes := make([]lineage.ImpactNode, 0, evidenceImpactCap+5)
	for i := range evidenceImpactCap + 5 {
		nodes = append(nodes, lineage.ImpactNode{DatasetName: fmt.Sprintf("rollup-%d", i), JobNamespace: "finance", RunNamespace: "finance"})
	}
	w.sub.handle(t.Context(), w.heldEvent(w.hold("orders", storedCone(t, nodes...)), "orders"))

	var evidence struct {
		Impact struct {
			Downstream      []any `json:"downstream"`
			DownstreamTotal int   `json:"downstream_total"`
		} `json:"impact"`
	}
	require.NoError(t, json.Unmarshal(w.only().Evidence, &evidence))
	require.Len(t, evidence.Impact.Downstream, evidenceImpactCap)
	require.Equal(t, evidenceImpactCap+5, evidence.Impact.DownstreamTotal)
}

// The allowlist seeds from trusted outputs: the breaching run's own lineage must
// not widen it, and the cone stored on the hold is evidence, not authorization.
func TestHoldAllowlistExcludesTheBreachingRun(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	_, trustedTask := w.greenRun("finance")

	w.lineageRow(w.taskRunID, "poisoned", "output")
	w.lineageRow(trustedTask, "trusted", "output")
	w.consumer("reached-by-trusted", "trusted")
	w.consumer("reached-by-poison", "poisoned")

	cone := storedCone(t, lineage.ImpactNode{DatasetName: "reached-by-poison-out", JobAlias: "reached-by-poison", JobNamespace: "finance", RunNamespace: "finance"})
	w.sub.handle(ctx, w.heldEvent(w.hold("orders", cone), "orders"))

	inc := w.only()
	require.ElementsMatch(t, []string{w.alias, "reached-by-trusted"}, unmarshalAllowlist(inc.AllowedJobs))
	require.Contains(t, string(inc.Evidence), "reached-by-poison", "the cone itself is still evidence")
}

func TestHoldWithoutRunOrTaskOpensOnTheJob(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	payload, err := json.Marshal(map[string]any{"hold_id": holdID, "dataset": "orders", "reason": "min"})
	require.NoError(t, err)

	w.sub.handle(ctx, event.Event{Type: event.TypeDatasetHeld, JobID: w.jobID, Payload: payload})

	inc := w.only()
	require.Nil(t, inc.RunID)
	require.Nil(t, inc.TaskID)
	require.Equal(t, "finance", inc.Namespace)
	require.Equal(t, `dataset "orders" held: min`, inc.LastError)
	w.release(ctx, holdID)
	require.Equal(t, models.IncidentStatusClosed, w.only().Status)
}

// HeldByRunID is a soft reference: a pruned run must not move the incident to
// the default namespace.
func TestHoldWhoseRunIsGoneKeepsTheJobNamespace(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	evt := w.heldEvent(w.hold("orders", nil), "orders")
	require.NoError(t, w.db.Delete(&models.JobRun{}, "id = ?", w.runID).Error)

	w.sub.handle(t.Context(), evt)

	require.Equal(t, "finance", w.only().Namespace)
}

func TestHoldIncidentNeedsDataAssertionsEnabled(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	evt := w.heldEvent(w.hold("orders", nil), "orders")
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
	require.NoError(t, env.Process())

	w.sub.handle(t.Context(), evt)

	require.Empty(t, w.incidents())
}

func TestHoldIncidentIsLeaderGated(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	w.sub.leaderCheck = func(context.Context) (bool, error) { return false, nil }

	w.sub.handle(t.Context(), w.heldEvent(w.hold("orders", nil), "orders"))

	require.Empty(t, w.incidents())
}

// A hold incident names a task that succeeded, so it must not pass for the
// task_failed twin of a later run_failed.
func TestHoldIncidentDoesNotSuppressRunFailure(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	w.sub.handle(ctx, w.heldEvent(w.hold("orders", nil), "orders"))

	w.sub.handle(ctx, event.Event{Type: event.TypeRunFailed, JobID: w.jobID, RunID: w.runID})

	require.Len(t, w.incidents(), 2)
}

func TestReleaseClosesEscalatedHoldIncident(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(holdID, "orders"))
	_, err := w.sub.store.Transition(ctx, w.only().ID, models.IncidentStatusEscalated, "")
	require.NoError(t, err)

	w.release(ctx, holdID)

	closed := w.only()
	require.Equal(t, models.IncidentStatusClosed, closed.Status)
	require.Nil(t, closed.ActiveDedupeKey)
}

func TestHoldIncidentBundleDescribesNoFailure(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	w.sub.handle(ctx, w.heldEvent(w.hold("orders", nil), "orders"))
	inc := w.only()

	bundle, err := BuildBundle(ctx, w.db, inc.ID, nil)
	require.NoError(t, err)

	require.Equal(t, inc.LastError, bundle.Failure.Error)
	require.Empty(t, bundle.Failure.LogTail, "the green instance is not the failure")
	require.Empty(t, bundle.Failure.Result)
	require.Equal(t, string(ClassDataQualityHold), bundle.Classification.Class)
	require.Contains(t, string(bundle.Classification.Evidence), "rowCount")

	_, matched := DefaultRuleSet().Match(FailureClass(inc.Class))
	require.False(t, matched, "no deterministic rule retries bad data")
	require.NotPanics(t, func() { ResolvePlaybook(ctx, w.db, inc.ID, "") })
}

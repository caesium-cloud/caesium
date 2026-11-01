package incident

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// heldRow records an active hold the subscriber never heard about: the row a
// dataset_held consumed by a follower leaves behind.
func (w *holdWorld) heldRow(dataset string, openedAt time.Time) *models.DatasetHold {
	w.t.Helper()
	key := models.DatasetHoldKey("", dataset)
	hold := &models.DatasetHold{
		ID: uuid.New(), Name: dataset, Status: models.DatasetHoldStatusActive, ActiveKey: &key, Reason: "min",
		HeldByJobID: w.jobID, HeldByJobAlias: w.alias, HeldByRunID: &w.runID, LastBreachRunID: &w.runID,
		HeldByTaskID: &w.taskID, HeldByTaskRunID: &w.taskRunID, HeldByStepName: "load",
		Violations:      datatypes.JSON(`[{"dataset":"` + dataset + `","metric":"rowCount","assertion":"min","observed":12,"bound":1000,"message":"rowCount 12 is below min 1000"}]`),
		OccurrenceCount: 1, OpenedAt: openedAt,
	}
	require.NoError(w.t, w.db.Create(hold).Error)
	return hold
}

// releaseUnseen ends the hold the way a release on another node looks from here:
// the row changes and no event arrives.
func (w *holdWorld) releaseUnseen(holdID uuid.UUID) {
	w.t.Helper()
	require.NoError(w.t, w.db.Model(&models.DatasetHold{}).Where("id = ?", holdID).Updates(map[string]any{
		"status": models.DatasetHoldStatusReleased, "active_key": nil, "release_reason": "clean_run", "released_by": "system",
	}).Error)
}

func TestReconcileClosesIncidentWhoseReleaseNeverArrived(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(holdID, "orders"))
	w.releaseUnseen(holdID)
	require.Equal(t, models.IncidentStatusOpen, w.only().Status, "no event has told the subscriber")

	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))

	closed := w.only()
	require.Equal(t, models.IncidentStatusClosed, closed.Status)
	require.Nil(t, closed.ActiveDedupeKey)
	require.Equal(t, "dataset hold released: clean_run by system", closed.ResolutionSummary)
}

func TestReconcileClosesEscalatedIncidentWhoseHoldEnded(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(holdID, "orders"))
	_, err := w.sub.store.Transition(ctx, w.only().ID, models.IncidentStatusEscalated, "")
	require.NoError(t, err)
	w.releaseUnseen(holdID)

	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))

	require.Equal(t, models.IncidentStatusClosed, w.only().Status)
}

func TestReconcileClosesIncidentWhoseHoldIsGone(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(holdID, "orders"))
	require.NoError(t, w.db.Delete(&models.DatasetHold{}, "id = ?", holdID).Error)

	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))

	require.Equal(t, models.IncidentStatusClosed, w.only().Status)
}

func TestReconcileLeavesActiveHoldsIncidentOpen(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	w.sub.handle(ctx, w.heldEvent(w.hold("orders", nil), "orders"))
	inc := w.only()

	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))
	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))

	got := w.only()
	require.Equal(t, inc.ID, got.ID)
	require.Equal(t, models.IncidentStatusOpen, got.Status)
	require.Equal(t, 1, got.OccurrenceCount)
}

func TestReconcileOpensIncidentWhoseHeldEventNeverArrived(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	cone := storedCone(t, coneNode("finance", "own-rollup"), coneNode("marketing", "secret-foreign"))
	hold := w.heldRow("orders", time.Now().UTC().Add(-time.Hour))
	require.NoError(t, w.db.Model(hold).Update("impact", cone).Error)

	require.NoError(t, w.sub.ReconcileHolds(ctx, time.Minute))
	require.NoError(t, w.sub.ReconcileHolds(ctx, time.Minute))

	inc := w.only()
	require.Equal(t, string(ClassDataQualityHold), inc.Class)
	require.Equal(t, models.IncidentStatusOpen, inc.Status)
	require.Equal(t, 1, inc.OccurrenceCount, "a second sweep changes nothing")
	require.Equal(t, "finance", inc.Namespace)
	require.Equal(t, w.runID, *inc.RunID)
	require.Equal(t, w.taskID, *inc.TaskID)
	require.Equal(t, "load", inc.TaskName)
	require.Contains(t, inc.LastError, "rowCount 12 is below min 1000")

	var evidence map[string]any
	require.NoError(t, json.Unmarshal(inc.Evidence, &evidence))
	require.Equal(t, hold.ID.String(), evidence["hold_id"])
	require.Equal(t, w.taskRunID.String(), evidence["task_run_id"])
	require.EqualValues(t, 2, evidence["impacted_datasets"])
	require.Contains(t, string(inc.Evidence), "own-rollup")
	require.NotContains(t, string(inc.Evidence), "secret-foreign")

	// The event arriving after all is the ordinary redelivery.
	w.sub.handle(ctx, w.heldEvent(hold.ID, "orders"))
	require.Equal(t, 1, w.only().OccurrenceCount)
}

func TestReconcileLeavesAFreshHoldToItsEvent(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	w.heldRow("orders", time.Now().UTC())

	require.NoError(t, w.sub.ReconcileHolds(t.Context(), time.Minute))

	require.Empty(t, w.incidents())
}

func TestReconcileDoesNotReopenAClosedHoldIncident(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	hold := w.heldRow("orders", time.Now().UTC().Add(-time.Hour))
	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))
	_, err := w.sub.store.Remediate(ctx, w.only().ID, "closed by hand")
	require.NoError(t, err)

	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))

	require.Equal(t, hold.ID.String(), mustHoldID(t, w.only()))
	require.Equal(t, models.IncidentStatusClosed, w.only().Status)
}

func TestReconcileIsLeaderGated(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	holdID := w.hold("orders", nil)
	w.sub.handle(ctx, w.heldEvent(holdID, "orders"))
	w.releaseUnseen(holdID)
	w.heldRow("refunds", time.Now().UTC().Add(-time.Hour))

	w.sub.leaderCheck = func(context.Context) (bool, error) { return false, nil }
	require.NoError(t, w.sub.ReconcileHolds(ctx, 0))

	require.Equal(t, models.IncidentStatusOpen, w.only().Status)
}

func TestReconcileOpensNothingWithDataAssertionsOff(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	w.heldRow("orders", time.Now().UTC().Add(-time.Hour))
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
	require.NoError(t, env.Process())

	require.NoError(t, w.sub.ReconcileHolds(t.Context(), 0))

	require.Empty(t, w.incidents())
}

// A later breach's run is as untrusted as the opening run: the sweep can open
// long after both, when a poisoned output has already reached the lineage graph.
func TestReconcileAllowlistExcludesEveryKnownBreachingRun(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	_, trustedTask := w.greenRun("finance")
	laterRun, laterTask := w.greenRun("finance")
	w.lineageRow(w.taskRunID, "opening-poison", "output")
	w.lineageRow(laterTask, "later-poison", "output")
	w.lineageRow(trustedTask, "trusted", "output")
	w.consumer("reached-by-opening", "opening-poison")
	w.consumer("reached-by-later", "later-poison")
	w.consumer("reached-by-trusted", "trusted")
	hold := w.heldRow("orders", time.Now().UTC().Add(-time.Hour))
	require.NoError(t, w.db.Model(hold).Update("last_breach_run_id", laterRun).Error)

	require.NoError(t, w.sub.ReconcileHolds(t.Context(), 0))

	require.ElementsMatch(t, []string{w.alias, "reached-by-trusted"}, unmarshalAllowlist(w.only().AllowedJobs))
}

// greptile on #710: an operator rejecting the last approval between a status
// read and the close left a released hold's incident escalated for good.
func TestCloseHoldIncidentDecidesFromTheStoredStatus(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	ctx := t.Context()
	w.sub.handle(ctx, w.heldEvent(w.hold("orders", nil), "orders"))
	stale := w.only()
	require.Equal(t, models.IncidentStatusOpen, stale.Status)
	_, err := w.sub.store.Transition(ctx, stale.ID, models.IncidentStatusEscalated, "")
	require.NoError(t, err)

	w.sub.closeHoldIncident(ctx, &stale, "dataset hold released: manual_ack by user:ada")

	closed := w.only()
	require.Equal(t, models.IncidentStatusClosed, closed.Status)
	require.Equal(t, "dataset hold released: manual_ack by user:ada", closed.ResolutionSummary)
}

func TestHoldIDFromDedupeKey(t *testing.T) {
	id := uuid.New()
	got, ok := holdIDFromDedupeKey(DedupeKey("finance", uuid.New(), "a|hold:not-this", ClassDataQualityHold) + "|" + holdSubject(id))
	require.True(t, ok)
	require.Equal(t, id, got)

	_, ok = holdIDFromDedupeKey(DedupeKey("finance", uuid.New(), "load", ClassDataQualityHold))
	require.False(t, ok)
}

// A pre-upgrade dataset_held still pending at upgrade carries a baseline computed
// across tenants. Evidence is served as stored, so it must never be stored.
func TestLegacyBaselineNeverReachesEvidence(t *testing.T) {
	const (
		median  = 31337
		bound   = 15668.5
		message = "observed 10 deviates 31327 from the baseline median 31337 (5 sample(s)), more than the declared 50%"
	)
	legacy := []map[string]any{{
		"dataset": "orders", "metric": "rowCount", "assertion": "deltaFromBaseline", "observed": 10, "bound": bound,
		"delta_from_baseline": "50%", "baseline_median": median, "baseline_samples": 5, "message": message,
	}, {
		"dataset": "orders", "metric": "rowCount", "assertion": "min", "observed": 10, "bound": 1000,
		"message": "rowCount 10 is below min 1000",
	}}

	check := func(t *testing.T, w *holdWorld) {
		t.Helper()
		inc := w.only()
		for _, text := range []string{string(inc.Evidence), inc.LastError} {
			require.NotContains(t, text, "31337")
			require.NotContains(t, text, "15668")
			require.NotContains(t, text, "baseline_median")
			require.NotContains(t, text, "baseline_samples")
		}
		require.Contains(t, inc.LastError, "baseline evidence withheld")
		var evidence struct {
			Violations []map[string]any `json:"violations"`
		}
		require.NoError(t, json.Unmarshal(inc.Evidence, &evidence))
		require.Len(t, evidence.Violations, 2)
		require.Equal(t, "deltaFromBaseline", evidence.Violations[0]["assertion"])
		require.EqualValues(t, 10, evidence.Violations[0]["observed"], "only the baseline evidence goes")
		require.NotContains(t, evidence.Violations[0], "bound", "the bound derives from the baseline")
		require.EqualValues(t, 1000, evidence.Violations[1]["bound"], "an absolute bound owes nothing to a baseline")

		bundle, err := BuildBundle(t.Context(), w.db, inc.ID, nil)
		require.NoError(t, err)
		require.NotContains(t, string(bundle.Classification.Evidence), "31337")
		require.NotContains(t, bundle.Failure.Error, "31337")
	}

	t.Run("event", func(t *testing.T) {
		w := newHoldWorld(t, "finance", 0)
		holdID := w.hold("orders", nil)
		evt := w.heldEvent(holdID, "orders")
		var payload map[string]any
		require.NoError(t, json.Unmarshal(evt.Payload, &payload))
		payload["violations"] = legacy
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		evt.Payload = raw

		w.sub.handle(t.Context(), evt)

		check(t, w)
	})

	t.Run("hold row", func(t *testing.T) {
		w := newHoldWorld(t, "finance", 0)
		hold := w.heldRow("orders", time.Now().UTC().Add(-time.Hour))
		raw, err := json.Marshal(legacy)
		require.NoError(t, err)
		require.NoError(t, w.db.Model(hold).Update("violations", datatypes.JSON(raw)).Error)

		require.NoError(t, w.sub.ReconcileHolds(t.Context(), 0))

		check(t, w)
	})
}

func TestOwnedBaselineStaysInEvidence(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	evt := w.heldEvent(w.hold("orders", nil), "orders")

	w.sub.handle(t.Context(), evt)

	require.Contains(t, string(w.only().Evidence), `"baseline_median":5000`)
}

func TestHoldEventWithUndecodableVerdictsStoresNone(t *testing.T) {
	w := newHoldWorld(t, "finance", 0)
	holdID := w.hold("orders", nil)
	w.sub.handle(t.Context(), event.Event{
		Type: event.TypeDatasetHeld, JobID: w.jobID, RunID: w.runID,
		Payload: []byte(`{"hold_id":"` + holdID.String() + `","dataset":"orders","reason":"min","violations":{"baseline_median":31337}}`),
	})

	inc := w.only()
	require.NotContains(t, string(inc.Evidence), "31337")
	require.Equal(t, `dataset "orders" held: min`, inc.LastError)
}

func mustHoldID(t *testing.T, inc models.Incident) string {
	t.Helper()
	id, ok := holdIDFromDedupeKey(inc.DedupeKey)
	require.True(t, ok)
	return id.String()
}

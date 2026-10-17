package event

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestTriggerEventsOwnedDeliveriesOnly: a trigger's event history shows a
// namespace-confined caller only deliveries whose every started run is in its
// namespaces (no skip, no error), filtered before paging, so a delivery made
// before the job moved (with its private payload) stays with the old owner
// (identity-and-access B4).
func TestTriggerEventsOwnedDeliveriesOnly(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	now := time.Now().UTC()
	run := func(namespace string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.JobRun{ID: id, JobID: uuid.New(), Namespace: namespace, Status: "succeeded", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	trigger := uuid.New()
	deliver := func(name string, age time.Duration, match models.EventTriggerMatch) {
		id := uuid.New()
		require.NoError(t, db.Create(&models.IngestedEvent{ID: id, Type: name, Data: datatypes.JSON(`{"private":"` + name + `"}`), CreatedAt: now.Add(-age)}).Error)
		match.ID, match.EventID, match.TriggerID, match.MatchedAt = uuid.New(), id, trigger, now.Add(-age)
		require.NoError(t, db.Create(&match).Error)
	}
	runs := func(ids ...uuid.UUID) datatypes.JSON {
		values := make([]string, 0, len(ids))
		for _, id := range ids {
			values = append(values, id.String())
		}
		data, err := json.Marshal(values)
		require.NoError(t, err)
		return datatypes.JSON(data)
	}
	// Newest first: the post-move finance delivery, then the pre-move
	// marketing ones.
	deliver("finance-after-move", time.Minute, models.EventTriggerMatch{RunsStarted: runs(run("finance"))})
	deliver("marketing-before-move", 2*time.Minute, models.EventTriggerMatch{RunsStarted: runs(run("marketing"))})
	deliver("marketing-skipped", 3*time.Minute, models.EventTriggerMatch{Skipped: true, SkipReason: "paused"})
	deliver("marketing-older", 4*time.Minute, models.EventTriggerMatch{RunsStarted: runs(run("marketing"), run("marketing"))})

	types := func(ctx context.Context, limit uint64) []string {
		events, err := NewWithDatabase(ctx, db).ListTriggerEvents(trigger, &ListRequest{Limit: limit})
		require.NoError(t, err)
		out := make([]string, 0, len(events))
		for _, evt := range events {
			out = append(out, evt.Type)
		}
		return out
	}
	require.Equal(t, []string{"finance-after-move", "marketing-before-move", "marketing-skipped", "marketing-older"}, types(context.Background(), 10))

	finance := authz.WithAllowedNamespaces(context.Background(), []string{"finance"})
	require.Equal(t, []string{"finance-after-move"}, types(finance, 10))
	require.True(t, authz.Filtered(finance))
	marketing := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	require.Equal(t, []string{"marketing-before-move", "marketing-older"}, types(marketing, 10))
	require.Equal(t, []string{"marketing-before-move"}, types(marketing, 1), "filtered before paging")

	// The ingested-events list applies the same ownership to every delivery.
	ingested, err := NewWithDatabase(marketing, db).ListIngested(&ListRequest{Limit: 10})
	require.NoError(t, err)
	require.Len(t, ingested, 2)

	// A lifecycle event carries its originating run's payload: a marketing
	// delivery of a finance run's completion stays hidden from marketing.
	financeSource := run("finance")
	lifecycle := uuid.New()
	require.NoError(t, db.Create(&models.IngestedEvent{ID: lifecycle, Type: "run_completed", Source: LifecycleEventSource,
		Data: datatypes.JSON(`{"run_id":"` + financeSource.String() + `","secret":"finance-completion","job_labels":{"owner":"label-secret"},"tasks":[{"job_labels":{"owner":"label-secret"}}]}`), CreatedAt: now}).Error)
	require.NoError(t, db.Create(&models.EventTriggerMatch{ID: uuid.New(), EventID: lifecycle, TriggerID: trigger, MatchedAt: now,
		RunsStarted: runs(run("marketing"))}).Error)
	require.NotContains(t, types(marketing, 10), "run_completed")
	ingested, err = NewWithDatabase(marketing, db).ListIngested(&ListRequest{Limit: 10})
	require.NoError(t, err)
	require.Len(t, ingested, 2, "the finance completion is not marketing's")
	both := authz.WithAllowedNamespaces(context.Background(), []string{"finance", "marketing"})
	require.Contains(t, types(both, 10), "run_completed")
	ingested, err = NewWithDatabase(both, db).ListIngested(&ListRequest{Limit: 10, Source: LifecycleEventSource})
	require.NoError(t, err)
	require.Len(t, ingested, 1)
	require.Contains(t, string(ingested[0].Data), "finance-completion")
	require.NotContains(t, string(ingested[0].Data), "label-secret", "job labels never reach a confined caller")
	events, err := NewWithDatabase(both, db).ListTriggerEvents(trigger, &ListRequest{Limit: 10, Source: LifecycleEventSource})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.NotContains(t, string(events[0].Data), "label-secret")
	unrestricted, err := NewWithDatabase(context.Background(), db).ListIngested(&ListRequest{Limit: 10, Source: LifecycleEventSource})
	require.NoError(t, err)
	require.Contains(t, string(unrestricted[0].Data), "label-secret", "an unrestricted caller's data is unchanged")
}

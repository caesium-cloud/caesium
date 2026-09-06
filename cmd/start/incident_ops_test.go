package start

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/notification"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// recordingBus captures published events so a test can assert that an escalation
// was actually DELIVERED onto the stream the notification subscriber reads.
type recordingBus struct {
	published []event.Event
}

func (b *recordingBus) Publish(e event.Event) { b.published = append(b.published, e) }

func (b *recordingBus) Subscribe(context.Context, event.Filter) (<-chan event.Event, error) {
	return nil, nil
}

func seedEscalationIncident(t *testing.T, db *gorm.DB) (uuid.UUID, string) {
	t.Helper()
	now := time.Now().UTC()

	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "vendor-ingest",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	incidentID := uuid.New()
	require.NoError(t, db.Create(&models.Incident{
		ID:        incidentID,
		JobID:     job.ID,
		TaskName:  "extract",
		Class:     "unknown",
		Status:    models.IncidentStatusTriaging,
		DedupeKey: "vendor-ingest:extract:unknown",
		OpenedAt:  now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return incidentID, job.Alias
}

// TestEscalatePublishesNotifiableEvent is the delivery guarantee. An escalation
// that only writes an AgentAction row and a log line contacts nobody, which is
// the one outcome an escalation cannot have — and the git-synced jobdef-patch
// route degrades to exactly this call, so a patch a human approved would be
// recorded `route=escalate` with the diff attached and no human ever told.
func TestEscalatePublishesNotifiableEvent(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	incidentID, alias := seedEscalationIncident(t, db)

	bus := &recordingBus{}
	store := event.NewStore(db)
	ops := newIncidentActionOps(db, bus, store)

	const summary = "approved jobdef patch cannot be applied to a git-synced job\n{\"alias\":\"vendor-ingest\"}"
	require.NoError(t, ops.Escalate(ctx, incidentID, "pagerduty-oncall", summary))

	// Published onto the bus the notification subscriber consumes...
	require.Len(t, bus.published, 1)
	evt := bus.published[0]
	require.Equal(t, event.TypeIncidentEscalated, evt.Type)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(evt.Payload, &payload))
	require.Equal(t, incidentID.String(), payload["incident_id"])
	require.Equal(t, "pagerduty-oncall", payload["channel"])
	require.Equal(t, summary, payload["summary"], "the rendered diff must reach the recipient")
	require.Equal(t, alias, payload["job_alias"], "job_alias feeds NotificationPolicy's filters")
	require.Equal(t, "extract", payload["task_name"])

	// ...and PERSISTED, so an escalation raised while nothing was listening is
	// still queryable from /v1/events.
	persisted, err := store.ListSince(ctx, 0, 100, event.Filter{Types: []event.Type{event.TypeIncidentEscalated}})
	require.NoError(t, err)
	require.Len(t, persisted, 1)
	require.Equal(t, evt.JobID, persisted[0].JobID)
}

// TestEscalateIsRoutableByNotificationPolicy proves the published type is one an
// operator can actually route: notification policies validate their event_types
// against notifiableTypes, so an unlisted type is unroutable no matter how
// faithfully it is published.
func TestEscalateIsRoutableByNotificationPolicy(t *testing.T) {
	require.Contains(t, notification.ValidEventTypes(), event.TypeIncidentEscalated,
		"incident_escalated must be selectable as a notification policy event type")
}

// TestEscalateWithoutEventSinkFails: with no way to reach anyone, Escalate must
// refuse. dispatch records the action `executed` only when this returns nil, so
// returning success here would record a page nobody received.
func TestEscalateWithoutEventSinkFails(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	incidentID, _ := seedEscalationIncident(t, db)
	ops := newIncidentActionOps(db, nil, nil)

	require.Error(t, ops.Escalate(context.Background(), incidentID, "pagerduty-oncall", "summary"),
		"an escalation with no delivery path must not be recorded as executed")
}

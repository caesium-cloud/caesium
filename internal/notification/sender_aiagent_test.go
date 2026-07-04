package notification

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAIAgentSenderOpensIncident(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	sender := NewAIAgentSender(db, nil) // nil leader check = always act
	jobID := uuid.New()

	err := sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
		EventType: event.TypeTaskFailed,
		JobID:     jobID,
		Error:     "authentication failed: 401 unauthorized",
	})
	require.NoError(t, err)

	var incidents []models.Incident
	require.NoError(t, db.Find(&incidents).Error)
	require.Len(t, incidents, 1)
	require.Equal(t, jobID, incidents[0].JobID)
	// The classifier maps the 401 auth log pattern to auth_failure.
	require.Equal(t, "auth_failure", incidents[0].Class)

	// A second matched event for the same key folds in as an occurrence (the
	// atomic conditional insert prevents a twin), not a new incident.
	require.NoError(t, sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
		EventType: event.TypeTaskFailed,
		JobID:     jobID,
		Error:     "authentication failed: 401 unauthorized",
	}))
	require.NoError(t, db.Find(&incidents).Error)
	require.Len(t, incidents, 1)
	require.Equal(t, 2, incidents[0].OccurrenceCount)
}

func TestAIAgentSenderLeaderGated(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	// A non-leader node must NOT open incidents — the leader gate avoids N-node
	// duplicate work (the store's atomic insert is the correctness backstop).
	sender := NewAIAgentSender(db, func(context.Context) (bool, error) { return false, nil })

	err := sender.Send(context.Background(), models.NotificationChannel{Name: "triage"}, Payload{
		EventType: event.TypeTaskFailed,
		JobID:     uuid.New(),
		Error:     "authentication failed: 401",
	})
	require.NoError(t, err)

	var count int64
	require.NoError(t, db.Model(&models.Incident{}).Count(&count).Error)
	require.Equal(t, int64(0), count)
}

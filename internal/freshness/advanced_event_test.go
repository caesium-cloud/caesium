package freshness

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type advancedEventRecorder struct{ events []event.Event }

func (r *advancedEventRecorder) Publish(evt event.Event) { r.events = append(r.events, evt) }
func (*advancedEventRecorder) Subscribe(context.Context, event.Filter) (<-chan event.Event, error) {
	return nil, nil
}

// Caller-level characterization pins the identity difference before moving
// shared construction into the package helper.
func TestDatasetAdvancedPublisherCallerIdentities(t *testing.T) {
	bus := &advancedEventRecorder{}
	namespace := " team "
	jobID, runID := uuid.New(), uuid.New()
	before := time.Now().UTC()
	(&ArrivalObserver{bus: bus}).publishDatasetAdvanced(&namespace, "orders")
	(&Capturer{bus: bus}).publishDatasetAdvanced(&namespace, "orders", jobID, runID)
	after := time.Now().UTC()
	require.Len(t, bus.events, 2)
	for _, evt := range bus.events {
		require.Equal(t, event.TypeDatasetAdvanced, evt.Type)
		require.Same(t, time.UTC, evt.Timestamp.Location())
		require.False(t, evt.Timestamp.Before(before))
		require.False(t, evt.Timestamp.After(after))
		var payload map[string]string
		require.NoError(t, json.Unmarshal(evt.Payload, &payload))
		require.Equal(t, map[string]string{"namespace": "team", "name": "orders"}, payload)
	}
	require.Equal(t, uuid.Nil, bus.events[0].JobID)
	require.Equal(t, uuid.Nil, bus.events[0].RunID)
	require.Equal(t, jobID, bus.events[1].JobID)
	require.Equal(t, runID, bus.events[1].RunID)
	(&ArrivalObserver{}).publishDatasetAdvanced(nil, "orders")
	(&Capturer{}).publishDatasetAdvanced(nil, "orders", jobID, runID)
	require.Len(t, bus.events, 2)
}

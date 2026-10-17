package event

import (
	"encoding/json"
	"testing"

	eventsvc "github.com/caesium-cloud/caesium/api/rest/service/event"
	eventstore "github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestLifecycleIngestedEventCarriesProvenance pins what the ingested-event
// ownership rule relies on (api/rest/service/event provenanceOwnedSQL,
// identity-and-access B4): an event the lifecycle bridge derives from a run
// is stamped with the lifecycle source and names its originating run under
// "run_id", so a confined caller sees it only when it may read that run.
func TestLifecycleIngestedEventCarriesProvenance(t *testing.T) {
	router := NewRouter(openEventRouterTestDB(t))
	runID, jobID := uuid.New(), uuid.New()
	ingested, err := router.lifecycleIngestedEvent(t.Context(), eventstore.Event{
		Type: eventstore.TypeRunCompleted, JobID: jobID, RunID: runID,
		Payload: json.RawMessage(`{"job_alias":"source-job","status":"succeeded"}`),
	})
	require.NoError(t, err)
	require.Equal(t, eventsvc.LifecycleEventSource, ingested.Source)
	var data map[string]any
	require.NoError(t, json.Unmarshal(ingested.Data, &data))
	require.Equal(t, runID.String(), data["run_id"])
}

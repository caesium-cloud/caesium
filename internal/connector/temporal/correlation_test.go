package temporal

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
)

func correlationService() *fakeService {
	return &fakeService{describe: &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: "workflow", RunId: "run"}, Type: &commonpb.WorkflowType{Name: "publication"}}},
		history: &workflowservice.GetWorkflowExecutionHistoryResponse{History: &historypb.History{Events: []*historypb.HistoryEvent{
			{EventId: 4, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED, Attributes: &historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{ActivityId: "activity", ActivityType: &commonpb.ActivityType{Name: "caesium.start"}}}},
			{EventId: 5, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED, Attributes: &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{ScheduledEventId: 4, Attempt: 2}}},
		}}}}
}
func correlationBindingFixture() connector.Binding {
	return connector.Binding{Name: "publication", Version: "1", ActivityAllowlist: []connector.ActivityJob{{ActivityType: "caesium.start", Jobs: []string{"job"}}}}
}
func TestCorrelationActivitiesDerivesPinnedIdentityWithoutPayload(t *testing.T) {
	fake := correlationService()
	observer, err := NewObserver(fake, "default", "", 0, 0, 0)
	require.NoError(t, err)
	page, err := observer.CorrelationActivities(context.Background(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Len(t, page.Activities, 1)
	source := page.Activities[0]
	require.Equal(t, "run/activity", source.IdempotencyKey)
	require.Equal(t, "workflow", source.Execution.Coordinates["workflow_id"])
	require.Equal(t, "run", source.Execution.Coordinates["run_id"])
	require.EqualValues(t, 5, source.SourceEventID)
	require.EqualValues(t, 4, source.ScheduledEventID)
	require.EqualValues(t, 2, source.Attempt)
	require.Nil(t, source.Hint)
	require.Equal(t, describeCall{"workflow", "run"}, fake.describes[0])
	require.Len(t, fake.histories, 1)
}
func TestCorrelationActivitiesRejectsUnpinnedOrMismatchedSource(t *testing.T) {
	for _, field := range []string{"empty_run", "wrong_run", "wrong_workflow", "wrong_type", "missing_type"} {
		t.Run(field, func(t *testing.T) {
			fake := correlationService()
			runID := "run"
			switch field {
			case "empty_run":
				runID = ""
			case "wrong_run":
				fake.describe.WorkflowExecutionInfo.Execution.RunId = "other"
			case "wrong_workflow":
				fake.describe.WorkflowExecutionInfo.Execution.WorkflowId = "other"
			case "wrong_type":
				fake.describe.WorkflowExecutionInfo.Type.Name = "other"
			case "missing_type":
				fake.describe.WorkflowExecutionInfo.Type = nil
			}
			observer, err := NewObserver(fake, "default", "", 0, 0, 0)
			require.NoError(t, err)
			_, err = observer.CorrelationActivities(t.Context(), "primary", "workflow", runID, correlationBindingFixture(), nil, 100)
			require.ErrorIs(t, err, connector.ErrUnverifiedCorrelation)
			require.Empty(t, fake.histories)
		})
	}
}
func TestCorrelationActivitiesPreservesScheduleEvidenceAndOmitsSplitIdentity(t *testing.T) {
	fake := correlationService()
	fake.history.History.Events = fake.history.History.Events[:1]
	observer, err := NewObserver(fake, "default", "", 0, 0, 0)
	require.NoError(t, err)
	page, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Len(t, page.Activities, 1)
	require.EqualValues(t, 4, page.Activities[0].SourceEventID)
	fake.history.History.Events = correlationService().history.History.Events[1:]
	page, err = observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Empty(t, page.Activities, "split-page started event cannot invent activity identity")
}
func TestCorrelationActivitiesExtractsOnlyFrozenHintAndBoundsEvidence(t *testing.T) {
	fake := correlationService()
	hint := connector.CorrelationEnvelope{Version: "v1", JobID: "job-uuid", IdempotencyKey: "run/activity", Outcome: "created", RunID: "run-uuid"}
	raw, err := json.Marshal(hint)
	require.NoError(t, err)
	completed := &historypb.HistoryEvent{EventId: 6, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED, Attributes: &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{ScheduledEventId: 4, StartedEventId: 5, Result: &commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: raw}}}}}}
	fake.history.History.Events = append(fake.history.History.Events, completed)
	observer, err := NewObserver(fake, "default", "", 0, 0, 0)
	require.NoError(t, err)
	page, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Equal(t, &hint, page.Activities[0].Hint)
	require.EqualValues(t, 6, page.Activities[0].SourceEventID)
	payload := completed.GetActivityTaskCompletedEventAttributes().GetResult().Payloads[0]
	payload.Metadata["encryption-key-id"] = []byte("custom-codec-canary")
	page, err = observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Nil(t, page.Activities[0].Hint)
	require.Equal(t, "run/activity", page.Activities[0].IdempotencyKey)
	customJSON, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(customJSON), "job-uuid")
	require.NotContains(t, string(customJSON), "custom-codec-canary")
	delete(payload.Metadata, "encryption-key-id")
	payload.Metadata["encoding"] = []byte("binary/custom")
	payload.Data = []byte("opaque-secret")
	page, err = observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Nil(t, page.Activities[0].Hint)
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "opaque-secret")
	small, err := NewObserver(fake, "default", "", 0, 0, 100)
	require.NoError(t, err)
	page, err = small.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.ErrorIs(t, err, connector.ErrPayloadTooLarge)
}
func TestCorrelationHintRefusesMalformedOrAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{`{"version":"v1"}`, `{"version":"v1","job_id":"j","idempotency_key":"k","outcome":"created","run_id":null}`, `{"version":"v1","job_id":"wrong","job_id":"j","idempotency_key":"k","outcome":"created"}`, `{"version":"v1","job_id":"j","idempotency_key":"k","outcome":"created","secret":"x"}`, `{"version":"v1","job_id":"j","idempotency_key":"k","outcome":"created"} {}`} {
		_, err := correlationHint(&commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: []byte(raw)}}})
		require.ErrorIs(t, err, connector.ErrUnverifiedCorrelation)
	}
}

func TestCorrelationActivitiesBoundsEmptyAndUnrecognizedPageCursors(t *testing.T) {
	for _, kind := range []string{"empty", "unrecognized"} {
		t.Run(kind, func(t *testing.T) {
			fake := correlationService()
			if kind == "empty" {
				fake.history.History.Events = nil
			} else {
				fake.history.History.Events[0].GetActivityTaskScheduledEventAttributes().ActivityType.Name = "unrelated"
			}
			fake.history.NextPageToken = bytes.Repeat([]byte("cursor"), 100)
			observer, err := NewObserver(fake, "default", "", 0, 0, 100)
			require.NoError(t, err)
			_, err = observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
			require.ErrorIs(t, err, connector.ErrPayloadTooLarge, "an empty/unrecognized page must not return an oversized cursor")
		})
	}
	recognized := correlationService()
	recognized.history.NextPageToken = bytes.Repeat([]byte("cursor"), 1000)
	bounded, err := NewObserver(recognized, "default", "", 0, 0, 1024)
	require.NoError(t, err)
	page, err := bounded.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Len(t, page.Activities, 1)
	require.True(t, page.Incomplete)
	require.Empty(t, page.NextPageToken)
	encoded, err := json.Marshal(page)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 1024)
	observer, err := NewObserver(correlationService(), "default", "", 0, 0, 1)
	require.NoError(t, err)
	_, err = observer.boundCorrelationPage(CorrelationPage{})
	require.ErrorIs(t, err, connector.ErrPayloadTooLarge, "even the base response must fit")
}

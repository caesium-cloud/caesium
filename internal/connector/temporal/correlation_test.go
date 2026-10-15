package temporal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/correlation"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
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
			require.ErrorIs(t, err, correlation.ErrUnverifiedCorrelation)
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
		require.ErrorIs(t, err, correlation.ErrUnverifiedCorrelation)
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

// Admissions are written by the real store path, not hand-seeded expected rows.
// This remains component evidence; public connector wiring is C1's future gate.
func correlationAdmissionFixture(t *testing.T) (*correlation.Correlator, *correlation.Store, models.Job, run.StartResult) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	trigger := models.Trigger{ID: uuid.New(), Alias: "correlation-review-trigger", Type: models.TriggerTypeCron, Configuration: `{"cron":"0 0 1 1 *","timezone":"UTC"}`}
	require.NoError(t, db.Create(&trigger).Error)
	job := models.Job{ID: uuid.New(), Alias: "job", TriggerID: trigger.ID}
	require.NoError(t, db.Create(&job).Error)
	runs := run.NewStore(db)
	started, err := runs.StartWithResult(t.Context(), job.ID, nil, run.WithStartParams(map[string]string{"request": "not reconstructed"}), run.WithStartIdempotencyKey("run/activity"))
	require.NoError(t, err)
	ref, err := connector.NewExecutionReference("primary", map[string]string{"workflow_id": "workflow", "run_id": "run"})
	require.NoError(t, err)
	_, err = connector.NewStore(db).RecordSnapshot(t.Context(), connector.Observation{Execution: ref, Generation: 1, SourceEventID: "4", NativeStatus: "running", DisplayStatus: connector.DisplayRunning, Availability: connector.AvailabilityAvailable, Completeness: connector.CompletenessComplete, SourceKind: connector.SourceDirect, ObservedAt: time.Now().UTC()})
	require.NoError(t, err)
	catalog := correlation.NewStore(db)
	return correlation.NewCorrelator(runs, catalog), catalog, job, started
}

func correlationPendingHint(t *testing.T, hint connector.CorrelationEnvelope) *commonpb.Payloads {
	t.Helper()
	raw, err := json.Marshal(hint)
	require.NoError(t, err)
	return &commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: raw}}}
}

func TestCorrelationActivitiesRetainsMatchedPendingAfterUnrelatedActivities(t *testing.T) {
	for _, field := range []string{"matching", "wrong_job", "wrong_key", "stale_queued", "wrong_queue", "wrong_run"} {
		t.Run(field, func(t *testing.T) {
			verifier, catalog, job, started := correlationAdmissionFixture(t)
			hint := connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: "run/activity", Outcome: "created", RunID: started.RunID.String()}
			switch field {
			case "wrong_job":
				hint.JobID = uuid.NewString()
			case "wrong_key":
				hint.IdempotencyKey = "other/activity"
			case "stale_queued":
				hint.Outcome = "queued"
				hint.RunID = ""
				hint.QueueID = uuid.NewString()
			case "wrong_queue":
				hint.QueueID = uuid.NewString()
			case "wrong_run":
				hint.RunID = uuid.NewString()
			}
			fake := correlationService()
			fake.history.History.Events = fake.history.History.Events[:1]
			for i := range connector.MaxPageEntries {
				fake.describe.PendingActivities = append(fake.describe.PendingActivities, &workflowpb.PendingActivityInfo{ActivityId: fmt.Sprintf("unrelated-%d", i), ActivityType: &commonpb.ActivityType{Name: "unrelated"}, Attempt: 9, HeartbeatDetails: &commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("json/plain")}, Data: []byte("unrelated-payload-canary")}}}})
			}
			fake.describe.PendingActivities = append(fake.describe.PendingActivities, &workflowpb.PendingActivityInfo{ActivityId: "activity", ActivityType: &commonpb.ActivityType{Name: "caesium.start"}, Attempt: 3, HeartbeatDetails: correlationPendingHint(t, hint)})
			observer, err := NewObserver(fake, "default", "", 0, 0, 0)
			require.NoError(t, err)
			page, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
			require.NoError(t, err)
			require.Len(t, page.Activities, 1)
			require.False(t, page.Incomplete)
			source := page.Activities[0]
			require.EqualValues(t, 3, source.Attempt)
			require.Equal(t, &hint, source.Hint)
			encoded, err := json.Marshal(page)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "unrelated-payload-canary")
			require.LessOrEqual(t, len(encoded), connector.MaxPageMetadataBytes)
			_, err = verifier.Verify(t.Context(), correlationBindingFixture(), source)
			if field == "matching" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, correlation.ErrUnverifiedCorrelation)
			}
			links, err := catalog.Correlations(t.Context(), source.Execution)
			require.NoError(t, err)
			if field == "matching" {
				require.Len(t, links, 1)
			} else {
				require.Empty(t, links, "a present mismatched heartbeat must not become a metadata-only verified link")
			}
		})
	}
}

func TestCorrelationActivitiesWithholdsOlderStartedAttemptAcrossPages(t *testing.T) {
	for _, withCompletion := range []bool{false, true} {
		t.Run(fmt.Sprintf("old_completion_%t", withCompletion), func(t *testing.T) {
			fake := correlationService()
			fake.history.History.Events[1].GetActivityTaskStartedEventAttributes().Attempt = 1
			fake.describe.PendingActivities = []*workflowpb.PendingActivityInfo{{ActivityId: "activity", ActivityType: &commonpb.ActivityType{Name: "caesium.start"}, Attempt: 3}}
			if withCompletion {
				fake.history.History.Events = append(fake.history.History.Events, &historypb.HistoryEvent{EventId: 7, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED, Attributes: &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{ScheduledEventId: 4, StartedEventId: 5}}})
			}
			fake.history.NextPageToken = []byte("current-start-page")
			observer, err := NewObserver(fake, "default", "", 0, 0, 0)
			require.NoError(t, err)
			first, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
			require.NoError(t, err)
			require.Empty(t, first.Activities, "old attempt1 cannot be exposed when Describe reports3")
			require.True(t, first.Incomplete)
			require.Equal(t, []byte("current-start-page"), first.NextPageToken)
			current := &historypb.HistoryEvent{EventId: 9, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED, Attributes: &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{ScheduledEventId: 4, Attempt: 3}}}
			fake.history = &workflowservice.GetWorkflowExecutionHistoryResponse{History: &historypb.History{Events: []*historypb.HistoryEvent{current}}}
			second, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), first.NextPageToken, 100)
			require.NoError(t, err)
			require.Empty(t, second.Activities, "a later page cannot invent its missing scheduled identity")
			require.True(t, second.Incomplete)
		})
	}
}

func TestCorrelationActivitiesCurrentStartAdvancesScheduleOnlyDelegation(t *testing.T) {
	verifier, catalog, job, started := correlationAdmissionFixture(t)
	hint := connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: "run/activity", Outcome: "created", RunID: started.RunID.String()}
	fake := correlationService()
	scheduled := fake.history.History.Events[0]
	oldStart := fake.history.History.Events[1]
	oldStart.GetActivityTaskStartedEventAttributes().Attempt = 1
	fake.history.History.Events = []*historypb.HistoryEvent{scheduled}
	fake.describe.PendingActivities = []*workflowpb.PendingActivityInfo{{ActivityId: "activity", ActivityType: &commonpb.ActivityType{Name: "caesium.start"}, Attempt: 3, HeartbeatDetails: correlationPendingHint(t, hint)}}
	observer, err := NewObserver(fake, "default", "", 0, 0, 0)
	require.NoError(t, err)
	initial, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Len(t, initial.Activities, 1)
	require.EqualValues(t, 4, initial.Activities[0].SourceEventID)
	require.EqualValues(t, 3, initial.Activities[0].Attempt)
	before, err := verifier.Verify(t.Context(), correlationBindingFixture(), initial.Activities[0])
	require.NoError(t, err)
	fake.history.History.Events = []*historypb.HistoryEvent{scheduled, oldStart}
	fake.history.NextPageToken = []byte("later")
	partial, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Empty(t, partial.Activities)
	require.True(t, partial.Incomplete)
	links, err := catalog.Correlations(t.Context(), initial.Activities[0].Execution)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, before.EvidenceDigest, links[0].EvidenceDigest)
	require.Equal(t, before.VerifiedAt, links[0].VerifiedAt)
	current := &historypb.HistoryEvent{EventId: 9, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED, Attributes: &historypb.HistoryEvent_ActivityTaskStartedEventAttributes{ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{ScheduledEventId: 4, Attempt: 3}}}
	fake.history.History.Events = append(fake.history.History.Events, current)
	fake.history.NextPageToken = nil
	complete, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Len(t, complete.Activities, 1)
	require.False(t, complete.Incomplete)
	require.EqualValues(t, 9, complete.Activities[0].SourceEventID)
	require.EqualValues(t, 3, complete.Activities[0].Attempt)
	after, err := verifier.Verify(t.Context(), correlationBindingFixture(), complete.Activities[0])
	require.NoError(t, err)
	require.Equal(t, before.ID, after.ID)
	require.Equal(t, before.AdmissionID, after.AdmissionID)
	require.Equal(t, before.RunID, after.RunID)
	require.EqualValues(t, 9, after.SourceEventID)
	var evidence correlation.ActivityEvidence
	require.NoError(t, json.Unmarshal(after.Evidence, &evidence))
	require.EqualValues(t, 3, evidence.Attempt)
	links, err = catalog.Correlations(t.Context(), complete.Activities[0].Execution)
	require.NoError(t, err)
	require.Len(t, links, 1, "the matching current source updates the original delegation")
}

func TestCorrelationActivitiesPreservesPendingHintWithoutDecodedResult(t *testing.T) {
	for _, result := range []string{"absent", "custom_binary", "custom_metadata"} {
		t.Run(result, func(t *testing.T) {
			verifier, catalog, job, started := correlationAdmissionFixture(t)
			reported := connector.CorrelationEnvelope{Version: "v1", JobID: uuid.NewString(), IdempotencyKey: "run/activity", Outcome: "created", RunID: started.RunID.String()}
			fake := correlationService()
			fake.describe.PendingActivities = []*workflowpb.PendingActivityInfo{{ActivityId: "activity", ActivityType: &commonpb.ActivityType{Name: "caesium.start"}, Attempt: 2, HeartbeatDetails: correlationPendingHint(t, reported)}}
			var payloads *commonpb.Payloads
			switch result {
			case "custom_binary":
				payloads = &commonpb.Payloads{Payloads: []*commonpb.Payload{{Metadata: map[string][]byte{"encoding": []byte("binary/custom")}, Data: []byte("opaque-result-canary")}}}
			case "custom_metadata":
				payloads = correlationPendingHint(t, connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: "run/activity", Outcome: "created", RunID: started.RunID.String()})
				payloads.Payloads[0].Metadata["encryption-key-id"] = []byte("opaque-result-canary")
			}
			fake.history.History.Events = append(fake.history.History.Events, &historypb.HistoryEvent{EventId: 6, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED, Attributes: &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{ScheduledEventId: 4, StartedEventId: 5, Result: payloads}}})
			observer, err := NewObserver(fake, "default", "", 0, 0, 0)
			require.NoError(t, err)
			page, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
			require.NoError(t, err)
			require.Len(t, page.Activities, 1)
			require.Equal(t, &reported, page.Activities[0].Hint)
			require.EqualValues(t, 6, page.Activities[0].SourceEventID)
			_, err = verifier.Verify(t.Context(), correlationBindingFixture(), page.Activities[0])
			require.ErrorIs(t, err, correlation.ErrUnverifiedCorrelation)
			links, err := catalog.Correlations(t.Context(), page.Activities[0].Execution)
			require.NoError(t, err)
			require.Empty(t, links)
			encoded, err := json.Marshal(page)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "opaque-result-canary")
		})
	}
}

func TestCorrelationActivitiesRefusesConflictingPendingAndCompletionHints(t *testing.T) {
	verifier, catalog, job, started := correlationAdmissionFixture(t)
	stale := connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: "run/activity", Outcome: "queued", QueueID: uuid.NewString()}
	current := connector.CorrelationEnvelope{Version: "v1", JobID: job.ID.String(), IdempotencyKey: "run/activity", Outcome: "created", RunID: started.RunID.String()}
	fake := correlationService()
	fake.describe.PendingActivities = []*workflowpb.PendingActivityInfo{{ActivityId: "activity", ActivityType: &commonpb.ActivityType{Name: "caesium.start"}, Attempt: 2, HeartbeatDetails: correlationPendingHint(t, stale)}}
	fake.history.History.Events = append(fake.history.History.Events, &historypb.HistoryEvent{EventId: 6, EventType: enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED, Attributes: &historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{ScheduledEventId: 4, StartedEventId: 5, Result: correlationPendingHint(t, current)}}})
	observer, err := NewObserver(fake, "default", "", 0, 0, 0)
	require.NoError(t, err)
	page, err := observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.ErrorIs(t, err, correlation.ErrUnverifiedCorrelation)
	require.Empty(t, page.Activities)
	// A fresh read after pending state disappears may use the completed result.
	fake.describe.PendingActivities = nil
	page, err = observer.CorrelationActivities(t.Context(), "primary", "workflow", "run", correlationBindingFixture(), nil, 100)
	require.NoError(t, err)
	require.Len(t, page.Activities, 1)
	require.Equal(t, &current, page.Activities[0].Hint)
	linked, err := verifier.Verify(t.Context(), correlationBindingFixture(), page.Activities[0])
	require.NoError(t, err)
	require.Equal(t, started.RunID, linked.RunID)
	links, err := catalog.Correlations(t.Context(), page.Activities[0].Execution)
	require.NoError(t, err)
	require.Len(t, links, 1)
}

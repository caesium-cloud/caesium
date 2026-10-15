package temporal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/correlation"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/workflowservice/v1"
)

// CorrelationPage is bounded source evidence, never an imported execution or
// decoded arbitrary payload. Missing scheduled events on split pages leave the
// activity unavailable; callers must not infer its identity from a result hint.
type CorrelationPage struct {
	Activities    []correlation.ActivityEvidence `json:"activities"`
	NextPageToken []byte                         `json:"next_page_token,omitempty"`
	Incomplete    bool                           `json:"incomplete,omitempty"`
}

// CorrelationActivities pins both workflow and run before reading evidence.
// Workflow type and activity coordinates come from the provider, not a payload.
// Only recognized activities' optional frozen JSON hints are decoded. Custom
// payload encodings remain metadata-only and derive the same admission key.
func (o *Observer) CorrelationActivities(ctx context.Context, connectionID, workflowID, runID string, binding connector.Binding, pageToken []byte, pageSize int) (CorrelationPage, error) {
	if strings.TrimSpace(workflowID) == "" || strings.TrimSpace(runID) == "" || binding.Name == "" || binding.Version == "" {
		return CorrelationPage{}, correlation.ErrUnverifiedCorrelation
	}
	ctx, cancel := o.callContext(ctx)
	defer cancel()
	desc, err := o.service.DescribeWorkflowExecution(ctx, &workflowservice.DescribeWorkflowExecutionRequest{Namespace: o.scope, Execution: &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID}})
	if err != nil {
		return CorrelationPage{}, o.redact(err)
	}
	info := desc.GetWorkflowExecutionInfo()
	if info.GetExecution().GetWorkflowId() != workflowID || info.GetExecution().GetRunId() != runID || info.GetType().GetName() != binding.Name {
		return CorrelationPage{}, correlation.ErrUnverifiedCorrelation
	}
	ref, err := connector.NewExecutionReference(connectionID, map[string]string{"workflow_id": workflowID, "run_id": runID})
	if err != nil {
		return CorrelationPage{}, err
	}
	response, err := o.service.GetWorkflowExecutionHistory(ctx, &workflowservice.GetWorkflowExecutionHistoryRequest{Namespace: o.scope, Execution: &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID}, MaximumPageSize: o.clampPageSize(pageSize), NextPageToken: pageToken, HistoryEventFilterType: enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT})
	if err != nil {
		return CorrelationPage{}, o.redact(err)
	}
	if response == nil || response.GetHistory() == nil {
		return CorrelationPage{}, errors.New("temporal correlation: unavailable history")
	}
	events := response.GetHistory().GetEvents()
	out := CorrelationPage{NextPageToken: cloneToken(response.GetNextPageToken())}
	if len(events) > int(o.clampPageSize(pageSize)) {
		events = events[:o.clampPageSize(pageSize)]
		out.Incomplete = true
		out.NextPageToken = nil
	}
	records := map[int64]correlation.ActivityEvidence{}
	schedules := map[string]int64{}
	var order []int64
	allowed := map[string]bool{}
	for _, a := range binding.ActivityAllowlist {
		allowed[a.ActivityType] = true
	}
	// Retain only the recognized IDs on this bounded history page. Describe's
	// provider order and unrelated pending work must not discard their hints.
	for _, event := range events {
		attrs := event.GetActivityTaskScheduledEventAttributes()
		if attrs == nil || !allowed[attrs.GetActivityType().GetName()] {
			continue
		}
		id, kind := attrs.GetActivityId(), attrs.GetActivityType().GetName()
		if id == "" || kind == "" || event.GetEventId() <= 0 || event.GetEventType() != enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED {
			continue
		}
		if _, duplicate := schedules[id]; duplicate {
			return CorrelationPage{}, correlation.ErrUnverifiedCorrelation
		}
		records[event.GetEventId()] = correlation.ActivityEvidence{Execution: ref, WorkflowType: binding.Name, ActivityID: id, ActivityType: kind, IdempotencyKey: runID + "/" + id, SourceEventID: event.GetEventId(), SourceEventType: enumLabel(enumspb.EventType_name, int32(event.GetEventType())), ScheduledEventID: event.GetEventId(), Attempt: 1}
		schedules[id] = event.GetEventId()
		order = append(order, event.GetEventId())
	}
	pendingSeen := map[string]bool{}
	for _, pending := range desc.GetPendingActivities() {
		scheduledID, matched := schedules[pending.GetActivityId()]
		if !matched {
			continue
		}
		rec := records[scheduledID]
		if pendingSeen[rec.ActivityID] || pending.GetActivityType().GetName() != rec.ActivityType || pending.GetAttempt() < 1 {
			return CorrelationPage{}, correlation.ErrUnverifiedCorrelation
		}
		hint, err := correlationHint(pending.GetHeartbeatDetails())
		if err != nil {
			return CorrelationPage{}, err
		}
		rec.Hint = hint
		rec.Attempt = pending.GetAttempt()
		records[scheduledID] = rec
		pendingSeen[rec.ActivityID] = true
	}
	// An older historical start cannot replace Describe's current attempt or
	// advance the source event independently of that attempt. Withhold it until
	// this page also supplies the matching current start.
	missingCurrentStart := map[int64]bool{}

	for _, event := range events {
		if started := event.GetActivityTaskStartedEventAttributes(); started != nil {
			rec, ok := records[started.GetScheduledEventId()]
			if !ok {
				out.Incomplete = true
				continue
			}
			if event.GetEventType() != enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED || event.GetEventId() <= rec.SourceEventID || started.GetAttempt() < 1 {
				return CorrelationPage{}, correlation.ErrUnverifiedCorrelation
			}
			if started.GetAttempt() < rec.Attempt {
				missingCurrentStart[started.GetScheduledEventId()] = true
				continue
			}
			delete(missingCurrentStart, started.GetScheduledEventId())
			rec.SourceEventID = event.GetEventId()
			rec.SourceEventType = enumLabel(enumspb.EventType_name, int32(event.GetEventType()))
			rec.Attempt = started.GetAttempt()
			records[started.GetScheduledEventId()] = rec
		}
		if completed := event.GetActivityTaskCompletedEventAttributes(); completed != nil {
			rec, ok := records[completed.GetScheduledEventId()]
			if !ok {
				out.Incomplete = true
				continue
			}
			if missingCurrentStart[completed.GetScheduledEventId()] {
				continue
			}
			if event.GetEventType() != enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED || event.GetEventId() <= rec.SourceEventID {
				return CorrelationPage{}, correlation.ErrUnverifiedCorrelation
			}
			hint, err := correlationHint(completed.GetResult())
			if err != nil {
				return CorrelationPage{}, err
			}
			// A missing/custom result must not erase a reported heartbeat.
			// Conflicting decoded reports make this mixed read unverified; a
			// fresh Describe/history read can resolve their relative progress.
			if hint != nil {
				if rec.Hint != nil && *rec.Hint != *hint {
					return CorrelationPage{}, correlation.ErrUnverifiedCorrelation
				}
				rec.Hint = hint
			}
			rec.SourceEventID = event.GetEventId()
			rec.SourceEventType = enumLabel(enumspb.EventType_name, int32(event.GetEventType()))
			records[completed.GetScheduledEventId()] = rec
		}
	}
	for _, id := range order {
		if missingCurrentStart[id] {
			out.Incomplete = true
			continue
		}
		if len(out.Activities) >= int(o.clampPageSize(pageSize)) {
			out.Incomplete = true
			out.NextPageToken = nil
			break
		}
		out.Activities = append(out.Activities, records[id])
	}
	return o.boundCorrelationPage(out)
}

// Budget the complete response, including an empty page's cursor and the
// incomplete marker. A cursor may only be kept when the complete page fits.
func (o *Observer) boundCorrelationPage(page CorrelationPage) (CorrelationPage, error) {
	fits, err := o.withinBudget(page)
	if err != nil {
		return CorrelationPage{}, o.redact(err)
	}
	if fits {
		return page, nil
	}
	best, err := longestPrefix(len(page.Activities), func(count int) (bool, error) {
		return o.withinBudget(CorrelationPage{Activities: page.Activities[:count], Incomplete: true})
	})
	if err != nil {
		return CorrelationPage{}, o.redact(err)
	}
	if best < 1 {
		return CorrelationPage{}, fmt.Errorf("temporal correlation: %w", connector.ErrPayloadTooLarge)
	}
	return CorrelationPage{Activities: page.Activities[:best], Incomplete: true}, nil
}

func correlationHint(payloads *commonpb.Payloads) (*connector.CorrelationEnvelope, error) {
	if payloads == nil || len(payloads.GetPayloads()) == 0 {
		return nil, nil
	}
	var hint *connector.CorrelationEnvelope
	for _, payload := range payloads.GetPayloads() {
		// Additional codec/encryption metadata makes the payload custom even
		// if it retains json/plain as its encoding label. It is metadata-only.
		if len(payload.GetMetadata()) != 1 || string(payload.GetMetadata()["encoding"]) != "json/plain" {
			continue
		}
		if len(payload.GetData()) > connector.MaxPageMetadataBytes {
			return nil, connector.ErrPayloadTooLarge
		}
		data := bytes.TrimSpace(payload.GetData())
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return nil, correlation.ErrUnverifiedCorrelation
		}
		// Ordinary declared business results are not correlation hints.
		recognized := false
		for _, field := range []string{"version", "job_id", "idempotency_key", "outcome", "queue_id", "run_id"} {
			if _, ok := fields[field]; ok {
				recognized = true
			}
		}
		if !recognized {
			continue
		}
		if hint != nil || len(payloads.GetPayloads()) != 1 {
			return nil, correlation.ErrUnverifiedCorrelation
		}
		// The frozen envelope is a closed object of strings. Reject duplicate
		// keys and null optional values instead of silently discarding reports.
		fieldsDecoder := json.NewDecoder(bytes.NewReader(data))
		if _, err := fieldsDecoder.Token(); err != nil {
			return nil, correlation.ErrUnverifiedCorrelation
		}
		seen := map[string]bool{}
		for fieldsDecoder.More() {
			token, err := fieldsDecoder.Token()
			if err != nil {
				return nil, correlation.ErrUnverifiedCorrelation
			}
			field, ok := token.(string)
			if !ok || seen[field] {
				return nil, correlation.ErrUnverifiedCorrelation
			}
			seen[field] = true
			var value json.RawMessage
			if fieldsDecoder.Decode(&value) != nil || len(value) == 0 || value[0] != '"' {
				return nil, correlation.ErrUnverifiedCorrelation
			}
		}
		value := connector.CorrelationEnvelope{}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&value); err != nil {
			return nil, fmt.Errorf("%w: malformed activity hint", correlation.ErrUnverifiedCorrelation)
		}
		if decoder.Decode(new(any)) != io.EOF || value.Validate() != nil {
			return nil, correlation.ErrUnverifiedCorrelation
		}
		hint = &value
	}
	return hint, nil
}

package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
)

// Relation types. Delegation is not used: a child workflow is "child".
const (
	RelationParent       = "parent"
	RelationChild        = "child"
	RelationContinuation = "continuation"
)

// Service is the workflow-service surface observation needs. The real SDK
// client's WorkflowService implements it. Tests pass a fake.
// SDK CheckHealth in v1.49.0 is a gRPC health RPC, not GetSystemInfo, so
// health calls GetSystemInfo and DescribeNamespace directly.
type Service interface {
	ListWorkflowExecutions(ctx context.Context, in *workflowservice.ListWorkflowExecutionsRequest, opts ...grpc.CallOption) (*workflowservice.ListWorkflowExecutionsResponse, error)
	DescribeWorkflowExecution(ctx context.Context, in *workflowservice.DescribeWorkflowExecutionRequest, opts ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	GetWorkflowExecutionHistory(ctx context.Context, in *workflowservice.GetWorkflowExecutionHistoryRequest, opts ...grpc.CallOption) (*workflowservice.GetWorkflowExecutionHistoryResponse, error)
	GetSystemInfo(ctx context.Context, in *workflowservice.GetSystemInfoRequest, opts ...grpc.CallOption) (*workflowservice.GetSystemInfoResponse, error)
	DescribeNamespace(ctx context.Context, in *workflowservice.DescribeNamespaceRequest, opts ...grpc.CallOption) (*workflowservice.DescribeNamespaceResponse, error)
}

var _ Service = workflowservice.WorkflowServiceClient(nil)

// Observer reads one Temporal namespace. It does not store executions.
// apiKey is kept only so errors can be redacted. It is never logged.
type Observer struct {
	service          Service
	scope            string
	apiKey           string
	deadline         time.Duration
	maxPageEntries   int
	maxMetadataBytes int
	now              func() time.Time
}

// NewObserver checks the deadline ceiling, the page-size ceiling, and the
// exposed-metadata ceiling. A non-positive deadline uses connector.MaxRPCDeadline.
// A deadline above that ceiling is rejected.
// maxPageEntries of zero uses connector.MaxPageEntries.
// Any other value outside 1..connector.MaxPageEntries is rejected.
// maxMetadataBytes of zero uses connector.MaxPageMetadataBytes.
// Any other value outside 1..connector.MaxPageMetadataBytes is rejected.
func NewObserver(service Service, namespace, apiKey string, deadline time.Duration, maxPageEntries, maxMetadataBytes int) (*Observer, error) {
	if service == nil {
		return nil, errors.New("temporal service is required")
	}
	if strings.TrimSpace(namespace) == "" {
		return nil, redactError(errors.New("temporal namespace is required"), []string{apiKey})
	}
	if deadline < 0 {
		return nil, redactError(errors.New("temporal rpc deadline must be positive"), []string{apiKey})
	}
	if deadline == 0 {
		deadline = connector.MaxRPCDeadline
	}
	if deadline > connector.MaxRPCDeadline {
		return nil, redactError(fmt.Errorf("temporal rpc deadline must be at most %s", connector.MaxRPCDeadline), []string{apiKey})
	}
	if maxPageEntries == 0 {
		maxPageEntries = connector.MaxPageEntries
	} else if maxPageEntries < 1 || maxPageEntries > connector.MaxPageEntries {
		return nil, redactError(fmt.Errorf("temporal page size must be between 1 and %d", connector.MaxPageEntries), []string{apiKey})
	}
	if maxMetadataBytes == 0 {
		maxMetadataBytes = connector.MaxPageMetadataBytes
	} else if maxMetadataBytes < 1 || maxMetadataBytes > connector.MaxPageMetadataBytes {
		return nil, redactError(fmt.Errorf("temporal page metadata must be between 1 and %d bytes", connector.MaxPageMetadataBytes), []string{apiKey})
	}
	return &Observer{
		service:          service,
		scope:            namespace,
		apiKey:           apiKey,
		deadline:         deadline,
		maxPageEntries:   maxPageEntries,
		maxMetadataBytes: maxMetadataBytes,
		now:              time.Now,
	}, nil
}

// NewObserverFromClient uses the real SDK workflow service.
func NewObserverFromClient(c client.Client, namespace, apiKey string, deadline time.Duration, maxPageEntries, maxMetadataBytes int) (*Observer, error) {
	if c == nil {
		return nil, errors.New("temporal client is required")
	}
	return NewObserver(c.WorkflowService(), namespace, apiKey, deadline, maxPageEntries, maxMetadataBytes)
}

// Execution is one workflow run. Scope is the configured namespace, not a
// Temporal coordinate. Coordinates hold workflow_id and run_id only.
// Failure is true only for WORKFLOW_EXECUTION_STATUS_FAILED.
// Continued-as-new is terminal and is not a failure.
type Execution struct {
	WorkflowID    string            `json:"workflow_id"`
	RunID         string            `json:"run_id"`
	Scope         string            `json:"scope"`
	Coordinates   map[string]string `json:"coordinates"`
	NativeStatus  string            `json:"native_status"`
	DisplayStatus string            `json:"display_status"`
	Terminal      bool              `json:"terminal"`
	Failure       bool              `json:"failure"`
	ObservedAt    time.Time         `json:"observed_at,omitzero"`
	Parent        *Relation         `json:"parent,omitempty"`
	// Pending is the in-flight activity list from Describe. History does not
	// fill it. LastFailure and heartbeat payloads are not copied.
	Pending []PendingActivity `json:"pending_activities,omitempty"`
	// Incomplete is true when Pending was shortened so the JSON stays within
	// the metadata budget. Describe is not paged; the prefix is what fit.
	Incomplete bool `json:"incomplete,omitempty"`
}

// PendingActivity is one in-flight activity. State is the protobuf enum name.
type PendingActivity struct {
	ActivityID      string `json:"activity_id"`
	ActivityType    string `json:"activity_type"`
	Attempt         int32  `json:"attempt"`
	MaximumAttempts int32  `json:"maximum_attempts,omitempty"`
	State           string `json:"state"`
}

// Page is one visibility page. The server order is preserved; this package
// does not invent one. NextPageToken is the server token when every execution
// from that response fit in the metadata budget. Incomplete means a tail or
// that token was omitted. The caller repeats the same request token with a
// smaller page size. The omitted server token is not returned, because that
// token skips the dropped executions.
type Page struct {
	Executions    []Execution `json:"executions"`
	NextPageToken []byte      `json:"next_page_token,omitempty"`
	Incomplete    bool        `json:"incomplete,omitempty"`
}

// Relation is parent, child, or continuation. Type is never "delegation".
type Relation struct {
	Type       string `json:"type"`
	WorkflowID string `json:"workflow_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	EventID    int64  `json:"event_id,omitempty"`
}

// EventMeta is a history event id and its protobuf enum name. Attributes,
// including payloads, are not copied.
type EventMeta struct {
	ID   int64  `json:"event_id"`
	Type string `json:"event_type"`
}

// ActivityRecord is one activity attempt. Attempt and ScheduledEventID come
// from ActivityTaskStarted. ActivityID and ActivityType are set only when
// ActivityTaskScheduled for that scheduled event is on the same page.
// The observer does not remember scheduled events across RPCs. A closed
// attempt split across pages keeps ScheduledEventID and leaves id and type
// empty. In-flight attempts are PendingActivity values on Describe.
// An attempt is not a workflow failure.
type ActivityRecord struct {
	EventID          int64  `json:"event_id"`
	ScheduledEventID int64  `json:"scheduled_event_id"`
	ActivityID       string `json:"activity_id"`
	ActivityType     string `json:"activity_type"`
	Attempt          int32  `json:"attempt"`
}

// WorkflowTaskProblem is a workflow-task failure or timeout.
// It does not change the workflow's display status.
type WorkflowTaskProblem struct {
	EventID int64  `json:"event_id"`
	Cause   string `json:"cause"`
}

// HistoryPage is one bounded history page. Payloads are not copied.
// A non-empty NextPageToken means the server has more history. It does not
// mean the workflow failed. Incomplete means this response kept an event
// prefix, or dropped the server token, so the JSON stays within the metadata
// budget. Activities, relations, and problems match that prefix. The caller
// repeats the same request token with a smaller page size.
type HistoryPage struct {
	WorkflowID    string                `json:"workflow_id"`
	RunID         string                `json:"run_id"`
	Scope         string                `json:"scope"`
	ObservedAt    time.Time             `json:"observed_at,omitzero"`
	Events        []EventMeta           `json:"events,omitempty"`
	Activities    []ActivityRecord      `json:"activities,omitempty"`
	Relations     []Relation            `json:"relations,omitempty"`
	Problems      []WorkflowTaskProblem `json:"problems,omitempty"`
	NextPageToken []byte                `json:"next_page_token,omitempty"`
	Incomplete    bool                  `json:"incomplete,omitempty"`
}

// List returns one visibility page. query is passed through unchanged.
// pageSize is clamped to the observer's maximum. Zero or negative sizes
// use that maximum, so a page is never unbounded.
func (o *Observer) List(ctx context.Context, query string, pageToken []byte, pageSize int) (Page, error) {
	ctx, cancel := o.callContext(ctx)
	defer cancel()
	resp, err := o.service.ListWorkflowExecutions(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace:     o.scope,
		PageSize:      o.clampPageSize(pageSize),
		NextPageToken: pageToken,
		Query:         query,
	})
	if err != nil {
		return Page{}, o.redact(fmt.Errorf("temporal list: %w", err))
	}
	if resp == nil {
		return Page{}, o.redact(errors.New("temporal list: empty response"))
	}
	var executions []Execution
	for _, info := range resp.GetExecutions() {
		executions = append(executions, o.executionFromInfo(info))
	}
	return o.boundList(executions, resp.GetNextPageToken())
}

// Describe reads one execution. ObservedAt is the caller's clock on the
// returned value and is not stored. ParentExecution, when set, is relation
// type parent. Pending activities are id, type, attempt, maximum attempts,
// and state. Failure payloads are not copied.
func (o *Observer) Describe(ctx context.Context, workflowID, runID string) (Execution, error) {
	if strings.TrimSpace(workflowID) == "" {
		return Execution{}, o.redact(errors.New("temporal describe: workflow id is required"))
	}
	ctx, cancel := o.callContext(ctx)
	defer cancel()
	resp, err := o.service.DescribeWorkflowExecution(ctx, &workflowservice.DescribeWorkflowExecutionRequest{
		Namespace: o.scope,
		Execution: &commonpb.WorkflowExecution{WorkflowId: workflowID, RunId: runID},
	})
	if err != nil {
		return Execution{}, o.redact(fmt.Errorf("temporal describe: %w", err))
	}
	if resp == nil || resp.GetWorkflowExecutionInfo() == nil {
		return Execution{}, o.redact(errors.New("temporal describe: missing execution"))
	}
	execution := o.executionFromInfo(resp.GetWorkflowExecutionInfo())
	execution.ObservedAt = o.clock()
	if execution.WorkflowID == "" {
		execution.WorkflowID = workflowID
		execution.Coordinates["workflow_id"] = workflowID
	}
	if execution.RunID == "" {
		execution.RunID = runID
		execution.Coordinates["run_id"] = runID
	}
	execution.Pending = pendingActivities(resp.GetPendingActivities())
	return o.boundDescribe(execution)
}

// History returns one metadata page. Workflow inputs, results, and activity
// payloads are not copied. Activity attempts and workflow-task problems do
// not change the workflow status.
func (o *Observer) History(ctx context.Context, workflowID, runID string, pageToken []byte, pageSize int) (HistoryPage, error) {
	if strings.TrimSpace(workflowID) == "" {
		return HistoryPage{}, o.redact(errors.New("temporal history: workflow id is required"))
	}
	ctx, cancel := o.callContext(ctx)
	defer cancel()
	resp, err := o.service.GetWorkflowExecutionHistory(ctx, &workflowservice.GetWorkflowExecutionHistoryRequest{
		Namespace: o.scope,
		Execution: &commonpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      runID,
		},
		MaximumPageSize:        o.clampPageSize(pageSize),
		NextPageToken:          pageToken,
		WaitNewEvent:           false,
		HistoryEventFilterType: enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
		SkipArchival:           false,
	})
	if err != nil {
		return HistoryPage{}, o.redact(fmt.Errorf("temporal history: %w", err))
	}
	if resp == nil {
		return HistoryPage{}, o.redact(errors.New("temporal history: empty response"))
	}
	if resp.GetHistory() == nil && len(resp.GetRawHistory()) > 0 {
		return HistoryPage{}, o.redact(errors.New("temporal history: encoded history is not decoded"))
	}
	page := HistoryPage{
		WorkflowID: workflowID,
		RunID:      runID,
		Scope:      o.scope,
		ObservedAt: o.clock(),
	}
	if page.RunID == "" {
		// The history response does not echo a run id. WorkflowExecutionStarted's
		// OriginalExecutionRunId is preserved across reset while the current
		// execution run id changes, and ContinuedExecutionRunId is the previous
		// run. Describe resolves the current run the same way an empty run id
		// does on the history RPC. Resolve it before the metadata budget is
		// measured so the returned JSON cannot grow past that budget.
		resolved, err := o.describedRunID(ctx, workflowID)
		if err != nil {
			return HistoryPage{}, err
		}
		page.RunID = resolved
	}
	events := []*historypb.HistoryEvent{}
	if resp.GetHistory() != nil {
		events = resp.GetHistory().GetEvents()
	}
	return o.boundHistory(page, workflowID, events, resp.GetNextPageToken())
}

// Health returns nil when GetSystemInfo and DescribeNamespace both succeed.
// A failure is availability only. It is not a workflow display status, and a
// missing worker is not a workflow failure. A wrong namespace is the same
// kind of availability error.
func (o *Observer) Health(ctx context.Context) error {
	ctx, cancel := o.callContext(ctx)
	defer cancel()
	if _, err := o.service.GetSystemInfo(ctx, &workflowservice.GetSystemInfoRequest{}); err != nil {
		return o.redact(fmt.Errorf("temporal unavailable: %w", err))
	}
	if _, err := o.service.DescribeNamespace(ctx, &workflowservice.DescribeNamespaceRequest{Namespace: o.scope}); err != nil {
		return o.redact(fmt.Errorf("temporal unavailable: %w", err))
	}
	return nil
}

func (o *Observer) executionFromInfo(info *workflowpb.WorkflowExecutionInfo) Execution {
	native, display, terminal, failure := statusView(info.GetStatus())
	workflowID := ""
	runID := ""
	if execution := info.GetExecution(); execution != nil {
		workflowID = execution.GetWorkflowId()
		runID = execution.GetRunId()
	}
	out := Execution{
		WorkflowID:    workflowID,
		RunID:         runID,
		Scope:         o.scope,
		Coordinates:   map[string]string{"workflow_id": workflowID, "run_id": runID},
		NativeStatus:  native,
		DisplayStatus: display,
		Terminal:      terminal,
		Failure:       failure,
	}
	if parent := info.GetParentExecution(); parent != nil {
		out.Parent = &Relation{
			Type:       RelationParent,
			WorkflowID: parent.GetWorkflowId(),
			RunID:      parent.GetRunId(),
		}
	}
	return out
}

func (o *Observer) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= o.deadline {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, o.deadline)
}

func (o *Observer) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

func (o *Observer) redact(err error) error {
	var secrets []string
	if o != nil {
		secrets = []string{o.apiKey}
	}
	return connector.RedactError(err, secrets, nil)
}

func cloneToken(token []byte) []byte {
	if token == nil {
		return nil
	}
	out := make([]byte, len(token))
	copy(out, token)
	return out
}

func (o *Observer) clampPageSize(pageSize int) int32 {
	limit := connector.MaxPageEntries
	if o != nil && o.maxPageEntries >= 1 && o.maxPageEntries < limit {
		limit = o.maxPageEntries
	}
	if pageSize < 1 || pageSize > limit {
		return int32(limit)
	}
	return int32(pageSize)
}

// describedRunID asks for the current run when history did not identify one.
// The caller must not use this when a run id was passed or already resolved.
func (o *Observer) describedRunID(ctx context.Context, workflowID string) (string, error) {
	resp, err := o.service.DescribeWorkflowExecution(ctx, &workflowservice.DescribeWorkflowExecutionRequest{
		Namespace: o.scope,
		Execution: &commonpb.WorkflowExecution{WorkflowId: workflowID},
	})
	if err != nil {
		return "", o.redact(fmt.Errorf("temporal history: %w", err))
	}
	var runID string
	if resp != nil && resp.GetWorkflowExecutionInfo() != nil && resp.GetWorkflowExecutionInfo().GetExecution() != nil {
		runID = resp.GetWorkflowExecutionInfo().GetExecution().GetRunId()
	}
	if runID == "" {
		return "", o.redact(errors.New("temporal history: missing run id"))
	}
	return runID, nil
}

func statusView(status enumspb.WorkflowExecutionStatus) (native, display string, terminal, failure bool) {
	native = enumLabel(enumspb.WorkflowExecutionStatus_name, int32(status))
	switch status {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return native, "running", false, false
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return native, "completed", true, false
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		return native, "failed", true, true
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return native, "canceled", true, false
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return native, "terminated", true, false
	case enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return native, "continued", true, false
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return native, "timed_out", true, false
	default:
		return native, "unknown", false, false
	}
}

func enumLabel(names map[int32]string, number int32) string {
	if name, ok := names[number]; ok {
		return name
	}
	return strconv.Itoa(int(number))
}

type scheduledActivity struct {
	id   string
	name string
}

func projectHistory(workflowID string, events []*historypb.HistoryEvent) ([]EventMeta, []ActivityRecord, []Relation, []WorkflowTaskProblem) {
	// This page only. The Observer does not keep activity identity across RPCs.
	scheduled := make(map[int64]scheduledActivity, len(events))
	for _, event := range events {
		attrs := event.GetActivityTaskScheduledEventAttributes()
		if attrs == nil {
			continue
		}
		name := ""
		if activityType := attrs.GetActivityType(); activityType != nil {
			name = activityType.GetName()
		}
		scheduled[event.GetEventId()] = scheduledActivity{id: attrs.GetActivityId(), name: name}
	}
	metas := make([]EventMeta, 0, len(events))
	var activities []ActivityRecord
	var relations []Relation
	var problems []WorkflowTaskProblem
	for _, event := range events {
		metas = append(metas, EventMeta{
			ID:   event.GetEventId(),
			Type: enumLabel(enumspb.EventType_name, int32(event.GetEventType())),
		})
		if attrs := event.GetActivityTaskStartedEventAttributes(); attrs != nil {
			ref := scheduled[attrs.GetScheduledEventId()]
			activities = append(activities, ActivityRecord{
				EventID:          event.GetEventId(),
				ScheduledEventID: attrs.GetScheduledEventId(),
				ActivityID:       ref.id,
				ActivityType:     ref.name,
				Attempt:          attrs.GetAttempt(),
			})
		}
		if attrs := event.GetWorkflowExecutionStartedEventAttributes(); attrs != nil {
			if parent := attrs.GetParentWorkflowExecution(); parent != nil {
				relations = append(relations, Relation{
					Type:       RelationParent,
					WorkflowID: parent.GetWorkflowId(),
					RunID:      parent.GetRunId(),
					EventID:    event.GetEventId(),
				})
			}
			relations = appendContinuation(relations, workflowID, attrs.GetContinuedExecutionRunId(), event.GetEventId())
		}
		if attrs := event.GetChildWorkflowExecutionStartedEventAttributes(); attrs != nil {
			childID := ""
			childRun := ""
			if execution := attrs.GetWorkflowExecution(); execution != nil {
				childID = execution.GetWorkflowId()
				childRun = execution.GetRunId()
			}
			relations = append(relations, Relation{
				Type:       RelationChild,
				WorkflowID: childID,
				RunID:      childRun,
				EventID:    event.GetEventId(),
			})
		}
		if attrs := event.GetWorkflowExecutionContinuedAsNewEventAttributes(); attrs != nil {
			relations = appendContinuation(relations, workflowID, attrs.GetNewExecutionRunId(), event.GetEventId())
		}
		if attrs := event.GetWorkflowExecutionFailedEventAttributes(); attrs != nil {
			relations = appendContinuation(relations, workflowID, attrs.GetNewExecutionRunId(), event.GetEventId())
		}
		if attrs := event.GetWorkflowExecutionTimedOutEventAttributes(); attrs != nil {
			relations = appendContinuation(relations, workflowID, attrs.GetNewExecutionRunId(), event.GetEventId())
		}
		if attrs := event.GetWorkflowExecutionCompletedEventAttributes(); attrs != nil {
			relations = appendContinuation(relations, workflowID, attrs.GetNewExecutionRunId(), event.GetEventId())
		}
		if attrs := event.GetWorkflowTaskFailedEventAttributes(); attrs != nil {
			problems = append(problems, WorkflowTaskProblem{
				EventID: event.GetEventId(),
				Cause:   enumLabel(enumspb.WorkflowTaskFailedCause_name, int32(attrs.GetCause())),
			})
		}
		if attrs := event.GetWorkflowTaskTimedOutEventAttributes(); attrs != nil {
			problems = append(problems, WorkflowTaskProblem{
				EventID: event.GetEventId(),
				Cause:   enumLabel(enumspb.TimeoutType_name, int32(attrs.GetTimeoutType())),
			})
		}
	}
	return metas, activities, relations, problems
}

// appendContinuation records a new or previous run on the same workflow.
// An empty run id is not a relation. Cron and retry set NewExecutionRunId on
// the closing event; continued-as-new does the same. There is no sub-kind.
func appendContinuation(relations []Relation, workflowID, runID string, eventID int64) []Relation {
	if runID == "" {
		return relations
	}
	return append(relations, Relation{
		Type:       RelationContinuation,
		WorkflowID: workflowID,
		RunID:      runID,
		EventID:    eventID,
	})
}

func pendingActivities(infos []*workflowpb.PendingActivityInfo) []PendingActivity {
	if len(infos) == 0 {
		return nil
	}
	out := make([]PendingActivity, 0, len(infos))
	for _, info := range infos {
		if info == nil {
			continue
		}
		name := ""
		if activityType := info.GetActivityType(); activityType != nil {
			name = activityType.GetName()
		}
		out = append(out, PendingActivity{
			ActivityID:      info.GetActivityId(),
			ActivityType:    name,
			Attempt:         info.GetAttempt(),
			MaximumAttempts: info.GetMaximumAttempts(),
			State:           enumLabel(enumspb.PendingActivityState_name, int32(info.GetState())),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (o *Observer) metadataBudget() int {
	if o == nil || o.maxMetadataBytes < 1 || o.maxMetadataBytes > connector.MaxPageMetadataBytes {
		return connector.MaxPageMetadataBytes
	}
	return o.maxMetadataBytes
}

func (o *Observer) withinBudget(v any) (bool, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	return len(raw) <= o.metadataBudget(), nil
}

// longestPrefix returns the largest count in 0..n for which fit is true.
// A result below zero means even the empty prefix does not fit.
func longestPrefix(n int, fit func(count int) (bool, error)) (int, error) {
	best := -1
	lo, hi := 0, n
	for lo <= hi {
		mid := (lo + hi) / 2
		ok, err := fit(mid)
		if err != nil {
			return 0, err
		}
		if ok {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return best, nil
}

func (o *Observer) boundList(executions []Execution, token []byte) (Page, error) {
	if executions == nil {
		executions = []Execution{}
	}
	full := Page{Executions: executions, NextPageToken: cloneToken(token)}
	ok, err := o.withinBudget(full)
	if err != nil {
		return Page{}, o.redact(err)
	}
	if ok {
		return full, nil
	}
	best, err := longestPrefix(len(executions), func(count int) (bool, error) {
		trial := Page{Executions: executions[:count], Incomplete: true}
		if count == 0 {
			trial.Executions = []Execution{}
		}
		return o.withinBudget(trial)
	})
	if err != nil {
		return Page{}, o.redact(err)
	}
	if best < 1 {
		if len(executions) == 0 {
			return Page{}, o.redact(errors.New("temporal list: page token exceeds metadata budget"))
		}
		if best < 0 {
			return Page{}, o.redact(errors.New("temporal list: page exceeds metadata budget"))
		}
		return Page{}, o.redact(errors.New("temporal list: execution exceeds metadata budget"))
	}
	return Page{Executions: executions[:best], Incomplete: true}, nil
}

func (o *Observer) boundDescribe(execution Execution) (Execution, error) {
	ok, err := o.withinBudget(execution)
	if err != nil {
		return Execution{}, o.redact(err)
	}
	if ok {
		return execution, nil
	}
	pending := execution.Pending
	execution.Pending = nil
	execution.Incomplete = false
	baseOK, err := o.withinBudget(execution)
	if err != nil {
		return Execution{}, o.redact(err)
	}
	if !baseOK || len(pending) == 0 {
		return Execution{}, o.redact(errors.New("temporal describe: execution exceeds metadata budget"))
	}
	execution.Incomplete = true
	markedOK, err := o.withinBudget(execution)
	if err != nil {
		return Execution{}, o.redact(err)
	}
	if !markedOK {
		return Execution{}, o.redact(errors.New("temporal describe: execution exceeds metadata budget"))
	}
	best, err := longestPrefix(len(pending), func(count int) (bool, error) {
		trial := execution
		trial.Incomplete = true
		if count == 0 {
			trial.Pending = nil
		} else {
			trial.Pending = pending[:count]
		}
		return o.withinBudget(trial)
	})
	if err != nil {
		return Execution{}, o.redact(err)
	}
	if best < 1 {
		execution.Pending = nil
		return execution, nil
	}
	execution.Pending = pending[:best]
	execution.Incomplete = true
	return execution, nil
}

func (o *Observer) boundHistory(base HistoryPage, workflowID string, events []*historypb.HistoryEvent, token []byte) (HistoryPage, error) {
	full := o.historyPrefix(base, workflowID, events, len(events))
	full.NextPageToken = cloneToken(token)
	full.Incomplete = false
	ok, err := o.withinBudget(full)
	if err != nil {
		return HistoryPage{}, o.redact(err)
	}
	if ok {
		return full, nil
	}
	best, err := longestPrefix(len(events), func(count int) (bool, error) {
		trial := o.historyPrefix(base, workflowID, events, count)
		trial.Incomplete = true
		return o.withinBudget(trial)
	})
	if err != nil {
		return HistoryPage{}, o.redact(err)
	}
	if best < 1 {
		if len(events) == 0 {
			return HistoryPage{}, o.redact(errors.New("temporal history: page token exceeds metadata budget"))
		}
		if best < 0 {
			return HistoryPage{}, o.redact(errors.New("temporal history: page exceeds metadata budget"))
		}
		return HistoryPage{}, o.redact(errors.New("temporal history: event exceeds metadata budget"))
	}
	page := o.historyPrefix(base, workflowID, events, best)
	page.Incomplete = true
	return page, nil
}

func (o *Observer) historyPrefix(base HistoryPage, workflowID string, events []*historypb.HistoryEvent, count int) HistoryPage {
	if count < 0 {
		count = 0
	}
	if count > len(events) {
		count = len(events)
	}
	page := base
	page.NextPageToken = nil
	page.Events, page.Activities, page.Relations, page.Problems = projectHistory(workflowID, events[:count])
	return page
}

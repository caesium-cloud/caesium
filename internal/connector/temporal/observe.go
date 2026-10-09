package temporal

import (
	"context"
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
// health calls GetSystemInfo directly.
type Service interface {
	ListWorkflowExecutions(ctx context.Context, in *workflowservice.ListWorkflowExecutionsRequest, opts ...grpc.CallOption) (*workflowservice.ListWorkflowExecutionsResponse, error)
	DescribeWorkflowExecution(ctx context.Context, in *workflowservice.DescribeWorkflowExecutionRequest, opts ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	GetWorkflowExecutionHistory(ctx context.Context, in *workflowservice.GetWorkflowExecutionHistoryRequest, opts ...grpc.CallOption) (*workflowservice.GetWorkflowExecutionHistoryResponse, error)
	GetSystemInfo(ctx context.Context, in *workflowservice.GetSystemInfoRequest, opts ...grpc.CallOption) (*workflowservice.GetSystemInfoResponse, error)
}

var _ Service = workflowservice.WorkflowServiceClient(nil)

// Observer reads one Temporal namespace. It does not store executions.
// apiKey is kept only so errors can be redacted. It is never logged.
type Observer struct {
	service  Service
	scope    string
	apiKey   string
	deadline time.Duration
	now      func() time.Time
}

// NewObserver checks the deadline ceiling and returns an observer.
// A non-positive deadline uses connector.MaxRPCDeadline.
// A deadline above that ceiling is rejected.
func NewObserver(service Service, namespace, apiKey string, deadline time.Duration) (*Observer, error) {
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
	return &Observer{
		service:  service,
		scope:    namespace,
		apiKey:   apiKey,
		deadline: deadline,
		now:      time.Now,
	}, nil
}

// NewObserverFromClient uses the real SDK workflow service.
func NewObserverFromClient(c client.Client, namespace, apiKey string, deadline time.Duration) (*Observer, error) {
	if c == nil {
		return nil, errors.New("temporal client is required")
	}
	return NewObserver(c.WorkflowService(), namespace, apiKey, deadline)
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
	ObservedAt    time.Time         `json:"observed_at,omitempty"`
	Parent        *Relation         `json:"parent,omitempty"`
}

// Page is one visibility page. NextPageToken is the server token, unchanged.
// The server order is preserved; this package does not invent one.
type Page struct {
	Executions    []Execution `json:"executions"`
	NextPageToken []byte      `json:"next_page_token,omitempty"`
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

// ActivityRecord is one activity attempt. Attempt comes from
// ActivityTaskStarted. An attempt is not a workflow failure.
type ActivityRecord struct {
	EventID      int64  `json:"event_id"`
	ActivityID   string `json:"activity_id"`
	ActivityType string `json:"activity_type"`
	Attempt      int32  `json:"attempt"`
}

// WorkflowTaskProblem is a workflow-task failure or timeout.
// It does not change the workflow's display status.
type WorkflowTaskProblem struct {
	EventID int64  `json:"event_id"`
	Cause   string `json:"cause"`
}

// HistoryPage is one bounded history page. Payloads are not copied.
// A non-empty NextPageToken means the page is incomplete, not that the
// workflow failed.
type HistoryPage struct {
	WorkflowID    string                `json:"workflow_id"`
	RunID         string                `json:"run_id"`
	Scope         string                `json:"scope"`
	ObservedAt    time.Time             `json:"observed_at,omitempty"`
	Events        []EventMeta           `json:"events,omitempty"`
	Activities    []ActivityRecord      `json:"activities,omitempty"`
	Relations     []Relation            `json:"relations,omitempty"`
	Problems      []WorkflowTaskProblem `json:"problems,omitempty"`
	NextPageToken []byte                `json:"next_page_token,omitempty"`
}

// List returns one visibility page. query is passed through unchanged.
// pageSize is clamped to connector.MaxPageEntries. Zero or negative sizes
// use that ceiling, so a page is never unbounded.
func (o *Observer) List(ctx context.Context, query string, pageToken []byte, pageSize int) (Page, error) {
	ctx, cancel := o.callContext(ctx)
	defer cancel()
	resp, err := o.service.ListWorkflowExecutions(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Namespace:     o.scope,
		PageSize:      clampPageSize(pageSize),
		NextPageToken: pageToken,
		Query:         query,
	})
	if err != nil {
		return Page{}, o.redact(fmt.Errorf("temporal list: %w", err))
	}
	if resp == nil {
		return Page{}, o.redact(errors.New("temporal list: empty response"))
	}
	page := Page{NextPageToken: cloneToken(resp.GetNextPageToken())}
	for _, info := range resp.GetExecutions() {
		page.Executions = append(page.Executions, o.executionFromInfo(info))
	}
	if page.Executions == nil {
		page.Executions = []Execution{}
	}
	return page, nil
}

// Describe reads one execution. ObservedAt is the caller's clock on the
// returned value and is not stored. When ParentExecution is set, the
// relation type is parent.
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
	if parent := resp.GetWorkflowExecutionInfo().GetParentExecution(); parent != nil {
		execution.Parent = &Relation{
			Type:       RelationParent,
			WorkflowID: parent.GetWorkflowId(),
			RunID:      parent.GetRunId(),
		}
	}
	return execution, nil
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
		MaximumPageSize:        clampPageSize(pageSize),
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
		WorkflowID:    workflowID,
		RunID:         runID,
		Scope:         o.scope,
		ObservedAt:    o.clock(),
		NextPageToken: cloneToken(resp.GetNextPageToken()),
	}
	events := []*historypb.HistoryEvent{}
	if resp.GetHistory() != nil {
		events = resp.GetHistory().GetEvents()
	}
	page.Events, page.Activities, page.Relations, page.Problems = projectHistory(workflowID, events)
	return page, nil
}

// Health returns nil when GetSystemInfo succeeds. A failure is availability
// only. It is not a workflow display status, and a missing worker is not a
// workflow failure.
func (o *Observer) Health(ctx context.Context) error {
	ctx, cancel := o.callContext(ctx)
	defer cancel()
	_, err := o.service.GetSystemInfo(ctx, &workflowservice.GetSystemInfoRequest{})
	if err != nil {
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
	return Execution{
		WorkflowID:    workflowID,
		RunID:         runID,
		Scope:         o.scope,
		Coordinates:   map[string]string{"workflow_id": workflowID, "run_id": runID},
		NativeStatus:  native,
		DisplayStatus: display,
		Terminal:      terminal,
		Failure:       failure,
	}
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
	if o == nil {
		return redactError(err, nil)
	}
	return redactError(err, []string{o.apiKey})
}

func cloneToken(token []byte) []byte {
	if token == nil {
		return nil
	}
	out := make([]byte, len(token))
	copy(out, token)
	return out
}

func clampPageSize(pageSize int) int32 {
	if pageSize < 1 || pageSize > connector.MaxPageEntries {
		return connector.MaxPageEntries
	}
	return int32(pageSize)
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
				EventID:      event.GetEventId(),
				ActivityID:   ref.id,
				ActivityType: ref.name,
				Attempt:      attrs.GetAttempt(),
			})
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
			relations = append(relations, Relation{
				Type:       RelationContinuation,
				WorkflowID: workflowID,
				RunID:      attrs.GetNewExecutionRunId(),
				EventID:    event.GetEventId(),
			})
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

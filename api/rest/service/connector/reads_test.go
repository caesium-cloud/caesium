package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/connector/temporal"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const testCursorSecret = "connector-shared-test-cursor-secret-at-least-32"

func testRuntime(t *testing.T) (*Runtime, *gorm.DB) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	cfg := &core.Config{Version: 1, Connections: []core.Connection{{ID: "primary", Enabled: true, Provider: core.ProviderTemporal, Endpoint: "localhost:7233", Scope: "default", Limits: core.DefaultLimits()}}}
	r := New(context.Background(), db, cfg, "fingerprint", "", nil, testCursorSecret)
	require.NoError(t, r.Ready(context.Background()))
	t.Cleanup(r.Close)
	return r, db
}
func TestCursorAuthenticatesConnectionFilterAndEpochAcrossReplicas(t *testing.T) {
	r, _ := testRuntime(t)
	token := r.encodeCursor("primary", "list:type:10", []byte("remote-token"))
	replica := &Runtime{fingerprint: r.fingerprint, cursorKey: []byte(testCursorSecret)}
	raw, err := replica.decodeCursor("primary", "list:type:10", token)
	require.NoError(t, err)
	require.Equal(t, []byte("remote-token"), raw)
	parts := strings.Split(token, ".")
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	parts[0] = base64.RawURLEncoding.EncodeToString([]byte(strings.ReplaceAll(string(body), "primary", "other")))
	_, err = replica.decodeCursor("other", "list:type:10", strings.Join(parts, "."))
	require.ErrorIs(t, err, ErrInvalidRequest)
	_, err = replica.decodeCursor("primary", "list:different:10", token)
	require.ErrorIs(t, err, ErrInvalidRequest)
	replica.fingerprint = "changed"
	_, err = replica.decodeCursor("primary", "list:type:10", token)
	require.ErrorIs(t, err, ErrInvalidRequest)
}

type describeService struct {
	workflowservice.WorkflowServiceClient
	unavailable  atomic.Bool
	block        atomic.Bool
	calls        atomic.Int64
	listCalls    atomic.Int64
	historyCalls atomic.Int64
}

func (s *describeService) DescribeWorkflowExecution(ctx context.Context, req *workflowservice.DescribeWorkflowExecutionRequest, opts ...grpc.CallOption) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	s.calls.Add(1)
	if s.block.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.unavailable.Load() {
		return nil, serviceerror.NewUnavailable("fixture outage")
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Execution: req.Execution, Type: &commonpb.WorkflowType{Name: "TemporalOnly"}, HistoryLength: 3, Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING}}, nil
}
func (s *describeService) ListWorkflowExecutions(_ context.Context, _ *workflowservice.ListWorkflowExecutionsRequest, _ ...grpc.CallOption) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	s.listCalls.Add(1)
	return &workflowservice.ListWorkflowExecutionsResponse{Executions: []*workflowpb.WorkflowExecutionInfo{
		{Execution: &commonpb.WorkflowExecution{WorkflowId: "temporal-only-a", RunId: "run-a"}, Type: &commonpb.WorkflowType{Name: "TemporalOnly"}, Status: enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING},
		{Execution: &commonpb.WorkflowExecution{WorkflowId: "temporal-only-b", RunId: "run-b"}, Type: &commonpb.WorkflowType{Name: "TemporalOnly"}, Status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED},
	}}, nil
}
func (s *describeService) GetWorkflowExecutionHistory(_ context.Context, _ *workflowservice.GetWorkflowExecutionHistoryRequest, _ ...grpc.CallOption) (*workflowservice.GetWorkflowExecutionHistoryResponse, error) {
	s.historyCalls.Add(1)
	return &workflowservice.GetWorkflowExecutionHistoryResponse{History: &historypb.History{}}, nil
}

func TestListAndHistoryDateSuccessfulObservationsWithoutCatalogWrites(t *testing.T) {
	r, db := testRuntime(t)
	svc := &describeService{}
	observer, err := temporal.NewObserver(svc, "default", "", time.Second, 100, 20<<10)
	require.NoError(t, err)
	r.connections["primary"].observer = observer
	var writes atomic.Int64
	count := func(*gorm.DB) { writes.Add(1) }
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test-page-create", count))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test-page-update", count))
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("test-page-delete", count))
	cardinality := func() []int64 {
		tables := []any{&models.ExternalExecution{}, &models.ExternalExecutionSnapshot{}, &models.ExternalExecutionRelation{}, &models.ConnectorOperation{}}
		out := make([]int64, len(tables))
		for i, table := range tables {
			require.NoError(t, db.Model(table).Count(&out[i]).Error)
		}
		return out
	}
	beforeCardinality := cardinality()
	var previous Page
	for range 2 {
		before := time.Now().UTC()
		raw, err := r.List(t.Context(), "actor", "primary", "", "", 100)
		after := time.Now().UTC()
		require.NoError(t, err)
		var page Page
		require.NoError(t, json.Unmarshal(raw, &page))
		require.Len(t, page.Executions, 2)
		for i, execution := range page.Executions {
			require.False(t, execution.ObservedAt.IsZero())
			require.False(t, execution.ObservedAt.Before(before), "row must be dated by this successful observation")
			require.False(t, execution.ObservedAt.After(after), "row cannot be dated after its response")
			require.Equal(t, time.UTC, execution.ObservedAt.Location())
			require.Equal(t, execution.ObservedAt, execution.Metadata.ObservedAt)
			require.Nil(t, execution.SnapshotObservedAt, "discovery must not manufacture a catalog snapshot")
			if previous.Executions != nil {
				require.Equal(t, previous.Executions[i].ID, execution.ID)
				require.False(t, execution.ObservedAt.Before(previous.Executions[i].ObservedAt))
			}
		}
		require.Equal(t, page.Executions[0].ObservedAt, page.Executions[1].ObservedAt, "one provider page shares an observation time")
		previous = page
	}
	ref, err := core.NewExecutionReference("primary", map[string]string{"workflow_id": "temporal-only-a", "run_id": "run-a"})
	require.NoError(t, err)
	for range 2 {
		before := time.Now().UTC()
		raw, err := r.History(t.Context(), "actor", "primary", core.ReferenceID(ref), "", 100)
		after := time.Now().UTC()
		require.NoError(t, err)
		var history History
		require.NoError(t, json.Unmarshal(raw, &history))
		require.False(t, history.ObservedAt.IsZero())
		require.False(t, history.ObservedAt.Before(before))
		require.False(t, history.ObservedAt.After(after))
	}
	require.Equal(t, int64(2), svc.listCalls.Load())
	require.Equal(t, int64(2), svc.historyCalls.Load())
	require.Zero(t, writes.Load(), "unchanged list/history polling must perform no catalog write")
	require.Equal(t, beforeCardinality, cardinality(), "stateless list/history reads must not grow the catalog")
	require.Equal(t, []int64{0, 0, 0, 0}, beforeCardinality)
}

func TestDetailReportsLiveTimeWithoutCatalogWritesAndDatedStaleFallback(t *testing.T) {
	r, db := testRuntime(t)
	svc := &describeService{}
	observer, err := temporal.NewObserver(svc, "default", "", time.Second, 100, 20<<10)
	require.NoError(t, err)
	r.connections["primary"].observer = observer
	ref, err := core.NewExecutionReference("primary", map[string]string{"workflow_id": "temporal-only", "run_id": "run-1"})
	require.NoError(t, err)
	raw, err := r.Detail(context.Background(), "actor", "primary", core.ReferenceID(ref))
	require.NoError(t, err)
	var first Execution
	require.NoError(t, json.Unmarshal(raw, &first))
	require.Equal(t, core.AvailabilityAvailable, first.Availability)
	var writes atomic.Int64
	count := func(*gorm.DB) { writes.Add(1) }
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test-read-create", count))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test-read-update", count))
	require.NoError(t, db.Callback().Delete().Before("gorm:delete").Register("test-read-delete", count))
	raw, err = r.Detail(context.Background(), "actor", "primary", core.ReferenceID(ref))
	require.NoError(t, err)
	var next Execution
	require.NoError(t, json.Unmarshal(raw, &next))
	require.True(t, next.ObservedAt.After(first.ObservedAt))
	require.Equal(t, first.SnapshotObservedAt, next.SnapshotObservedAt)
	require.Zero(t, writes.Load())
	svc.unavailable.Store(true)
	raw, err = r.Detail(context.Background(), "actor", "primary", core.ReferenceID(ref))
	require.NoError(t, err)
	var stale Execution
	require.NoError(t, json.Unmarshal(raw, &stale))
	require.Equal(t, core.AvailabilityStale, stale.Availability)
	require.Equal(t, *first.SnapshotObservedAt, stale.ObservedAt)
	require.Zero(t, writes.Load())
	svc.unavailable.Store(false)
	svc.block.Store(true)
	limits := r.connections["primary"].config.Limits
	limits.RPCDeadline = 20 * time.Millisecond
	r.connections["primary"].guard = core.NewReadGuard(r.store, r.fingerprint, limits)
	raw, err = r.Detail(context.Background(), "actor", "primary", core.ReferenceID(ref))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &stale))
	require.Equal(t, core.AvailabilityStale, stale.Availability, "RPC deadline should still serve a dated cache under a fresh epoch check")
	require.Zero(t, writes.Load())
	var snapshots int64
	require.NoError(t, db.Model(&models.ExternalExecutionSnapshot{}).Count(&snapshots).Error)
	require.Equal(t, int64(1), snapshots)
	require.NoError(t, r.store.ActivateConfiguration(context.Background(), r.cfg, r.fingerprint, "changed"))
	before := svc.calls.Load()
	_, err = r.Detail(context.Background(), "actor", "primary", core.ReferenceID(ref))
	require.ErrorIs(t, err, core.ErrEpochMismatch)
	require.Equal(t, before, svc.calls.Load(), "stale nodes must refuse before provider dispatch")
}
func TestClosePreventsObserverCreationWaitingForConnectionLock(t *testing.T) {
	r, _ := testRuntime(t)
	c := r.connections["primary"]
	c.mu.Lock()
	result := make(chan error, 1)
	go func() { _, err := r.observer(context.Background(), c); result <- err }()
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	require.Eventually(t, r.closed.Load, time.Second, time.Millisecond)
	c.mu.Unlock()
	require.ErrorIs(t, <-result, core.ErrEpochMismatch)
	<-closed
	require.Nil(t, c.client)
	require.Nil(t, c.observer)
}

func TestReverseLinksHonorStrictestParticipatingConnectionBudget(t *testing.T) {
	r, db := testRuntime(t)
	jobID, runID := uuid.New(), uuid.New()
	for i := range 2 {
		row := models.ExternalExecutionCorrelation{ID: uuid.New(), OpaqueID: "opaque", ConnectionID: "primary", ActivityID: fmt.Sprint(i), ActivityType: "declared", RelationType: core.RelationDelegation, BindingVersion: "v1", AdmissionID: uuid.New(), JobID: jobID, RunID: &runID, IdempotencyKey: fmt.Sprint(i), Outcome: "created", SourceEventID: 1, Evidence: datatypes.JSON(`{}`), EvidenceDigest: fmt.Sprint(i), VerifiedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now()}
		require.NoError(t, db.Create(&row).Error)
	}
	r.connections["primary"].config.Limits.MaxPageEntries = 1
	_, err := r.RunLinks(context.Background(), "actor", jobID, runID)
	require.ErrorIs(t, err, core.ErrPayloadTooLarge)
	r.connections["primary"].config.Limits.MaxPageEntries = 100
	r.connections["primary"].config.Limits.MaxPageMetadataBytes = 100
	_, err = r.RunLinks(context.Background(), "actor", jobID, runID)
	require.ErrorIs(t, err, core.ErrPayloadTooLarge)
}

func seedRunLink(t *testing.T, db *gorm.DB, connectionID, opaqueID string, jobID, runID uuid.UUID) models.ExternalExecutionCorrelation {
	t.Helper()
	row := models.ExternalExecutionCorrelation{ID: uuid.New(), OpaqueID: opaqueID, ConnectionID: connectionID, ActivityID: uuid.NewString(), ActivityType: "declared", RelationType: core.RelationDelegation, BindingVersion: "v1", AdmissionID: uuid.New(), JobID: jobID, RunID: &runID, IdempotencyKey: uuid.NewString(), Outcome: "created", SourceEventID: 1, Evidence: datatypes.JSON(`{}`), EvidenceDigest: uuid.NewString(), VerifiedAt: time.Now(), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, db.Create(&row).Error)
	return row
}

func TestReverseLinksRetainDisabledAndRemovedConnections(t *testing.T) {
	r, db := testRuntime(t)
	disabled := r.cfg.Connections[0]
	disabled.ID, disabled.Enabled = "disabled", false
	r.cfg.Connections = append(r.cfg.Connections, disabled)
	limits := core.DefaultLimits()
	limits.ReadsPerMinute = 1
	r.connections["primary"].guard = core.NewReadGuard(r.store, r.fingerprint, limits)
	jobID, runID := uuid.New(), uuid.New()
	var expected []models.ExternalExecutionCorrelation
	for i, id := range []string{"disabled", "removed", "primary", "primary"} {
		expected = append(expected, seedRunLink(t, db, id, fmt.Sprint(i), jobID, runID))
	}
	rows, err := r.RunLinks(context.Background(), "actor", jobID, runID)
	require.NoError(t, err, "retired links must not hide active links, regardless of row order")
	require.Len(t, rows, len(expected))
	for i := range expected {
		require.Equal(t, expected[i].ID, rows[i].ID)
	}
	_, err = r.RunLinks(context.Background(), "actor", jobID, runID)
	require.ErrorIs(t, err, core.ErrReadBudget, "duplicate rows charge the active connection only once per request")
	require.Nil(t, r.connections["primary"].client)
	require.Nil(t, r.connections["primary"].observer)
}

func TestReverseLinksHonorDisabledConnectionResponseCeilings(t *testing.T) {
	for _, ceiling := range []string{"entries", "metadata"} {
		t.Run(ceiling, func(t *testing.T) {
			r, db := testRuntime(t)
			disabled := r.cfg.Connections[0]
			disabled.ID, disabled.Enabled = "disabled", false
			if ceiling == "entries" {
				disabled.Limits.MaxPageEntries = 1
			} else {
				disabled.Limits.MaxPageMetadataBytes = 100
			}
			r.cfg.Connections = append(r.cfg.Connections, disabled)
			jobID, runID := uuid.New(), uuid.New()
			seedRunLink(t, db, "disabled", "0", jobID, runID)
			seedRunLink(t, db, "primary", "1", jobID, runID)
			_, err := r.RunLinks(context.Background(), "actor", jobID, runID)
			require.ErrorIs(t, err, core.ErrPayloadTooLarge)
		})
	}
}

func TestReverseLinksWithoutEnabledConnectionsStillUseProcessBudget(t *testing.T) {
	for _, connectionID := range []string{"disabled", "removed", "empty"} {
		t.Run(connectionID, func(t *testing.T) {
			r, db := testRuntime(t)
			disabled := r.cfg.Connections[0]
			disabled.ID, disabled.Enabled = "disabled", false
			r.cfg.Connections = append(r.cfg.Connections, disabled)
			limits := core.DefaultLimits()
			limits.ReadsPerMinute = 1
			r.readBudget = core.NewReadGuard(r.store, r.fingerprint, limits)
			jobID, runID := uuid.New(), uuid.New()
			if connectionID != "empty" {
				seedRunLink(t, db, connectionID, "0", jobID, runID)
			}
			_, err := r.RunLinks(context.Background(), "actor", jobID, runID)
			require.NoError(t, err)
			_, err = r.RunLinks(context.Background(), "actor", jobID, runID)
			require.ErrorIs(t, err, core.ErrReadBudget)
		})
	}
}

func TestServiceObservationGenerationAdvancesFromAnotherNodesStoredFence(t *testing.T) {
	r, _ := testRuntime(t)
	svc := &describeService{}
	observer, err := temporal.NewObserver(svc, "default", "", time.Second, 100, 20<<10)
	require.NoError(t, err)
	r.connections["primary"].observer = observer
	ref, err := core.NewExecutionReference("primary", map[string]string{"workflow_id": "clock-independent", "run_id": "run-1"})
	require.NoError(t, err)
	_, err = r.store.RecordDirect(context.Background(), r.fingerprint, core.Observation{Execution: ref, Generation: 1 << 50, SourceEventID: "1", NativeStatus: "running", DisplayStatus: core.DisplayRunning, Availability: core.AvailabilityAvailable, Completeness: core.CompletenessComplete, Metadata: []byte(`{}`), SourceKind: core.SourceDirect, ObservedAt: time.Now().Add(time.Hour)}, r.connections["primary"].config.Limits)
	require.NoError(t, err)
	_, err = r.Detail(context.Background(), "actor", "primary", core.ReferenceID(ref))
	require.NoError(t, err)
	var identity models.ExternalExecution
	require.NoError(t, r.db.First(&identity, "opaque_id = ?", ref.OpaqueID()).Error)
	require.Equal(t, int64(1<<50)+1, *identity.LatestGeneration)
}

func TestConnectionBrowserMetadataUsesConfiguredValues(t *testing.T) {
	r, _ := testRuntime(t)
	r.cfg.Connections[0].UIURL = "https://temporal-ui.example.test/console"
	r.cfg.Connections[0].Limits.ConsoleRefresh = 30 * time.Second
	view, err := r.Connections(context.Background(), "viewer")
	require.NoError(t, err)
	require.Len(t, view.Connections, 1)
	require.Equal(t, "https://temporal-ui.example.test/console", view.Connections[0].UIURL)
	require.Equal(t, int64(30000), view.Connections[0].ConsoleRefreshMS)
}

func TestCorrelationHTTPIdentityRequiresMatchingPersistedEvidence(t *testing.T) {
	ref, err := core.NewExecutionReference("primary", map[string]string{"workflow_id": "publication", "run_id": "pinned-run"})
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{"execution": ref})
	require.NoError(t, err)
	original := models.ExternalExecutionCorrelation{ConnectionID: ref.ConnectionID, OpaqueID: ref.OpaqueID(), Evidence: raw}
	for _, name := range []string{"matching", "wrong connection", "wrong catalog digest", "malformed evidence", "missing evidence", "missing run", "extra coordinate"} {
		t.Run(name, func(t *testing.T) {
			row := original
			switch name {
			case "wrong connection":
				row.ConnectionID = "other"
			case "wrong catalog digest":
				row.OpaqueID = "other"
			case "malformed evidence":
				row.Evidence = []byte(`{"execution":`)
			case "missing evidence":
				row.Evidence = []byte(`{}`)
			case "missing run", "extra coordinate":
				coordinates := map[string]string{"workflow_id": "publication"}
				if name == "extra coordinate" {
					coordinates["run_id"], coordinates["extra"] = "pinned-run", "unexpected"
				}
				invalid, e := core.NewExecutionReference("primary", coordinates)
				require.NoError(t, e)
				row.OpaqueID = invalid.OpaqueID()
				row.Evidence, e = json.Marshal(map[string]any{"execution": invalid})
				require.NoError(t, e)
			}
			views := correlationViews([]models.ExternalExecutionCorrelation{row})
			require.Len(t, views, 1)
			require.Equal(t, row, views[0].ExternalExecutionCorrelation)
			if name == "matching" {
				require.Equal(t, core.ReferenceID(ref), views[0].ExecutionID)
				require.Equal(t, httpReference(ref), *views[0].Reference)
				require.NotEqual(t, row.OpaqueID, views[0].ExecutionID)
			} else {
				require.Empty(t, views[0].ExecutionID)
				require.Nil(t, views[0].Reference, "invalid historical identity must not create a navigable target")
			}
		})
	}
}

func TestReverseLinkBudgetIncludesHTTPNavigationIdentity(t *testing.T) {
	r, db := testRuntime(t)
	jobID, runID := uuid.New(), uuid.New()
	ref, err := core.NewExecutionReference("primary", map[string]string{"workflow_id": "publication", "run_id": "pinned-run"})
	require.NoError(t, err)
	row := seedRunLink(t, db, ref.ConnectionID, ref.OpaqueID(), jobID, runID)
	row.Evidence, err = json.Marshal(map[string]any{"execution": ref})
	require.NoError(t, err)
	require.NoError(t, db.Model(&row).Update("evidence", row.Evidence).Error)
	oldEnvelope, err := json.Marshal(map[string]any{"executions": []models.ExternalExecutionCorrelation{row}})
	require.NoError(t, err)
	r.connections["primary"].config.Limits.MaxPageMetadataBytes = len(oldEnvelope)
	_, err = r.RunLinks(t.Context(), "reader", jobID, runID)
	require.ErrorIs(t, err, core.ErrPayloadTooLarge, "the additive navigation fields must fit the complete response budget")
}

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
	unavailable atomic.Bool
	block       atomic.Bool
	calls       atomic.Int64
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

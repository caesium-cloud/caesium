//go:build integration

package test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	coperations "github.com/caesium-cloud/caesium/internal/connector/operations"
	ctemporal "github.com/caesium-cloud/caesium/internal/connector/temporal"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"go.temporal.io/api/workflowservice/v1"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type operationFixtureProvider struct {
	*ctemporal.OperationProvider
	suite *IntegrationTestSuite
	fault string
}

func (p *operationFixtureProvider) SubmitOperation(ctx context.Context, d coperations.OperationDispatch) (coperations.OperationEvidence, error) {
	if p.fault != "" {
		p.suite.messageFixtureControl(http.MethodPost, "/fault", map[string]any{"update_id": d.ExternalUpdateID, "mode": p.fault}, nil)
		p.fault = ""
	}
	return p.OperationProvider.SubmitOperation(ctx, d)
}

// TestConnectorsOperations qualifies D2's durable component against actual
// Temporal/mTLS and authenticated principals. It deliberately does not claim a
// live Caesium process restart, public catalog persistence or C2 HTTP proof.
func (s *IntegrationTestSuite) TestConnectorsOperations() {
	if os.Getenv("CAESIUM_CONNECTORS_FIXTURE") != "true" {
		s.T().Skip("requires explicitly selected real connector fixture")
	}
	s.Require().NotEmpty(s.authAPIKey)
	artifacts := os.Getenv("CAESIUM_CONNECTORS_ARTIFACTS")
	s.Require().NotEmpty(artifacts)
	candidate := os.Getenv("CAESIUM_CONNECTORS_CANDIDATE_SHA")
	s.Require().Len(candidate, 40)
	var readiness map[string]any
	s.messageFixtureControl(http.MethodGet, "/health", nil, &readiness)
	s.Require().Equal(true, readiness["ready"])
	s.Require().Equal("1.49.0", readiness["sdk"])
	s.Require().Equal("1.9.1", readiness["cli"])
	s.Require().Equal("1.32.0", readiness["server"])
	var startup []struct {
		Kind      string `json:"kind"`
		Candidate string `json:"candidate"`
		SDK       string `json:"sdk"`
		CLI       string `json:"cli"`
		Server    string `json:"server"`
	}
	s.messageFixtureControl(http.MethodGet, "/evidence", nil, &startup)
	started := false
	for _, event := range startup {
		if event.Kind == "fixture_started" {
			s.Require().Equal(candidate, event.Candidate)
			s.Require().Equal("1.49.0", event.SDK)
			s.Require().Equal("1.9.1", event.CLI)
			s.Require().Equal("1.32.0", event.Server)
			started = true
		}
	}
	s.Require().True(started, "candidate-bound observed startup journal is required")
	configPath := os.Getenv("CAESIUM_CONNECTORS_FIXTURE_CONFIG")
	if configPath == "" {
		configPath = filepath.Join(s.projectRoot, "test", "connectors", "testdata", "connections.yaml")
	}
	raw, err := os.ReadFile(configPath)
	s.Require().NoError(err)
	config, err := connector.Parse(raw, nil)
	s.Require().NoError(err)
	s.Require().Len(config.Connections, 1)
	connection := config.Connections[0]
	epoch, err := connector.Fingerprint(config)
	s.Require().NoError(err)
	c, err := ctemporal.Dial(s.T().Context(), ctemporal.DialConfig{Endpoint: connection.Endpoint, Namespace: connection.Scope, CertificatePaths: connection.CertificatePaths})
	s.Require().NoError(err)
	s.T().Cleanup(c.Close)
	versionContext, versionCancel := context.WithTimeout(s.T().Context(), 3*time.Second)
	system, err := c.WorkflowService().GetSystemInfo(versionContext, &workflowservice.GetSystemInfoRequest{})
	versionCancel()
	s.Require().NoError(err)
	s.Require().Equal("1.32.0", system.GetServerVersion())
	observer, err := ctemporal.NewObserverFromClient(c, connection.Scope, "", 3*time.Second, 100, 0)
	s.Require().NoError(err)
	messenger, err := ctemporal.NewMessengerFromClient(c, connection.ID, connection.Scope, connection.Bindings, "", 3*time.Second, 0)
	s.Require().NoError(err)
	operator := s.createAPIKeyCLI("--role", "operator", "--description", "durable connector operations operator")
	otherOperator := s.createAPIKeyCLI("--role", "operator", "--description", "durable connector operations other operator")
	principalFor := func(key createdAPIKey) *coperations.OperationPrincipal {
		actor := s.messageFixtureActor(key, "authenticate-durable-component")
		id, e := uuid.Parse(actor.StableID)
		s.Require().NoError(e)
		return &coperations.OperationPrincipal{Kind: actor.PrincipalKind, StableID: id.String(), Subject: actor.Subject, Role: actor.Role}
	}
	principal, other := principalFor(operator), principalFor(otherOperator)
	path := filepath.Join(artifacts, "operations-component-catalog.db")
	open := func() *gorm.DB {
		db, e := gorm.Open(sqlite.Open("file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"), &gorm.Config{})
		s.Require().NoError(e)
		return db
	}
	db := open()
	s.Require().NoError(db.AutoMigrate(&models.ExternalExecution{}, &models.ExternalExecutionSnapshot{}, &models.ConnectorOperation{}, &models.ConnectorConfiguration{}))
	closeDB := func() { sqlDB, e := db.DB(); s.Require().NoError(e); s.Require().NoError(sqlDB.Close()) }
	s.T().Cleanup(func() {
		sqlDB, e := db.DB()
		if e == nil {
			_ = sqlDB.Close()
		}
	})
	var catalog *connector.Store
	var store *coperations.Store
	var service *coperations.Service
	var provider *operationFixtureProvider
	construct := func() {
		catalog = connector.NewStore(db)
		store = coperations.NewStore(db)
		provider = &operationFixtureProvider{OperationProvider: &ctemporal.OperationProvider{Messenger: messenger}, suite: s}
		service, err = coperations.NewService(store, provider, connection, epoch)
		s.Require().NoError(err)
	}
	construct()
	s.Require().NoError(catalog.ActivateEpoch(s.T().Context(), "", epoch))
	reopen := func() {
		closeDB()
		db = open()
		messenger, err = ctemporal.NewMessengerFromClient(c, connection.ID, connection.Scope, connection.Bindings, "", 3*time.Second, 0)
		s.Require().NoError(err)
		construct()
	}
	requestFor := func(label string, eligible bool) (coperations.OperationSubmission, messageFixtureRef, ctemporal.MessageTarget) {
		ref := s.messageFixtureStart(c, "durable-"+label, "publication", eligible)
		target := messageFixtureTarget(connection, ref)
		s.messageFixtureAwait(messenger, target, func(state messageFixtureState) bool { return state.Stage == "awaiting_approval" })
		observed, e := observer.Describe(s.T().Context(), ref.WorkflowID, ref.RunID)
		s.Require().NoError(e)
		execution, e := connector.NewExecutionReference(connection.ID, observed.Coordinates)
		s.Require().NoError(e)
		metadata, e := json.Marshal(observed)
		s.Require().NoError(e)
		_, e = catalog.RecordSnapshot(s.T().Context(), connector.Observation{Execution: execution, Generation: 1, NativeStatus: observed.NativeStatus, DisplayStatus: observed.DisplayStatus, Availability: connector.AvailabilityAvailable, Completeness: connector.CompletenessComplete, Metadata: metadata, SourceKind: connector.SourceDirect, ObservedAt: observed.ObservedAt})
		s.Require().NoError(e, "component catalog must ingest an actual direct provider observation, never seeded application rows")
		return coperations.OperationSubmission{IdempotencyKey: label, Target: coperations.OperationTarget{Execution: execution, BindingName: "publication", BindingVersion: "1"}, Action: "approve_publication", Input: map[string]any{"note": "durable component approval"}, Principal: principal}, ref, target
	}
	completed := func(id uuid.UUID) models.ConnectorOperation {
		var op models.ConnectorOperation
		var e error
		s.Require().Eventually(func() bool {
			op, e = service.Reconcile(s.T().Context(), id)
			return e == nil && op.State == connector.OperationCompleted
		}, 20*time.Second, 200*time.Millisecond, "durable same-ID lookup did not complete: %v", e)
		s.Require().Nil(op.OpenKey)
		return op
	}
	var receipts []models.ConnectorOperation
	// Real lost accepted response, close/reopen the file store, reconstruct both
	// adapter/service, then recover by lookup without another Update RPC.
	accepted, acceptedRef, acceptedTarget := requestFor("lost-accepted", true)
	provider.fault = "accepted"
	admitted, err := service.Submit(s.T().Context(), accepted)
	s.Require().ErrorIs(err, coperations.ErrOperationProvider)
	s.Require().True(admitted.Created)
	s.Require().Equal(connector.OperationUnknown, admitted.Operation.State)
	acceptedID := admitted.Operation.ExternalUpdateID
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", acceptedID, ""))
	reopen()
	acceptedReceipt := completed(admitted.Operation.ID)
	receipts = append(receipts, acceptedReceipt)
	s.messageFixtureAwait(messenger, acceptedTarget, func(state messageFixtureState) bool { return state.Stage == "published" })
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", acceptedID, ""))
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", acceptedRef.RunID))
	changed := accepted
	changed.Input = map[string]any{"note": "changed"}
	_, err = service.Submit(s.T().Context(), changed)
	s.Require().ErrorIs(err, connector.ErrIdempotencyMismatch)
	replay, err := service.Submit(s.T().Context(), accepted)
	s.Require().NoError(err)
	s.Require().False(replay.Created)
	// A genuinely new terminal receipt/Update ID still has one logical effect.
	fresh := accepted
	fresh.IdempotencyKey = "terminal-new-id"
	fresh.Principal = other
	again, err := service.Submit(s.T().Context(), fresh)
	s.Require().NoError(err)
	s.Require().True(again.Created)
	s.Require().NotEqual(acceptedID, again.Operation.ExternalUpdateID)
	receipts = append(receipts, completed(again.Operation.ID))
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", acceptedRef.RunID))
	s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{}, nil)
	// Crash after durable admission but before any external send. Restart only
	// looks up; NotFound leaves the guard until the original principal confirms.
	crash, crashRef, crashTarget := requestFor("admission-crash", true)
	admitted, err = service.Admit(s.T().Context(), crash)
	s.Require().NoError(err)
	crashID := admitted.Operation.ExternalUpdateID
	reopen()
	unknown, err := service.Reconcile(s.T().Context(), admitted.Operation.ID)
	s.Require().ErrorIs(err, coperations.ErrOperationProvider)
	s.Require().Equal(connector.OperationUnknown, unknown.State)
	s.Require().NotNil(unknown.OpenKey)
	s.Require().Zero(messageFixtureCount(s.messageFixtureEvents(), "update_rpc", crashID, ""))
	s.Require().Zero(messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", crashRef.RunID))
	_, err = service.ConfirmResend(s.T().Context(), unknown.ID, unknown.Revision, principal, true)
	s.Require().NoError(err)
	receipts = append(receipts, completed(unknown.ID))
	s.messageFixtureAwait(messenger, crashTarget, func(state messageFixtureState) bool { return state.Stage == "published" })
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", crashID, ""))
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", crashRef.RunID))
	// Lost validator rejection then eligibility: not-found does not authorize a
	// send on restart, on replay, or under a different fresh key/principal.
	rejected, rejectedRef, rejectedTarget := requestFor("lost-rejection", false)
	provider.fault = "rejected"
	admitted, err = service.Submit(s.T().Context(), rejected)
	s.Require().ErrorIs(err, coperations.ErrOperationProvider)
	s.Require().Equal(connector.OperationUnknown, admitted.Operation.State)
	rejectedID := admitted.Operation.ExternalUpdateID
	reopen()
	s.messageFixtureControl(http.MethodPost, "/signal", map[string]any{"id": rejectedRef.WorkflowID, "run_id": rejectedRef.RunID, "name": "fixture_eligibility", "value": true}, nil)
	s.messageFixtureAwait(messenger, rejectedTarget, func(state messageFixtureState) bool { return state.Eligible })
	for range 2 {
		unknown, err = service.Reconcile(s.T().Context(), admitted.Operation.ID)
		s.Require().ErrorIs(err, coperations.ErrOperationProvider)
		s.Require().Equal(connector.OperationUnknown, unknown.State)
		s.Require().NotNil(unknown.OpenKey)
		s.Require().Contains(string(unknown.ProviderEvidence), "NotFound")
	}
	replay, err = service.Submit(s.T().Context(), rejected)
	s.Require().ErrorIs(err, coperations.ErrOperationProvider)
	unknown = replay.Operation
	s.Require().False(replay.Created)
	bypass := rejected
	bypass.IdempotencyKey = "second-operator"
	bypass.Principal = other
	conflict, err := service.Submit(s.T().Context(), bypass)
	s.Require().NoError(err)
	s.Require().True(conflict.Conflict)
	s.Require().Equal(admitted.Operation.ID, conflict.Operation.ID)
	_, err = service.ConfirmResend(s.T().Context(), unknown.ID, unknown.Revision, other, true)
	s.Require().ErrorIs(err, coperations.ErrOperationForbidden)
	_, err = service.ConfirmResend(s.T().Context(), unknown.ID, unknown.Revision, principal, false)
	s.Require().ErrorIs(err, coperations.ErrOperationConfirmation)
	state := s.messageFixtureAwait(messenger, rejectedTarget, func(state messageFixtureState) bool { return state.Eligible && !state.Approved })
	s.Require().Zero(state.ApprovalEffects)
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", rejectedID, ""))
	s.Require().Zero(messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", rejectedRef.RunID))
	_, err = service.ConfirmResend(s.T().Context(), unknown.ID, unknown.Revision, principal, true)
	s.Require().NoError(err)
	confirmed := completed(unknown.ID)
	s.messageFixtureAwait(messenger, rejectedTarget, func(state messageFixtureState) bool { return state.Stage == "published" })
	receipts = append(receipts, confirmed)
	s.Require().Equal(rejectedID, confirmed.ExternalUpdateID)
	s.Require().Equal(admitted.Operation.Actor, confirmed.Actor)
	s.Require().Equal(admitted.Operation.Input, confirmed.Input)
	s.Require().Equal(uint64(2), confirmed.Attempt)
	s.Require().Contains(string(confirmed.ResendAudit), principal.StableID)
	s.Require().Equal(2, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", rejectedID, ""))
	s.Require().Equal(1, messageFixtureCount(s.messageFixtureEvents(), "approval_effect", "", rejectedRef.RunID))
	reopen()
	_, err = service.Reconcile(s.T().Context(), confirmed.ID)
	s.Require().NoError(err)
	_, err = service.Submit(s.T().Context(), rejected)
	s.Require().NoError(err)
	s.Require().Equal(2, messageFixtureCount(s.messageFixtureEvents(), "update_rpc", rejectedID, ""))
	events := s.messageFixtureEvents()
	var acceptedLoss, rejectedLoss bool
	for _, event := range events {
		if event.Kind == "update_rpc" && event.UpdateID == acceptedID && event.ReplyLost && !event.Rejected && event.RPCCode == "OK" {
			acceptedLoss = true
		}
		if event.Kind == "update_rpc" && event.UpdateID == rejectedID && event.ReplyLost && event.Rejected && event.RPCCode == "OK" {
			rejectedLoss = true
		}
	}
	s.Require().True(acceptedLoss, "must drop an actual accepted reply")
	s.Require().True(rejectedLoss, "must drop an actual validator rejection reply")
	evidence, err := json.MarshalIndent(map[string]any{"candidate_sha": candidate, "qualification": "durable component recovery; not a live Caesium process/catalog restart", "sdk": readiness["sdk"], "server": readiness["server"], "receipts": receipts, "events": events}, "", "  ")
	s.Require().NoError(err)
	s.Require().NotContains(string(evidence), operator.Plaintext)
	s.Require().NotContains(string(evidence), otherOperator.Plaintext)
	s.Require().NoError(os.WriteFile(filepath.Join(artifacts, "operations-component-evidence.json"), append(evidence, '\n'), 0600))
	s.messageFixtureControl(http.MethodPost, "/fault", map[string]any{}, nil)
}

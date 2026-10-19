package operations

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type operationTestProvider struct {
	mu             sync.Mutex
	sends, lookups []OperationDispatch
	stableID       func(string) (string, error)
	submit         func(OperationDispatch) (OperationEvidence, error)
	lookup         func(OperationDispatch) (OperationEvidence, error)
}

func (p *operationTestProvider) StableOperationID(id string) (string, error) {
	p.mu.Lock()
	fn := p.stableID
	p.mu.Unlock()
	if fn != nil {
		return fn(id)
	}
	return "fake-" + id, nil
}
func (p *operationTestProvider) SubmitOperation(_ context.Context, d OperationDispatch) (OperationEvidence, error) {
	p.mu.Lock()
	p.sends = append(p.sends, d)
	fn := p.submit
	p.mu.Unlock()
	if fn != nil {
		return fn(d)
	}
	return OperationEvidence{State: connector.OperationUnknown, RPCCode: "DeadlineExceeded"}, errors.New("lost reply")
}
func (p *operationTestProvider) LookupOperation(_ context.Context, d OperationDispatch) (OperationEvidence, error) {
	p.mu.Lock()
	p.lookups = append(p.lookups, d)
	fn := p.lookup
	p.mu.Unlock()
	if fn != nil {
		return fn(d)
	}
	return OperationEvidence{State: connector.OperationUnknown, RPCCode: "NotFound"}, errors.New("absent")
}
func operationTestPrincipal() *OperationPrincipal {
	return &OperationPrincipal{Kind: connector.PrincipalKindAPIKey, StableID: uuid.NewString(), Subject: "key-prefix", Role: "operator"}
}
func operationTestConnection() connector.Connection {
	return connector.Connection{ID: "primary", Enabled: true, Provider: "test", Bindings: []connector.Binding{{Name: "publication", Version: "1", Actions: []connector.Action{{Name: "approve", InputSchema: json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"}},"required":["note"],"additionalProperties":false}`), ResultSchema: json.RawMessage(`{"type":"object","properties":{"approved":{"type":"boolean"}},"required":["approved"],"additionalProperties":false}`)}}}}}
}
func operationTestService(t *testing.T, store *Store, p OperationProvider) *Service {
	t.Helper()
	s, e := NewService(store, p, operationTestConnection(), "epoch")
	require.NoError(t, e)
	return s
}
func operationTestRequest(t *testing.T, store *Store) OperationSubmission {
	t.Helper()
	ref := mustExecutionRef(t, "primary", "pinned-run")
	requireDirect(t, store, ref)
	return OperationSubmission{IdempotencyKey: "key", Target: OperationTarget{Execution: ref, BindingName: "publication", BindingVersion: "1"}, Action: "approve", Input: map[string]any{"note": "reviewed"}, Principal: operationTestPrincipal()}
}
func TestOperationsConcurrentAdmissionAndOpenGuard(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s := operationTestService(t, store, p)
	r := operationTestRequest(t, store)
	const n = 12
	decisions := make(chan connector.OperationDecision, n)
	errs := make(chan error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; d, e := s.Submit(t.Context(), r); decisions <- d; errs <- e }()
	}
	close(start)
	wg.Wait()
	close(decisions)
	close(errs)
	var id uuid.UUID
	created := 0
	for d := range decisions {
		if id == uuid.Nil {
			id = d.Operation.ID
		}
		require.Equal(t, id, d.Operation.ID)
		if d.Created {
			created++
		}
		require.False(t, d.Conflict)
	}
	require.Equal(t, 1, created)
	for e := range errs {
		if e != nil {
			require.True(t, errors.Is(e, ErrOperationProvider) || errors.Is(e, ErrOperationPending) || errors.Is(e, ErrOperationChanged), "unexpected concurrent result: %v", e)
		}
	}
	require.Len(t, p.sends, 1)
	require.Equal(t, id.String(), p.sends[0].Actor.OperationID)
	require.Equal(t, "fake-"+id.String(), p.sends[0].ExternalUpdateID)
	// A second operator with a different key receives the open receipt.
	other := r
	other.IdempotencyKey = "other"
	other.Principal = operationTestPrincipal()
	d, e := s.Submit(t.Context(), other)
	require.NoError(t, e)
	require.True(t, d.Conflict)
	require.Equal(t, id, d.Operation.ID)
	require.Len(t, p.sends, 1)
	// A changed principal or input cannot reuse the original key.
	other.IdempotencyKey = r.IdempotencyKey
	_, e = s.Submit(t.Context(), other)
	require.ErrorIs(t, e, connector.ErrIdempotencyMismatch)
	changed := r
	changed.Input = map[string]any{"note": "changed"}
	_, e = s.Submit(t.Context(), changed)
	require.ErrorIs(t, e, connector.ErrIdempotencyMismatch)
	rows, e := store.Operations(t.Context(), r.Target.Execution, 100)
	require.NoError(t, e)
	require.Len(t, rows, 1)
}
func TestOperationsAdmissionCrashReopenLookupOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	db := openSQLiteFile(t, path)
	migrateStore(t, db)
	store := NewStore(db)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s := operationTestService(t, store, p)
	r := operationTestRequest(t, store)
	d, e := s.Admit(t.Context(), r)
	require.NoError(t, e)
	require.True(t, d.Created)
	require.Empty(t, p.sends)
	closeSQLite(t, db)
	db = openSQLiteFile(t, path)
	store = NewStore(db)
	s = operationTestService(t, store, p)
	op, e := s.Reconcile(t.Context(), d.Operation.ID)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Equal(t, connector.OperationUnknown, op.State)
	require.NotNil(t, op.OpenKey)
	require.Equal(t, uint64(1), op.Attempt)
	require.Empty(t, p.sends)
	require.Len(t, p.lookups, 1)
	require.Nil(t, p.lookups[0].Input)
	require.Empty(t, p.lookups[0].Actor.OperationID)
	// Restart/repeated lookup and same-key replay never send.
	for range 2 {
		op, e = s.Reconcile(t.Context(), op.ID)
		require.ErrorIs(t, e, ErrOperationProvider)
	}
	replay, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	op = replay.Operation
	require.False(t, replay.Created)
	require.Empty(t, p.sends)
	_, e = s.ConfirmResend(t.Context(), op.ID, op.Revision, r.Principal, false)
	require.ErrorIs(t, e, ErrOperationConfirmation)
	_, e = s.ConfirmResend(t.Context(), op.ID, op.Revision, operationTestPrincipal(), true)
	require.ErrorIs(t, e, ErrOperationForbidden)
	denied := *r.Principal
	denied.Role = "viewer"
	_, e = s.ConfirmResend(t.Context(), op.ID, op.Revision, &denied, true)
	require.ErrorIs(t, e, ErrOperationForbidden)
	p.submit = func(d OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	op, e = s.ConfirmResend(t.Context(), op.ID, op.Revision, r.Principal, true)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, op.State)
	require.Nil(t, op.OpenKey)
	require.Equal(t, uint64(2), op.Attempt)
	require.Len(t, p.sends, 1)
	require.Equal(t, d.Operation.ExternalUpdateID, p.sends[0].ExternalUpdateID)
	require.JSONEq(t, string(d.Operation.Input), `{"note":"reviewed"}`)
	require.Equal(t, d.Operation.Actor, op.Actor)
	require.Contains(t, string(op.ResendAudit), r.Principal.StableID)
	_, e = s.ConfirmResend(t.Context(), op.ID, op.Revision, r.Principal, true)
	require.ErrorIs(t, e, ErrOperationConfirmation)
	_, e = s.Reconcile(t.Context(), op.ID)
	require.NoError(t, e)
	require.Len(t, p.lookups, 4)
}

func TestOperationsSubmitPublishesAdmissionWithInitialClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic-admission.db")
	db := openSQLiteFile(t, path)
	migrateStore(t, db)
	observer := openSQLiteFile(t, path)
	store := NewStore(db)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{submit: func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationAccepted}, nil
	}}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce, pauseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("operations:test_initial_claim_publication", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if ok && tx.Statement.Table == "connector_operations" && values["revision"] == uint64(2) && values["retryable_unsent"] == false {
			if _, evidence := values["provider_evidence"]; !evidence {
				pauseOnce.Do(func() { close(entered); <-release })
			}
		}
	}))
	result := make(chan error, 1)
	go func() { _, err := service.Submit(t.Context(), request); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Submit did not reach its initial dispatch claim")
	}
	// A separate WAL connection can read committed receipts during this pause.
	// Publishing admission first exposes revision 1 and permits a lookup to
	// invalidate its pending claim; atomic admission leaves no visible row.
	var visible int64
	readErr := observer.Model(&models.ConnectorOperation{}).Where("idempotency_key = ?", request.IdempotencyKey).Count(&visible).Error
	unblock()
	require.NoError(t, <-result)
	require.NoError(t, readErr)
	require.Zero(t, visible, "Submit exposed an admission before durably claiming its initial send")
	var operation models.ConnectorOperation
	require.NoError(t, observer.Where("idempotency_key = ?", request.IdempotencyKey).First(&operation).Error)
	require.Equal(t, connector.OperationAccepted, operation.State)
	require.Greater(t, operation.Revision, uint64(1))
	require.False(t, operation.RetryableUnsent)
	require.Len(t, provider.sends, 1)
}

func TestOperationsConflictingLookupCannotSuppressInitialDispatch(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var validations atomic.Int32
	provider.stableID = func(id string) (string, error) {
		// The second validation occurs after admission and immediately before
		// Submit's dispatch. Hold it while another operator recovers the receipt.
		if validations.Add(1) == 2 {
			close(entered)
			<-release
		}
		return "fake-" + id, nil
	}
	var dispatched atomic.Bool
	provider.submit = func(OperationDispatch) (OperationEvidence, error) {
		dispatched.Store(true)
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	provider.lookup = func(OperationDispatch) (OperationEvidence, error) {
		if dispatched.Load() {
			return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
		}
		return OperationEvidence{State: connector.OperationUnknown, RPCCode: "NotFound"}, errors.New("Update has not been sent")
	}
	result := make(chan error, 1)
	go func() { _, err := service.Submit(t.Context(), request); result <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Submit did not reach post-admission dispatch validation")
	}
	other := request
	other.IdempotencyKey = "other-operator-key"
	other.Principal = operationTestPrincipal()
	conflict, err := service.Submit(t.Context(), other)
	require.NoError(t, err)
	require.True(t, conflict.Conflict)
	unknown, err := service.Reconcile(t.Context(), conflict.Operation.ID)
	require.ErrorIs(t, err, ErrOperationProvider)
	require.Equal(t, connector.OperationUnknown, unknown.State)
	require.False(t, unknown.RetryableUnsent)
	unblock()
	// The lookup can supersede initial evidence through the existing CAS, but
	// it must not revoke the winner's already committed one-send authorization.
	require.ErrorIs(t, <-result, ErrOperationChanged)
	require.Len(t, provider.sends, 1, "a conflicting receipt lookup suppressed the admitted operator's only send")
	require.Greater(t, conflict.Operation.Revision, uint64(1))
	recovered, err := service.Reconcile(t.Context(), unknown.ID)
	require.NoError(t, err)
	require.Equal(t, connector.OperationCompleted, recovered.State)
	require.Equal(t, conflict.Operation.ExternalUpdateID, recovered.ExternalUpdateID)
	require.Equal(t, uint64(1), recovered.Attempt)
	require.Nil(t, recovered.OpenKey)
	require.Len(t, provider.sends, 1)
}

func TestOperationsEvidenceMappingPreservesUncertainty(t *testing.T) {
	cases := []struct {
		name   string
		result OperationEvidence
		lookup bool
		want   string
	}{
		{"fresh refusal", OperationEvidence{State: connector.OperationNotDispatched, PreflightRPCCode: "NotFound"}, false, connector.OperationNotDispatched},
		{"lookup refusal", OperationEvidence{State: connector.OperationNotDispatched, PreflightRPCCode: "NotFound"}, true, connector.OperationUnknown},
		{"submission failure", OperationEvidence{State: connector.OperationUnknown, FailureKind: "failure_origin_unknown"}, false, connector.OperationUnknown},
		{"lookup failure", OperationEvidence{State: connector.OperationUnknown, FailureKind: "failure_origin_unknown"}, true, connector.OperationUnknown},
		{"unproven rejection", OperationEvidence{State: connector.OperationRejected}, false, connector.OperationUnknown},
		{"proved rejection", OperationEvidence{State: connector.OperationRejected, RejectionProof: "validator_rejected"}, false, connector.OperationRejected},
		{"invalid result", OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"arbitrary":"data"}`)}, false, connector.OperationUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(openStoreDB(t))
			require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
			p := &operationTestProvider{}
			p.submit = func(OperationDispatch) (OperationEvidence, error) { return tc.result, nil }
			p.lookup = p.submit
			s := operationTestService(t, store, p)
			r := operationTestRequest(t, store)
			var d connector.OperationDecision
			var e error
			if tc.lookup {
				d, e = s.Admit(t.Context(), r)
				require.NoError(t, e)
				d.Operation, e = s.Reconcile(t.Context(), d.Operation.ID)
			} else {
				d, e = s.Submit(t.Context(), r)
			}
			require.NoError(t, e)
			require.Equal(t, tc.want, d.Operation.State)
			require.Equal(t, operationTerminal(tc.want), d.Operation.OpenKey == nil)
			if operationTerminal(tc.want) {
				_, e = s.Submit(t.Context(), r)
				require.NoError(t, e)
				require.Len(t, p.sends, 1)
			}
		})
	}
}
func TestOperationsResendAndEvidenceCAS(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s := operationTestService(t, store, p)
	r := operationTestRequest(t, store)
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	before := d.Operation
	// A lookup begun before confirmation cannot override the new dispatch result.
	p.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	op, e := s.ConfirmResend(t.Context(), before.ID, before.Revision, r.Principal, true)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, op.State)
	stale, e := store.recordOperationEvidence(t.Context(), before, connector.OperationUnknown, []byte(`{"state":"unknown"}`))
	require.ErrorIs(t, e, ErrOperationChanged)
	require.Equal(t, connector.OperationCompleted, stale.State)
	require.Nil(t, stale.OpenKey)
	// Configuration changes cannot resend or lookup an older snapshot.
	newer := r
	newer.IdempotencyKey = "new"
	d, e = s.Admit(t.Context(), newer)
	require.NoError(t, e)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "epoch", "next"))
	_, e = s.Reconcile(t.Context(), d.Operation.ID)
	require.ErrorIs(t, e, connector.ErrEpochMismatch)
	require.Len(t, p.lookups, 0)
	require.Len(t, p.sends, 2)
}
func TestOperationsConcurrentExplicitConfirmationsSendOnce(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s := operationTestService(t, store, p)
	r := operationTestRequest(t, store)
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	entered := make(chan struct{})
	release := make(chan struct{})
	p.submit = func(OperationDispatch) (OperationEvidence, error) {
		close(entered)
		<-release
		return OperationEvidence{State: connector.OperationAccepted}, nil
	}
	done := make(chan error)
	go func() {
		_, e := s.ConfirmResend(context.Background(), d.Operation.ID, d.Operation.Revision, r.Principal, true)
		done <- e
	}()
	<-entered
	_, e = s.ConfirmResend(t.Context(), d.Operation.ID, d.Operation.Revision, r.Principal, true)
	require.ErrorIs(t, e, ErrOperationConfirmation)
	close(release)
	require.NoError(t, <-done)
	require.Len(t, p.sends, 2)
}
func TestOperationsInputCredentialAndRoleRefusals(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s, e := NewService(store, p, operationTestConnection(), "epoch", "tiny")
	require.NoError(t, e)
	r := operationTestRequest(t, store)
	for _, note := range []string{"tiny", "-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----"} {
		bad := r
		bad.Input = map[string]any{"note": note}
		_, e = s.Submit(t.Context(), bad)
		require.ErrorIs(t, e, ErrSensitiveOperation)
	}
	bad := r
	bad.Input = map[string]any{connector.ReservedActorField: map[string]any{}, "note": "reviewed"}
	_, e = s.Submit(t.Context(), bad)
	require.ErrorIs(t, e, connector.ErrInvalidPayload)
	for _, scope := range [][]byte{[]byte(`{"jobs":["local"]}`), []byte(`{"agent":{"incident_id":"00000000-0000-0000-0000-000000000000"}}`), []byte(`{`)} {
		principal := *r.Principal
		principal.Scope = scope
		bad = r
		bad.Principal = &principal
		_, e = s.Submit(t.Context(), bad)
		require.ErrorIs(t, e, ErrOperationForbidden)
	}
	require.Empty(t, p.sends)
	require.Equal(t, int64(0), countModel(t, store.db, &models.ConnectorOperation{}))
	// Results with known credential bytes are withheld, keeping the guard open.
	p.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true,"secret":"tiny"}`), NativeStage: "tiny"}, errors.New("contains tiny")
	}
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Equal(t, connector.OperationUnknown, d.Operation.State)
	require.NotContains(t, string(d.Operation.ProviderEvidence), "tiny")
}

func TestOperationsRefuseEscapedSecretsAndUnboundedKeys(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	r := operationTestRequest(t, store)
	for _, secret := range []string{"token<prod>", "quote\"token", "back\\slash", "two\nlines", "amp&token"} {
		s, e := NewService(store, p, operationTestConnection(), "epoch", secret)
		require.NoError(t, e)
		bad := r
		bad.Input = map[string]any{"note": secret}
		_, e = s.Submit(t.Context(), bad)
		require.ErrorIs(t, e, ErrSensitiveOperation)
		bad = r
		bad.IdempotencyKey = secret
		_, e = s.Submit(t.Context(), bad)
		require.ErrorIs(t, e, ErrSensitiveOperation)
		raw, e := json.Marshal(map[string]any{"nested": map[string]any{secret: "value"}})
		require.NoError(t, e)
		require.ErrorIs(t, s.nonsecret(raw), ErrSensitiveOperation)
	}
	s := operationTestService(t, store, p)
	r.IdempotencyKey = string(make([]byte, 257))
	_, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, connector.ErrInvalidOperation)
	require.Empty(t, p.sends)
	require.Equal(t, int64(0), countModel(t, store.db, &models.ConnectorOperation{}))
}

func TestOperationsDelayedConfirmationReplayCannotResendUnknownAttempt(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s := operationTestService(t, store, p)
	r := operationTestRequest(t, store)
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	confirmedRevision := d.Operation.Revision
	op, e := s.ConfirmResend(t.Context(), d.Operation.ID, confirmedRevision, r.Principal, true)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Equal(t, connector.OperationUnknown, op.State)
	require.Equal(t, uint64(2), op.Attempt)
	// A delayed copy of exactly the same explicit decision is stale even though
	// the newer attempt has already returned to unknown.
	_, e = s.ConfirmResend(t.Context(), op.ID, confirmedRevision, r.Principal, true)
	require.ErrorIs(t, e, ErrOperationChanged)
	require.Len(t, p.sends, 2)
	// A fresh confirmation of the newly observed revision may send again.
	_, e = s.ConfirmResend(t.Context(), op.ID, op.Revision, r.Principal, true)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Len(t, p.sends, 3)
}

func TestOperationsPreserveLargeIntegerOnInitialSendAndExplicitResend(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	r := operationTestRequest(t, store)
	connection := operationTestConnection()
	connection.Bindings[0].Actions[0].InputSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"],"additionalProperties":false}`)
	s, e := NewService(store, p, connection, "epoch")
	require.NoError(t, e)
	for _, number := range []any{int64(9007199254740993), json.Number("9007199254740993")} {
		r.Input = map[string]any{"n": number}
		r.IdempotencyKey = uuid.NewString()
		d, e := s.Submit(t.Context(), r)
		if !d.Created {
			require.True(t, d.Conflict)
			continue
		}
		require.ErrorIs(t, e, ErrOperationProvider)
		require.JSONEq(t, `{"n":9007199254740993}`, string(d.Operation.Input))
		require.Equal(t, json.Number("9007199254740993"), p.sends[len(p.sends)-1].Input["n"])
		_, e = s.ConfirmResend(t.Context(), d.Operation.ID, d.Operation.Revision, r.Principal, true)
		require.ErrorIs(t, e, ErrOperationProvider)
		require.Equal(t, json.Number("9007199254740993"), p.sends[len(p.sends)-1].Input["n"])
		// Finish this receipt to exercise both supported caller representations.
		_, e = store.catalog.SetOperationState(t.Context(), d.Operation.ID, connector.OperationCompleted)
		require.NoError(t, e)
	}
	require.Len(t, p.sends, 4)
}

func TestOperationsValidateLargeIntegerResultWithoutRounding(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	r := operationTestRequest(t, store)
	connection := operationTestConnection()
	connection.Bindings[0].Actions[0].ResultSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer","const":9007199254740993}},"required":["n"],"additionalProperties":false}`)
	s, e := NewService(store, p, connection, "epoch")
	require.NoError(t, e)
	p.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"n":9007199254740993}`)}, nil
	}
	d, e := s.Submit(t.Context(), r)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, d.Operation.State)
	require.Contains(t, string(d.Operation.ProviderEvidence), "9007199254740993")
}

func TestOperationsFreshResendAuditActorCannotContainSecret(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	r := operationTestRequest(t, store)
	s, e := NewService(store, p, operationTestConnection(), "epoch", "credential<prod>")
	require.NoError(t, e)
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	changed := *r.Principal
	changed.Subject = "credential<prod>"
	_, e = s.ConfirmResend(t.Context(), d.Operation.ID, d.Operation.Revision, &changed, true)
	require.ErrorIs(t, e, ErrSensitiveOperation)
	op, e := store.Operation(t.Context(), d.Operation.ID)
	require.NoError(t, e)
	require.Empty(t, op.ResendAudit)
	require.Len(t, p.sends, 1)
}

func TestOperationsLowLevelUnsentTransitionCannotReleasePriorUncertainty(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s := operationTestService(t, store, p)
	r := operationTestRequest(t, store)
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	// Even moving unknown back to submitted cannot erase evidence that there may
	// have been an earlier send. The untouched-admission revision is required.
	_, e = store.catalog.SetOperationState(t.Context(), d.Operation.ID, connector.OperationSubmitted)
	require.NoError(t, e)
	_, e = store.catalog.SetOperationState(t.Context(), d.Operation.ID, connector.OperationNotDispatched)
	require.ErrorIs(t, e, connector.ErrInvalidTransition)
	_, e = store.catalog.SetOperationState(t.Context(), d.Operation.ID, connector.OperationAccepted)
	require.NoError(t, e)
	_, e = store.catalog.SetOperationState(t.Context(), d.Operation.ID, connector.OperationNotDispatched)
	require.ErrorIs(t, e, connector.ErrInvalidTransition)
	op, e := store.Operation(t.Context(), d.Operation.ID)
	require.NoError(t, e)
	require.Equal(t, connector.OperationAccepted, op.State)
	require.NotNil(t, op.OpenKey)
}

func TestOperationsSameEpochReorderedConfigurationSurvivesFileReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reordered.db")
	db := openSQLiteFile(t, path)
	migrateStore(t, db)
	store := NewStore(db)
	p := &operationTestProvider{}
	r := operationTestRequest(t, store)
	original := operationTestConnection()
	binding := &original.Bindings[0]
	extra := binding.Actions[0]
	extra.Name = "z_action"
	binding.Actions = append([]connector.Action{extra}, binding.Actions...)
	binding.ActivityAllowlist = []connector.ActivityJob{{ActivityType: "b", Jobs: []string{"beta", "alpha"}}, {ActivityType: "a", Jobs: []string{"gamma"}}}
	epoch, e := connector.Fingerprint(&connector.Config{Version: 1, Connections: []connector.Connection{original}})
	require.NoError(t, e)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", epoch))
	s, e := NewService(store, p, original, epoch)
	require.NoError(t, e)
	require.Nil(t, s.connection.Bindings[0].StatusQueryResultSchema, "freezing configuration must preserve an omitted optional schema")
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Equal(t, connector.OperationUnknown, d.Operation.State)
	// Canonical configuration identity ignores these nonsemantic order changes.
	reordered := original
	reordered.Bindings = append([]connector.Binding(nil), original.Bindings...)
	binding = &reordered.Bindings[0]
	binding.Actions = append([]connector.Action(nil), binding.Actions...)
	binding.ActivityAllowlist = append([]connector.ActivityJob(nil), binding.ActivityAllowlist...)
	for i := range binding.ActivityAllowlist {
		binding.ActivityAllowlist[i].Jobs = append([]string(nil), binding.ActivityAllowlist[i].Jobs...)
	}
	binding.Actions[0], binding.Actions[1] = binding.Actions[1], binding.Actions[0]
	binding.ActivityAllowlist[0], binding.ActivityAllowlist[1] = binding.ActivityAllowlist[1], binding.ActivityAllowlist[0]
	binding.ActivityAllowlist[1].Jobs[0], binding.ActivityAllowlist[1].Jobs[1] = binding.ActivityAllowlist[1].Jobs[1], binding.ActivityAllowlist[1].Jobs[0]
	sameEpoch, e := connector.Fingerprint(&connector.Config{Version: 1, Connections: []connector.Connection{reordered}})
	require.NoError(t, e)
	require.Equal(t, epoch, sameEpoch)
	closeSQLite(t, db)
	db = openSQLiteFile(t, path)
	store = NewStore(db)
	s, e = NewService(store, p, reordered, sameEpoch)
	require.NoError(t, e)
	replay, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.False(t, replay.Created)
	require.Equal(t, d.Operation.ID, replay.Operation.ID)
	require.Len(t, p.sends, 1)
	op, e := s.Reconcile(t.Context(), d.Operation.ID)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Equal(t, connector.OperationUnknown, op.State)
	require.Len(t, p.lookups, 2)
	require.Len(t, p.sends, 1)
	p.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	op, e = s.ConfirmResend(t.Context(), op.ID, op.Revision, r.Principal, true)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, op.State)
	require.Len(t, p.sends, 2)
	require.Equal(t, p.sends[0].Input, p.sends[1].Input)
	require.Equal(t, p.sends[0].Actor, p.sends[1].Actor)
	require.Equal(t, p.sends[0].ExternalUpdateID, p.sends[1].ExternalUpdateID)
	require.Equal(t, d.Operation.BindingSnapshot, op.BindingSnapshot)
}

func TestOperationsFreezeConfigurationWithoutMaterializingOptionalSchema(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	r := operationTestRequest(t, store)
	connection := operationTestConnection()
	connection.Bindings[0].ActivityAllowlist = []connector.ActivityJob{{ActivityType: "start", Jobs: []string{"local"}}}
	s, e := NewService(store, p, connection, "epoch")
	require.NoError(t, e)
	require.Nil(t, s.connection.Bindings[0].StatusQueryResultSchema)
	connection.Bindings[0].Version = "changed"
	connection.Bindings[0].Actions[0].Name = "changed"
	connection.Bindings[0].Actions[0].InputSchema[0] = 'x'
	connection.Bindings[0].Actions[0].ResultSchema[0] = 'x'
	connection.Bindings[0].ActivityAllowlist[0].Jobs[0] = "changed"
	d, e := s.Submit(t.Context(), r)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.True(t, d.Created)
	require.Len(t, p.sends, 1)
	require.Equal(t, "1", p.sends[0].Actor.BindingVersion)
	require.Equal(t, "local", s.connection.Bindings[0].ActivityAllowlist[0].Jobs[0])
}

func TestOperationsPrincipalProjectionRequiresAuthenticatedUUIDAndKind(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	p := &operationTestProvider{}
	s := operationTestService(t, store, p)
	r := operationTestRequest(t, store)
	for _, mutate := range []func(*OperationPrincipal){func(p *OperationPrincipal) { p.Kind = "service" }, func(p *OperationPrincipal) { p.StableID = "" }, func(p *OperationPrincipal) { p.StableID = uuid.Nil.String() }, func(p *OperationPrincipal) { p.StableID = "invented-id" }, func(p *OperationPrincipal) { p.Subject = "" }, func(p *OperationPrincipal) { p.Role = "Operator" }} {
		principal := *r.Principal
		mutate(&principal)
		bad := r
		bad.Principal = &principal
		_, e := s.Submit(t.Context(), bad)
		require.ErrorIs(t, e, ErrOperationForbidden)
	}
	require.Empty(t, p.sends)
	require.Equal(t, int64(0), countModel(t, store.db, &models.ConnectorOperation{}))
}

func migrateStore(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&models.ExternalExecution{}, &models.ExternalExecutionSnapshot{}, &models.ExternalExecutionRelation{}, &models.ConnectorOperation{}, &models.ConnectorConfiguration{}))
}
func openStoreDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openSQLiteFile(t, filepath.Join(t.TempDir(), "operations.db"))
	migrateStore(t, db)
	return db
}
func openSQLiteFile(t *testing.T, path string) *gorm.DB {
	t.Helper()
	db, e := gorm.Open(sqlite.Open("file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate"), &gorm.Config{})
	require.NoError(t, e)
	sqlDB, e := db.DB()
	require.NoError(t, e)
	sqlDB.SetMaxOpenConns(8)
	sqlDB.SetMaxIdleConns(8)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
func closeSQLite(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error)
	sqlDB, e := db.DB()
	require.NoError(t, e)
	require.NoError(t, sqlDB.Close())
}
func mustExecutionRef(t *testing.T, connection, id string) connector.ExecutionReference {
	t.Helper()
	ref, e := connector.NewExecutionReference(connection, map[string]string{"execution": id})
	require.NoError(t, e)
	return ref
}
func requireDirect(t *testing.T, store *Store, ref connector.ExecutionReference) {
	t.Helper()
	_, e := store.catalog.RecordSnapshot(t.Context(), connector.Observation{Execution: ref, Generation: 1, SourceEventID: "direct", NativeStatus: connector.DisplayRunning, DisplayStatus: connector.DisplayRunning, Availability: connector.AvailabilityAvailable, Completeness: connector.CompletenessComplete, SourceKind: connector.SourceDirect, ObservedAt: time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)})
	require.NoError(t, e)
}
func countModel(t *testing.T, db *gorm.DB, model any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(model).Count(&n).Error)
	return n
}

func TestOperationsCompatibleConfigurationEpochAdvanceRecoversPinnedReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cross-epoch.db")
	db := openSQLiteFile(t, path)
	migrateStore(t, db)
	store := NewStore(db)
	provider := &operationTestProvider{}
	request := operationTestRequest(t, store)
	connection := operationTestConnection()
	connection.Endpoint = "a.example:7233"
	connection.Scope = "scope-a"
	firstConfig := &connector.Config{Version: 1, Connections: []connector.Connection{connection}}
	firstEpoch, e := connector.Fingerprint(firstConfig)
	require.NoError(t, e)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", firstEpoch))
	firstService, e := NewService(store, provider, connection, firstEpoch)
	require.NoError(t, e)
	decision, e := firstService.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationProvider)
	original := decision.Operation
	require.Equal(t, connector.OperationUnknown, original.State)
	identityJSON, e := json.Marshal(connection.Identity())
	require.NoError(t, e)
	require.JSONEq(t, string(identityJSON), string(original.ConnectionIdentity))
	unrelated := operationTestConnection()
	unrelated.ID = "unrelated"
	unrelated.Endpoint = "b.example:7233"
	unrelated.Scope = "scope-b"
	secondEpoch, e := connector.Fingerprint(&connector.Config{Version: 1, Connections: []connector.Connection{connection, unrelated}})
	require.NoError(t, e)
	require.NotEqual(t, firstEpoch, secondEpoch)
	closeSQLite(t, db)
	db = openSQLiteFile(t, path)
	store = NewStore(db)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), firstEpoch, secondEpoch))
	current, e := NewService(store, provider, connection, secondEpoch)
	require.NoError(t, e)
	replay, e := current.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.False(t, replay.Created)
	require.Equal(t, original.ID, replay.Operation.ID)
	require.Equal(t, original.Actor, replay.Operation.Actor)
	require.Equal(t, original.ExternalUpdateID, replay.Operation.ExternalUpdateID)
	require.Equal(t, firstEpoch, replay.Operation.ConfigurationEpoch)
	require.Len(t, provider.sends, 1)
	changedTarget := request
	changedTarget.Target.Execution = mustExecutionRef(t, connection.ID, "different-run")
	requireDirect(t, store, changedTarget.Target.Execution)
	_, e = current.Submit(t.Context(), changedTarget)
	require.ErrorIs(t, e, connector.ErrIdempotencyMismatch)
	changedBinding := request
	changedBinding.Target.BindingVersion = "changed"
	_, e = current.Submit(t.Context(), changedBinding)
	require.ErrorIs(t, e, connector.ErrInvalidOperation)
	recovered, e := current.Reconcile(t.Context(), original.ID)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Equal(t, connector.OperationUnknown, recovered.State)
	require.NotNil(t, recovered.OpenKey)
	require.Len(t, provider.lookups, 2)
	require.Len(t, provider.sends, 1)
	// A stale process cannot use this compatibility rule to bypass active fencing.
	stale, e := NewService(store, provider, connection, firstEpoch)
	require.NoError(t, e)
	_, e = stale.Submit(t.Context(), request)
	require.ErrorIs(t, e, connector.ErrEpochMismatch)
	_, e = stale.Reconcile(t.Context(), original.ID)
	require.ErrorIs(t, e, connector.ErrEpochMismatch)
	_, e = stale.ConfirmResend(t.Context(), original.ID, recovered.Revision, request.Principal, true)
	require.ErrorIs(t, e, connector.ErrEpochMismatch)
	// The transaction itself fences the confirming service epoch, even when it
	// loses a race after the service-level check and before the CAS/audit write.
	var actor connector.ActorEnvelope
	require.NoError(t, json.Unmarshal(original.Actor, &actor))
	_, e = store.confirmOperationResend(t.Context(), recovered, actor, firstEpoch, connector.MaxPageMetadataBytes)
	require.ErrorIs(t, e, connector.ErrEpochMismatch)
	untouched, e := store.Operation(t.Context(), original.ID)
	require.NoError(t, e)
	require.Equal(t, recovered.Revision, untouched.Revision)
	require.Equal(t, recovered.Attempt, untouched.Attempt)
	require.Empty(t, untouched.ResendAudit)
	require.Len(t, provider.lookups, 2)
	require.Len(t, provider.sends, 1)
	// The current original principal confirms the observed revision; only that
	// same immutable run/input/actor/ID is sent, and audit records the new epoch.
	provider.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	completed, e := current.ConfirmResend(t.Context(), original.ID, recovered.Revision, request.Principal, true)
	require.NoError(t, e)
	require.Equal(t, connector.OperationCompleted, completed.State)
	require.Nil(t, completed.OpenKey)
	require.Equal(t, uint64(2), completed.Attempt)
	require.Equal(t, firstEpoch, completed.ConfigurationEpoch)
	require.Equal(t, original.ConnectionIdentity, completed.ConnectionIdentity)
	require.Equal(t, original.BindingSnapshot, completed.BindingSnapshot)
	require.Equal(t, original.Input, completed.Input)
	require.Equal(t, original.Actor, completed.Actor)
	require.Equal(t, original.ExternalUpdateID, completed.ExternalUpdateID)
	require.Len(t, provider.sends, 2)
	require.Equal(t, provider.sends[0], provider.sends[1])
	var audit []resendDecision
	require.NoError(t, json.Unmarshal(completed.ResendAudit, &audit))
	require.Len(t, audit, 1)
	require.Equal(t, secondEpoch, audit[0].ConfigurationEpoch)
	require.Equal(t, request.Principal.StableID, audit[0].StableID)
}

func TestOperationsEpochCompatibilityCannotRetargetConnectionOrBinding(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*connector.Connection)
	}{
		{"provider", func(c *connector.Connection) { c.Provider = "changed" }},
		{"endpoint", func(c *connector.Connection) { c.Endpoint = "changed.example:7233" }},
		{"scope", func(c *connector.Connection) { c.Scope = "changed" }},
		{"connection id", func(c *connector.Connection) { c.ID = "changed" }},
		{"removed binding", func(c *connector.Connection) { c.Bindings = nil }},
		{"binding version", func(c *connector.Connection) { c.Bindings[0].Version = "2" }},
		{"binding type", func(c *connector.Connection) { c.Bindings[0].Name = "changed" }},
		{"removed action", func(c *connector.Connection) { c.Bindings[0].Actions = nil }},
		{"input schema", func(c *connector.Connection) {
			c.Bindings[0].Actions[0].InputSchema = json.RawMessage(`{"type":"object","properties":{"note":{"type":"string"},"extra":{"type":"string"}},"required":["note"],"additionalProperties":false}`)
		}},
		{"result schema", func(c *connector.Connection) {
			c.Bindings[0].Actions[0].ResultSchema = json.RawMessage(`{"type":"object","properties":{"approved":{"type":"boolean"},"extra":{"type":"string"}},"required":["approved"],"additionalProperties":false}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(openStoreDB(t))
			provider := &operationTestProvider{}
			request := operationTestRequest(t, store)
			originalConnection := operationTestConnection()
			originalConnection.Endpoint = "a.example:7233"
			originalConnection.Scope = "scope-a"
			firstEpoch, e := connector.Fingerprint(&connector.Config{Version: 1, Connections: []connector.Connection{originalConnection}})
			require.NoError(t, e)
			require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", firstEpoch))
			originalService, e := NewService(store, provider, originalConnection, firstEpoch)
			require.NoError(t, e)
			decision, e := originalService.Submit(t.Context(), request)
			require.ErrorIs(t, e, ErrOperationProvider)
			unrelated := operationTestConnection()
			unrelated.ID = "unrelated"
			secondEpoch, e := connector.Fingerprint(&connector.Config{Version: 1, Connections: []connector.Connection{originalConnection, unrelated}})
			require.NoError(t, e)
			require.NoError(t, store.catalog.ActivateEpoch(t.Context(), firstEpoch, secondEpoch))
			// Even an incorrectly composed adapter that claims the active epoch cannot
			// bypass the receipt's immutable identity/canonical binding snapshots.
			incompatible := originalConnection
			incompatible.Bindings = append([]connector.Binding(nil), originalConnection.Bindings...)
			incompatible.Bindings[0].Actions = append([]connector.Action(nil), originalConnection.Bindings[0].Actions...)
			tc.mutate(&incompatible)
			service, e := NewService(store, provider, incompatible, secondEpoch)
			require.NoError(t, e)
			_, e = service.Reconcile(t.Context(), decision.Operation.ID)
			require.ErrorIs(t, e, connector.ErrInvalidOperation)
			_, e = service.ConfirmResend(t.Context(), decision.Operation.ID, decision.Operation.Revision, request.Principal, true)
			require.ErrorIs(t, e, connector.ErrInvalidOperation)
			_, e = service.Submit(t.Context(), request)
			require.Error(t, e)
			unchanged, e := store.Operation(t.Context(), decision.Operation.ID)
			require.NoError(t, e)
			require.Equal(t, decision.Operation.State, unchanged.State)
			require.Equal(t, decision.Operation.Revision, unchanged.Revision)
			require.Equal(t, decision.Operation.Attempt, unchanged.Attempt)
			require.Empty(t, unchanged.ResendAudit)
			require.NotNil(t, unchanged.OpenKey)
			require.Len(t, provider.sends, 1)
			require.Empty(t, provider.lookups)
		})
	}
}

func TestOperationsConnectionIdentitySnapshotIsBoundedAndSecretScreened(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	request := operationTestRequest(t, store)
	for _, endpoint := range []string{"credential<prod>:7233", "-----BEGIN PRIVATE KEY-----\nbody\n-----END PRIVATE KEY-----"} {
		connection := operationTestConnection()
		connection.Endpoint = endpoint
		service, e := NewService(store, provider, connection, "epoch", "credential<prod>")
		require.NoError(t, e)
		_, e = service.Submit(t.Context(), request)
		require.ErrorIs(t, e, ErrSensitiveOperation)
	}
	connection := operationTestConnection()
	service, e := NewService(store, provider, connection, "epoch")
	require.NoError(t, e)
	decision, e := service.Admit(t.Context(), request)
	require.NoError(t, e)
	op := decision.Operation
	snapshotBytes := len(op.Input) + len(op.Actor) + len(op.Target) + len(op.BindingSnapshot) + len(op.ConnectionIdentity)
	connection.Limits.MaxPageMetadataBytes = snapshotBytes - 1
	bounded, e := NewService(store, provider, connection, "epoch")
	require.NoError(t, e)
	_, e = bounded.Submit(t.Context(), request)
	require.ErrorIs(t, e, connector.ErrPayloadTooLarge)
	disabled := connection
	disabled.Enabled = false
	_, e = NewService(store, provider, disabled, "epoch")
	require.ErrorIs(t, e, connector.ErrInvalidOperation)
	require.Empty(t, provider.sends)
}

func TestOperationsTransientNoSendProofIsClaimedOnceBeforeRetry(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	provider.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationNotDispatched, PreflightRPCCode: "Unavailable"}, errors.New("preflight transport failed")
	}
	first, e := service.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Equal(t, connector.OperationSubmitted, first.Operation.State)
	require.NotNil(t, first.Operation.OpenKey)
	require.True(t, first.Operation.RetryableUnsent)
	require.Equal(t, uint64(1), first.Operation.Attempt)
	// Background reconciliation never consumes or sends a proven-unsent retry.
	_, e = service.Reconcile(t.Context(), first.Operation.ID)
	require.ErrorIs(t, e, ErrOperationPending)
	require.Empty(t, provider.lookups)
	require.Len(t, provider.sends, 1)
	entered, release := make(chan struct{}), make(chan struct{})
	provider.submit = func(OperationDispatch) (OperationEvidence, error) {
		close(entered)
		<-release
		return OperationEvidence{State: connector.OperationUnknown, RPCCode: "DeadlineExceeded"}, errors.New("reply lost")
	}
	result := make(chan error, 1)
	go func() { _, e := service.Submit(context.Background(), request); result <- e }()
	<-entered
	claimed, e := store.Operation(t.Context(), first.Operation.ID)
	require.NoError(t, e)
	require.False(t, claimed.RetryableUnsent)
	require.Greater(t, claimed.Revision, first.Operation.Revision)
	// A competing duplicate may only look up this claimed send, never dispatch.
	_, e = service.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Len(t, provider.sends, 2)
	require.Len(t, provider.lookups, 1)
	close(release)
	e = <-result
	require.True(t, errors.Is(e, ErrOperationProvider) || errors.Is(e, ErrOperationChanged))
	_, e = service.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Len(t, provider.sends, 2)
	require.Len(t, provider.lookups, 2)
	require.Equal(t, provider.sends[0], provider.sends[1])
}

func TestOperationsAdmissionOnlyReplayIsPendingAndClaimedCrashIsLookupOnly(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	admitted, e := service.Admit(t.Context(), request)
	require.NoError(t, e)
	require.False(t, admitted.Operation.RetryableUnsent)
	_, e = service.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationPending)
	require.Empty(t, provider.sends)
	require.Empty(t, provider.lookups)
	claimed, e := store.claimDispatch(t.Context(), admitted.Operation, "epoch", true)
	require.NoError(t, e)
	require.Greater(t, claimed.Revision, admitted.Operation.Revision)
	require.False(t, claimed.RetryableUnsent)
	// A restart cannot distinguish crash-before-RPC from crash-after-RPC here.
	restarted := operationTestService(t, store, provider)
	_, e = restarted.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Empty(t, provider.sends)
	require.Len(t, provider.lookups, 1)
}

func TestOperationsEvidenceWriteFailureAfterSendReplaysLookupOnly(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	injected := errors.New("evidence write unavailable")
	failEvidence := true
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("operations:test_evidence_failure", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if ok && failEvidence && values["provider_evidence"] != nil {
			_ = tx.AddError(injected)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove("operations:test_evidence_failure") })
	provider.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	first, e := service.Submit(t.Context(), request)
	require.ErrorIs(t, e, injected)
	require.Len(t, provider.sends, 1)
	failEvidence = false
	var row models.ConnectorOperation
	require.NoError(t, db.Where("idempotency_key = ?", request.IdempotencyKey).First(&row).Error)
	require.Equal(t, connector.OperationSubmitted, row.State)
	require.Empty(t, row.ProviderEvidence)
	require.False(t, row.RetryableUnsent)
	require.Greater(t, row.Revision, uint64(1))
	provider.lookup = func(d OperationDispatch) (OperationEvidence, error) {
		require.Nil(t, d.Input)
		require.Empty(t, d.Actor.OperationID)
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	repaired, e := service.Submit(t.Context(), request)
	require.NoError(t, e)
	require.False(t, repaired.Created)
	require.Equal(t, row.ID, repaired.Operation.ID)
	require.Equal(t, connector.OperationCompleted, repaired.Operation.State)
	require.Nil(t, repaired.Operation.OpenKey)
	require.Len(t, provider.sends, 1)
	require.Len(t, provider.lookups, 1)
	require.NotEqual(t, uuid.Nil, first.Operation.ID, "failed persistence should retain a reviewable receipt id")
}

func TestOperationsAcceptedProofSurvivesEveryWeakerLookup(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	provider.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationAccepted, NativeStage: "accepted"}, nil
	}
	first, e := service.Submit(t.Context(), request)
	require.NoError(t, e)
	accepted := first.Operation
	cases := []struct {
		evidence OperationEvidence
		err      error
	}{
		{OperationEvidence{State: connector.OperationUnknown, RPCCode: "NotFound"}, errors.New("missing")},
		{OperationEvidence{State: connector.OperationUnknown, RPCCode: "DeadlineExceeded"}, errors.New("timeout")},
		{OperationEvidence{State: connector.OperationUnknown, RPCCode: "Unavailable"}, errors.New("unavailable")},
		{OperationEvidence{State: connector.OperationSubmitted}, nil},
		{OperationEvidence{State: connector.OperationUnknown, FailureKind: "failure_origin_unknown"}, nil},
		{OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"invalid":true}`)}, nil},
		{OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true,"padding":"` + strings.Repeat("x", connector.MaxPageMetadataBytes) + `"}`)}, nil},
	}
	for _, tc := range cases {
		provider.lookup = func(OperationDispatch) (OperationEvidence, error) { return tc.evidence, tc.err }
		current, e := service.Reconcile(t.Context(), accepted.ID)
		if tc.err != nil {
			require.ErrorIs(t, e, ErrOperationProvider)
		} else {
			require.NoError(t, e)
		}
		require.Equal(t, connector.OperationAccepted, current.State)
		require.Equal(t, accepted.ProviderEvidence, current.ProviderEvidence)
		require.Equal(t, accepted.Revision, current.Revision)
		require.NotNil(t, current.OpenKey)
		_, e = service.ConfirmResend(t.Context(), current.ID, current.Revision, request.Principal, true)
		require.ErrorIs(t, e, ErrOperationConfirmation)
	}
	require.Len(t, provider.sends, 1)
}

func TestOperationsResendPreflightCannotProveEarlierSendAbsent(t *testing.T) {
	for _, code := range []string{"Unavailable", "NotFound", ""} {
		t.Run(code, func(t *testing.T) {
			store := NewStore(openStoreDB(t))
			require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
			provider := &operationTestProvider{}
			service := operationTestService(t, store, provider)
			request := operationTestRequest(t, store)
			first, e := service.Submit(t.Context(), request)
			require.ErrorIs(t, e, ErrOperationProvider)
			provider.submit = func(OperationDispatch) (OperationEvidence, error) {
				return OperationEvidence{State: connector.OperationNotDispatched, PreflightRPCCode: code}, errors.New("no send on this attempt")
			}
			second, e := service.ConfirmResend(t.Context(), first.Operation.ID, first.Operation.Revision, request.Principal, true)
			require.ErrorIs(t, e, ErrOperationProvider)
			require.Equal(t, connector.OperationUnknown, second.State)
			require.NotNil(t, second.OpenKey)
			require.False(t, second.RetryableUnsent)
			_, e = service.Submit(t.Context(), request)
			require.ErrorIs(t, e, ErrOperationProvider)
			require.Len(t, provider.sends, 2)
			require.Len(t, provider.lookups, 1)
		})
	}
}

func TestOperationsLostNoSendEvidenceCannotGrantRetry(t *testing.T) {
	db := openStoreDB(t)
	store := NewStore(db)
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	injected := errors.New("no-send proof write lost")
	fail := true
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("operations:test_unsent_failure", func(tx *gorm.DB) {
		values, ok := tx.Statement.Dest.(map[string]any)
		if ok && fail && values["provider_evidence"] != nil {
			_ = tx.AddError(injected)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove("operations:test_unsent_failure") })
	provider.submit = func(OperationDispatch) (OperationEvidence, error) {
		return OperationEvidence{State: connector.OperationNotDispatched, PreflightRPCCode: "Unavailable"}, errors.New("preflight unavailable")
	}
	first, e := service.Submit(t.Context(), request)
	require.ErrorIs(t, e, injected)
	require.NotEqual(t, uuid.Nil, first.Operation.ID)
	require.False(t, first.Operation.RetryableUnsent)
	fail = false
	_, e = service.Submit(t.Context(), request)
	require.ErrorIs(t, e, ErrOperationProvider)
	require.Len(t, provider.sends, 1)
	require.Len(t, provider.lookups, 1)
}

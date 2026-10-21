package operations

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestOperationsConcurrentLookupReturnsCurrentReceipt(t *testing.T) {
	unknown := OperationEvidence{State: connector.OperationUnknown, RPCCode: "NotFound"}
	completed := OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}
	cases := []struct {
		name    string
		current OperationEvidence
		delayed OperationEvidence
		want    string
	}{
		{"equal unknown", unknown, unknown, connector.OperationUnknown},
		{"accepted beats weak lookup", OperationEvidence{State: connector.OperationAccepted, NativeStage: "accepted"}, unknown, connector.OperationAccepted},
		{"completed beats weak lookup", completed, unknown, connector.OperationCompleted},
		{"rejected beats weak lookup", OperationEvidence{State: connector.OperationRejected, RejectionProof: "validator_rejected"}, unknown, connector.OperationRejected},
		{"stronger delayed completion advances", unknown, completed, connector.OperationCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewStore(openStoreDB(t))
			require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
			provider := &operationTestProvider{}
			service := operationTestService(t, store, provider)
			request := operationTestRequest(t, store)
			first, err := service.Submit(t.Context(), request)
			require.ErrorIs(t, err, ErrOperationProvider)
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var calls atomic.Int32
			provider.lookup = func(OperationDispatch) (OperationEvidence, error) {
				evidence := tc.current
				if calls.Add(1) == 1 {
					close(entered)
					<-release
					evidence = tc.delayed
				}
				if evidence.State == connector.OperationUnknown {
					return evidence, errors.New("provider lookup not found")
				}
				return evidence, nil
			}
			type result struct {
				op  models.ConnectorOperation
				err error
			}
			delayed := make(chan result, 1)
			go func() {
				op, err := service.Reconcile(t.Context(), first.Operation.ID)
				delayed <- result{op, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first lookup did not reach the provider")
			}
			current, err := service.Reconcile(t.Context(), first.Operation.ID)
			if tc.current.State == connector.OperationUnknown {
				require.ErrorIs(t, err, ErrOperationProvider)
			} else {
				require.NoError(t, err)
			}
			unblock()
			var observed result
			select {
			case observed = <-delayed:
			case <-time.After(5 * time.Second):
				t.Fatal("delayed lookup did not complete")
			}
			if tc.delayed.State == connector.OperationUnknown {
				if tc.want == connector.OperationUnknown {
					require.ErrorIs(t, observed.err, ErrOperationProvider, "a concurrent lookup must not become a revision conflict")
				} else {
					require.NoError(t, observed.err, "durable positive evidence must survive a stale weak lookup")
				}
				require.Equal(t, current.Revision, observed.op.Revision)
				require.Equal(t, current.ProviderEvidence, observed.op.ProviderEvidence)
			} else {
				require.NoError(t, observed.err)
				require.Greater(t, observed.op.Revision, current.Revision)
			}
			require.Equal(t, tc.want, observed.op.State)
			require.Equal(t, first.Operation.ID, observed.op.ID)
			require.Equal(t, first.Operation.ExternalUpdateID, observed.op.ExternalUpdateID)
			require.Equal(t, first.Operation.Attempt, observed.op.Attempt)
			require.Equal(t, first.Operation.Target, observed.op.Target)
			require.Equal(t, first.Operation.Actor, observed.op.Actor)
			persisted, err := store.Operation(t.Context(), observed.op.ID)
			require.NoError(t, err)
			require.Equal(t, observed.op, persisted)
			require.Len(t, provider.sends, 1)
			require.Len(t, provider.lookups, 2)
			for _, lookup := range provider.lookups {
				require.Equal(t, first.Operation.ExternalUpdateID, lookup.ExternalUpdateID)
				require.Nil(t, lookup.Input)
				require.Empty(t, lookup.Actor.OperationID)
			}
		})
	}
}

func TestOperationsDelayedLookupCannotCrossConfirmedAttempt(t *testing.T) {
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	first, err := service.Submit(t.Context(), request)
	require.ErrorIs(t, err, ErrOperationProvider)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	provider.lookup = func(OperationDispatch) (OperationEvidence, error) {
		close(entered)
		<-release
		return OperationEvidence{State: connector.OperationCompleted, Result: json.RawMessage(`{"approved":true}`)}, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := service.Reconcile(t.Context(), first.Operation.ID)
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("lookup did not reach the provider")
	}
	confirmed, err := service.ConfirmResend(t.Context(), first.Operation.ID, first.Operation.Revision, request.Principal, true)
	require.ErrorIs(t, err, ErrOperationProvider)
	require.Equal(t, uint64(2), confirmed.Attempt)
	unblock()
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrOperationChanged)
	case <-time.After(5 * time.Second):
		t.Fatal("delayed lookup did not complete")
	}
	persisted, err := store.Operation(t.Context(), confirmed.ID)
	require.NoError(t, err)
	require.Equal(t, confirmed, persisted)
	require.Equal(t, connector.OperationUnknown, persisted.State)
	require.Len(t, provider.sends, 2)
	require.Len(t, provider.lookups, 1)
}

package operations

import (
	"github.com/caesium-cloud/caesium/internal/connector"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"testing"
)

// Literal target bytes and the fixed principal fingerprint pin the W4 durable
// representation. C1 HTTP reference field names must not alter existing receipts
// or turn an identical retry into an idempotency conflict after restart.
func TestW4ReceiptCodecAndSameKeyFingerprintSurviveReadComposition(t *testing.T) {
	const oldTarget = `{"execution":{"ConnectionID":"primary","Coordinates":{"execution":"pinned-run"}},"binding_name":"publication","binding_version":"1"}`
	const oldFingerprint = "e7df6deb8ae603de6e96a040a3cd32f819662fae7cf517f946fb109dc27c8716"
	store := NewStore(openStoreDB(t))
	require.NoError(t, store.catalog.ActivateEpoch(t.Context(), "", "epoch"))
	provider := &operationTestProvider{}
	service := operationTestService(t, store, provider)
	request := operationTestRequest(t, store)
	request.Principal.StableID = "11111111-2222-4333-8444-555555555555"
	original, err := service.Submit(t.Context(), request)
	require.ErrorIs(t, err, ErrOperationProvider)
	require.Equal(t, oldTarget, string(original.Operation.Target))
	require.Equal(t, oldFingerprint, original.Operation.RequestFingerprint)
	require.NoError(t, store.db.Model(&models.ConnectorOperation{}).Where("id = ?", original.Operation.ID).Updates(map[string]any{"target": oldTarget, "request_fingerprint": oldFingerprint}).Error)
	restarted := operationTestService(t, store, provider)
	replay, err := restarted.Submit(t.Context(), request)
	require.ErrorIs(t, err, ErrOperationProvider)
	require.False(t, replay.Created)
	require.Equal(t, original.Operation.ID, replay.Operation.ID)
	require.Equal(t, connector.OperationUnknown, replay.Operation.State)
	require.Len(t, provider.sends, 1)
	require.Len(t, provider.lookups, 1)
	require.Equal(t, request.Target, provider.lookups[0].Target)
}

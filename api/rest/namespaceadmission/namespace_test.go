package namespaceadmission

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestWriteConflict: a quota refusal is 409 namespace_quota with Retry-After
// naming the namespace and its cap; an undeclared namespace is 409
// namespace_undeclared without Retry-After; anything else is left to the
// caller.
func TestWriteConflict(t *testing.T) {
	write := func(err error) (bool, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil), rec)
		handled, writeErr := WriteConflict(c, err)
		require.NoError(t, writeErr)
		return handled, rec
	}

	handled, rec := write(&runstorage.NamespaceQuotaError{Namespace: "marketing", Limit: 2})
	require.True(t, handled)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Equal(t, "5", rec.Header().Get("Retry-After"))
	var quota QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &quota))
	require.Equal(t, QuotaResponse{
		Error:             "namespace_quota",
		Message:           quota.Message,
		Namespace:         "marketing",
		MaxConcurrentRuns: 2,
	}, quota)
	require.Contains(t, quota.Message, "quotas.maxConcurrentRuns")

	handled, rec = write(&runstorage.NamespaceUndeclaredError{Namespace: "gone"})
	require.True(t, handled)
	require.Equal(t, http.StatusConflict, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"), "an undeclared namespace will not free up by waiting")
	var body UndeclaredResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "namespace_undeclared", body.Error)
	require.Equal(t, "gone", body.Namespace)

	for _, err := range []error{nil, errors.New("other"), runstorage.ErrMaxConcurrentRunsReached} {
		handled, rec := write(err)
		require.False(t, handled, "%v", err)
		require.Empty(t, rec.Body.Bytes())
	}
}

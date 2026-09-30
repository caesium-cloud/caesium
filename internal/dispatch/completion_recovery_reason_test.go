package dispatch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPostCompleteDistinguishesRecoveryFromContention(t *testing.T) {
	for _, code := range []string{ReasonOwnerNotReady, "owner_busy", ""} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPost, r.Method)
				w.WriteHeader(http.StatusServiceUnavailable)
				require.NoError(t, json.NewEncoder(w).Encode(ErrorResponse{Code: code}))
			}))
			defer server.Close()
			_, err := PostComplete(t.Context(), server.URL, "token", CompleteRequest{})
			if code == ReasonOwnerNotReady {
				require.ErrorIs(t, err, ErrOwnerNotReady)
				require.NotErrorIs(t, err, ErrOwnerBusy)
			} else {
				require.ErrorIs(t, err, ErrOwnerBusy)
				require.NotErrorIs(t, err, ErrOwnerNotReady)
			}
		})
	}
}

func TestPostCompleteOwnershipFenceIsTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		require.NoError(t, json.NewEncoder(w).Encode(ErrorResponse{Code: "stale_generation"}))
	}))
	defer server.Close()
	_, err := PostComplete(t.Context(), server.URL, "token", CompleteRequest{})
	require.ErrorIs(t, err, ErrOwnerRejected)
	require.NotErrorIs(t, err, ErrOwnerNotReady)
	require.NotErrorIs(t, err, ErrOwnerBusy)
}

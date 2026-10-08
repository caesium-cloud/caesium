package imagecheck

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/require"
)

func TestIsNotFoundUsesTypedClassification(t *testing.T) {
	require.False(t, isNotFound(nil))
	require.True(t, isNotFound(errdefs.ErrNotFound))
	require.True(t, isNotFound(fmt.Errorf("inspect failed: %w", errdefs.ErrNotFound)))
	require.False(t, isNotFound(errors.New("No such image: unrelated transport failure")))
}
func TestCheckPreservesErrorsThatOnlyMentionMissingImages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/images/missing/"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"image absent"}`))
		case strings.Contains(r.URL.Path, "/images/broken/"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"No such image: unrelated daemon failure"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+server.Listener.Addr().String())
	t.Setenv("DOCKER_API_VERSION", "1.47")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	results := Check(t.Context(), []string{"available", "missing", "broken"})
	require.Len(t, results, 3)
	require.True(t, results[0].Available)
	require.NoError(t, results[0].Error)
	require.False(t, results[1].Available)
	require.NoError(t, results[1].Error)
	require.False(t, results[2].Available)
	require.ErrorContains(t, results[2].Error, "No such image")
}

func TestCheckInspectErrorsDistinguishesWrappedAbsenceFromWording(t *testing.T) {
	wrapped := fmt.Errorf("inspect failed: %w", errdefs.ErrNotFound)
	ordinary := errors.New("No such image: unrelated transport failure")
	results := checkImages([]string{"missing", "broken"}, func(image string) error {
		if image == "missing" {
			return wrapped
		}
		return ordinary
	})
	require.Equal(t, []Result{{Image: "missing", Available: false}, {Image: "broken", Error: ordinary}}, results)
}

package dataset

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestReleaseConflictPreservesBothCauses(t *testing.T) {
	oldClient, oldKey, oldServer := httpClient, apiKeyFlag, serverFlag
	t.Cleanup(func() { httpClient, apiKeyFlag, serverFlag = oldClient, oldKey, oldServer })
	apiKeyFlag = ""
	serverFlag = "http://example.invalid"
	t.Setenv(apiKeyEnvVar, "")
	sentinel := errors.New("hold lookup interrupted")
	httpClient = &http.Client{Transport: policyTransport(func(*http.Request) (*http.Response, error) { return nil, sentinel })}
	conflict := &httpStatusError{StatusCode: http.StatusConflict, Body: "stale hold"}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err := releaseConflict(cmd, "namespace", "name", "hold", conflict)
	require.ErrorIs(t, err, conflict)
	require.ErrorIs(t, err, sentinel)
	var statusErr *httpStatusError
	require.ErrorAs(t, err, &statusErr)
	require.Same(t, conflict, statusErr)
	require.EqualError(t, err, `dataset request failed (409): stale hold; current hold state for dataset namespace/name could not be verified: Get "http://example.invalid/v1/datasets/holds?name=name&namespace=namespace&status=active": hold lookup interrupted`)
}

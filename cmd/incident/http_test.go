package incident

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

type policyTransport func(*http.Request) (*http.Response, error)

func (f policyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type policyBody struct {
	io.Reader
	closed bool
}

func (b *policyBody) Close() error { b.closed = true; return nil }

type policyFailure struct{ err error }

func (r policyFailure) Read([]byte) (int, error) { return 0, r.err }

func TestRequestPolicy(t *testing.T) {
	oldClient, oldKey := httpClient, apiKeyFlag
	t.Cleanup(func() { httpClient, apiKeyFlag = oldClient, oldKey })
	apiKeyFlag = " key "
	sentinel := errors.New("read interrupted")
	for _, tc := range []struct {
		name    string
		status  int
		readErr error
		want    string
	}{
		{"exact success", 200, nil, ""},
		{"different success status", 201, nil, "policy failed (201): partial"},
		{"redirect status", 302, nil, "policy failed (302): partial"},
		{"error status", 500, nil, "policy failed (500): partial"},
		{"read before status", 500, sentinel, "reading policy response: read interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := io.Reader(strings.NewReader("partial"))
			if tc.readErr != nil {
				reader = io.MultiReader(reader, policyFailure{tc.readErr})
			}
			body := &policyBody{Reader: reader}
			httpClient = &http.Client{Transport: policyTransport(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "Bearer key", req.Header.Get("Authorization"))
				require.Equal(t, "application/json", req.Header.Get("Content-Type"))
				require.Equal(t, http.MethodPost, req.Method)
				return &http.Response{StatusCode: tc.status, Body: body, Header: make(http.Header)}, nil
			})}
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			data, err := doRequest(cmd, http.MethodPost, "http://example.invalid", strings.NewReader("{}"), 200, "policy")
			if tc.want == "" {
				require.NoError(t, err)
				require.Equal(t, "partial", string(data))
			} else {
				require.EqualError(t, err, tc.want)
				require.Nil(t, data)
			}
			if tc.readErr != nil {
				require.ErrorIs(t, err, sentinel)
			}
			require.True(t, body.closed)
			require.Empty(t, stdout.String())
			require.Equal(t, "warning: --api-key is visible in process listings; prefer CAESIUM_INCIDENT_API_KEY\n", stderr.String())
		})
	}
	apiKeyFlag = ""
	t.Setenv("CAESIUM_INCIDENT_API_KEY", "")
	t.Setenv("CAESIUM_API_KEY", "")
	httpClient = &http.Client{Transport: policyTransport(func(req *http.Request) (*http.Response, error) {
		require.Empty(t, req.Header.Get("Content-Type"))
		require.Empty(t, req.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	_, err := doRequest(cmd, http.MethodGet, "http://example.invalid", nil, 200, "policy")
	require.NoError(t, err)
}

func TestListResponseStreams(t *testing.T) {
	oldServer, oldKey := serverFlag, apiKeyFlag
	t.Cleanup(func() { serverFlag, apiKeyFlag = oldServer, oldKey })
	apiKeyFlag = ""
	t.Setenv("CAESIUM_INCIDENT_API_KEY", "")
	t.Setenv("CAESIUM_API_KEY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/incidents", r.URL.Path)
		require.Empty(t, r.Header.Get("Content-Type"))
		_, _ = w.Write([]byte(`{"records":[]}`))
	}))
	defer server.Close()
	serverFlag = server.URL
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	require.NoError(t, listCmd.RunE(cmd, nil))
	require.Equal(t, "{\n  \"records\": []\n}\n", stdout.String())
	require.Empty(t, stderr.String())
}

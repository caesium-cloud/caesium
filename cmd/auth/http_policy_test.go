package auth

import (
	"bytes"
	"context"
	"io"
	"net/http"
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

func TestAuthCompleteResponsePolicy(t *testing.T) {
	oldClient, oldServer, oldKey := http.DefaultClient, listServer, listAPIKey
	t.Cleanup(func() { http.DefaultClient, listServer, listAPIKey = oldClient, oldServer, oldKey })
	listServer, listAPIKey = "http://example.invalid", " key "
	for _, status := range []int{200, 201, 302, 400, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &policyBody{Reader: strings.NewReader(`[]`)}
			http.DefaultClient = &http.Client{Transport: policyTransport(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "Bearer key", req.Header.Get("Authorization"))
				require.Empty(t, req.Header.Get("Content-Type"))
				return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
			})}
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			err := keyListCmd.RunE(cmd, nil)
			if status < 400 {
				require.NoError(t, err)
				require.Equal(t, "[]\n", stdout.String())
			} else {
				require.ErrorContains(t, err, "key list failed")
				require.Contains(t, err.Error(), "[]")
				require.Empty(t, stdout.String())
			}
			require.True(t, body.closed)
			require.Equal(t, "warning: --api-key is visible in process listings; prefer CAESIUM_API_KEY\n", stderr.String())
		})
	}
}

// Prior successful response reads discarded io.ReadAll errors, even after valid
// JSON bytes. Partial-read regressions accompany the complete-read fix.

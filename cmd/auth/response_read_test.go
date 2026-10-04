package auth

import (
	"bytes"
	"context"
	"errors"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
)

type responseReadTransport func(*http.Request) (*http.Response, error)

func (f responseReadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type responseReadBody struct {
	io.Reader
	closed bool
}

func (b *responseReadBody) Close() error { b.closed = true; return nil }

type responseReadFailure struct{ err error }

func (r responseReadFailure) Read([]byte) (int, error) { return 0, r.err }
func TestResponseRequiresCompleteRead(t *testing.T) {
	oldClient := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = oldClient })
	t.Setenv("CAESIUM_API_KEY", "")
	old_createServer := createServer
	createServer = "http://example.invalid"
	t.Cleanup(func() { createServer = old_createServer })
	old_listServer := listServer
	listServer = "http://example.invalid"
	t.Cleanup(func() { listServer = old_listServer })
	old_revokeServer := revokeServer
	revokeServer = "http://example.invalid"
	t.Cleanup(func() { revokeServer = old_revokeServer })
	old_rotateServer := rotateServer
	rotateServer = "http://example.invalid"
	t.Cleanup(func() { rotateServer = old_rotateServer })
	old_auditServer := auditServer
	auditServer = "http://example.invalid"
	t.Cleanup(func() { auditServer = old_auditServer })
	old_createAPIKey := createAPIKey
	createAPIKey = ""
	t.Cleanup(func() { createAPIKey = old_createAPIKey })
	old_listAPIKey := listAPIKey
	listAPIKey = ""
	t.Cleanup(func() { listAPIKey = old_listAPIKey })
	old_revokeAPIKey := revokeAPIKey
	revokeAPIKey = ""
	t.Cleanup(func() { revokeAPIKey = old_revokeAPIKey })
	old_rotateAPIKey := rotateAPIKey
	rotateAPIKey = ""
	t.Cleanup(func() { rotateAPIKey = old_rotateAPIKey })
	old_auditAPIKey := auditAPIKey
	auditAPIKey = ""
	t.Cleanup(func() { auditAPIKey = old_auditAPIKey })

	sentinel := errors.New("response interrupted")
	for _, tc := range []struct {
		name     string
		leaf     *cobra.Command
		args     []string
		fallback bool
	}{
		{"create", keyCreateCmd, nil, false},
		{"list", keyListCmd, nil, false},
		{"revoke", keyRevokeCmd, nil, false},
		{"rotate", keyRotateCmd, nil, false},
		{"audit", auditCmd, nil, false},
	} {
		for _, status := range []int{200, 500} {
			t.Run(tc.name+http.StatusText(status), func(t *testing.T) {
				body := &responseReadBody{Reader: io.MultiReader(strings.NewReader(`{"partial":true}`), responseReadFailure{sentinel})}
				http.DefaultClient = &http.Client{Transport: responseReadTransport(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
				})}
				var stdout, stderr bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				err := tc.leaf.RunE(cmd, tc.args)
				require.ErrorIs(t, err, sentinel)
				if status == 500 {
					require.Contains(t, err.Error(), "failed (500): {\"partial\":true}")
				} else {
					require.Contains(t, err.Error(), "response:")
				}
				require.Empty(t, stdout.String())
				require.Empty(t, stderr.String())
				require.True(t, body.closed)
			})
		}
		if tc.fallback {
			t.Run(tc.name+" complete raw fallback", func(t *testing.T) {
				body := &responseReadBody{Reader: strings.NewReader("not JSON")}
				http.DefaultClient = &http.Client{Transport: responseReadTransport(func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200, Body: body}, nil })}
				var stdout, stderr bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetContext(context.Background())
				cmd.SetOut(&stdout)
				cmd.SetErr(&stderr)
				require.NoError(t, tc.leaf.RunE(cmd, tc.args))
				require.Equal(t, "not JSON", stdout.String())
				require.Empty(t, stderr.String())
				require.True(t, body.closed)
			})
		}
	}
}

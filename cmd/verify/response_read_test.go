package verify

import (
	"bytes"
	"context"
	"errors"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	old_verifyServer := verifyServer
	verifyServer = "http://example.invalid"
	t.Cleanup(func() { verifyServer = old_verifyServer })
	path := filepath.Join(t.TempDir(), "receipt.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"job_id":"11111111-1111-1111-1111-111111111111","run_id":"22222222-2222-2222-2222-222222222222"}`), 0600))
	sentinel := errors.New("response interrupted")
	for _, tc := range []struct {
		name     string
		leaf     *cobra.Command
		args     []string
		fallback bool
	}{
		{"verify", Cmd, []string{path}, false},
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

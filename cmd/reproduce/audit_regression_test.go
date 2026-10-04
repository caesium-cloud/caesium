package reproduce

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

type descriptorTransport func(*http.Request) (*http.Response, error)

func (f descriptorTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type descriptorBody struct {
	io.Reader
	closed bool
}

func (b *descriptorBody) Close() error { b.closed = true; return nil }

type descriptorFailure struct{ err error }

func (r descriptorFailure) Read([]byte) (int, error) { return 0, r.err }
func TestDescriptorReadFailurePreservesStatus(t *testing.T) {
	oldClient, oldKey := httpClient, reproduceAPIKey
	t.Cleanup(func() { httpClient, reproduceAPIKey = oldClient, oldKey })
	reproduceAPIKey = ""
	t.Setenv("CAESIUM_API_KEY", "")
	sentinel := errors.New("descriptor interrupted")
	for _, tc := range []struct {
		status     int
		body, want string
	}{
		{200, `{}`, "read descriptor response:"},
		{500, "partial diagnostic", "fetch descriptor failed (500): partial diagnostic"},
		{404, "descriptor unavailable", "descriptor unavailable for run run task task"},
		{404, "other absence", "fetch descriptor failed (404): other absence"},
	} {
		t.Run(http.StatusText(tc.status)+tc.body, func(t *testing.T) {
			body := &descriptorBody{Reader: io.MultiReader(strings.NewReader(tc.body), descriptorFailure{sentinel})}
			httpClient = &http.Client{Transport: descriptorTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Body: body}, nil
			})}
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			data, err := fetchDescriptor(context.Background(), cmd, "http://example.invalid", "job", "run", "task")
			require.Nil(t, data)
			require.ErrorIs(t, err, sentinel)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, stdout.String())
			require.Empty(t, stderr.String())
			require.True(t, body.closed)
		})
	}
}

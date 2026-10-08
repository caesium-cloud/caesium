package dataset

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

type policyTransport func(*http.Request) (*http.Response, error)

func (f policyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type policyBody struct {
	io.Reader
	closed bool
}

func (b *policyBody) Close() error { b.closed = true; return nil }

type policyFailure struct{ err error }

func (r policyFailure) Read([]byte) (int, error) { return 0, r.err }
func TestRequestStatusAndReadPolicy(t *testing.T) {
	oldClient := httpClient
	t.Cleanup(func() { httpClient = oldClient })
	oldKey := apiKeyFlag
	apiKeyFlag = ""
	t.Cleanup(func() { apiKeyFlag = oldKey })
	t.Setenv(apiKeyEnvVar, "key")
	sentinel := errors.New("read interrupted")
	for _, status := range []int{200, 302, 500} {
		for _, readErr := range []error{nil, sentinel} {
			reader := io.Reader(strings.NewReader(" partial "))
			if readErr != nil {
				reader = io.MultiReader(reader, policyFailure{readErr})
			}
			body := &policyBody{Reader: reader}
			httpClient = &http.Client{Transport: policyTransport(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "Bearer key", req.Header.Get("Authorization"))
				require.Equal(t, "application/json", req.Header.Get("Content-Type"))
				return &http.Response{StatusCode: status, Body: body}, nil
			})}
			var stdout, stderr bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			data, err := request(cmd, http.MethodPost, "http://example.invalid", strings.NewReader("{}"))
			if readErr != nil {
				require.ErrorIs(t, err, sentinel)
				require.EqualError(t, err, "reading dataset response: read interrupted")
				require.Nil(t, data)
			} else {
				if status >= 400 {
					var statusErr *httpStatusError
					require.ErrorAs(t, err, &statusErr)
					require.Equal(t, status, statusErr.StatusCode)
					require.Equal(t, "partial", statusErr.Body)
					require.Nil(t, data)
				} else {
					require.NoError(t, err)
					require.Equal(t, " partial ", string(data))
				}
			}
			require.True(t, body.closed)
			require.Empty(t, stdout.String())
			require.Empty(t, stderr.String())
		}
	}
}

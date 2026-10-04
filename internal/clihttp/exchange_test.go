package clihttp

import (
	"context"
	"errors"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type closingBody struct {
	io.Reader
	closed bool
}

func (b *closingBody) Close() error { b.closed = true; return nil }

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }
func TestExchangePreservesResponseAndClosesBody(t *testing.T) {
	sentinel := errors.New("body interrupted")
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "marker")
	for _, readErr := range []error{nil, sentinel} {
		for _, status := range []int{200, 302, 500} {
			reader := io.Reader(strings.NewReader("partial"))
			if readErr != nil {
				reader = io.MultiReader(reader, failingReader{readErr})
			}
			body := &closingBody{Reader: reader}
			headers := http.Header{"Authorization": {"Bearer key"}, "X-Multiple": {"first", "second"}}
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "marker", req.Context().Value(contextKey{}))
				require.Equal(t, http.MethodPost, req.Method)
				require.Equal(t, headers, req.Header)
				req.Header.Set("Authorization", "changed")
				payload, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, "payload", string(payload))
				return &http.Response{StatusCode: status, Body: body}, nil
			})}
			data, gotStatus, err := Exchange(ctx, client, http.MethodPost, "http://example.invalid", strings.NewReader("payload"), headers)
			require.Equal(t, "partial", string(data))
			require.Equal(t, status, gotStatus)
			require.Equal(t, readErr, err)
			require.True(t, body.closed)
			require.Equal(t, "Bearer key", headers.Get("Authorization"))
		}
	}
}
func TestExchangeCancellationAndRequestErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })}
	data, status, err := Exchange(ctx, client, http.MethodGet, "http://example.invalid", nil, nil)
	require.Nil(t, data)
	require.Zero(t, status)
	require.ErrorIs(t, err, context.Canceled)
	called := false
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("unexpected") })
	_, status, err = Exchange(context.Background(), client, http.MethodGet, ":invalid", nil, nil)
	require.Error(t, err)
	require.Zero(t, status)
	require.False(t, called)
}

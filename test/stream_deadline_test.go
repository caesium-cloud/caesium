//go:build integration

package test

import (
	"bufio"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise both actual REST streams past the API server's 30-second write
// timeout. Healthy idle connections must keep delivering 15-second heartbeats.
func (s *IntegrationTestSuite) TestIdleStreamsOutliveHTTPWriteTimeout() {
	for _, path := range []string{"/v1/events?types=integration_idle_stream", "/v1/logs/stream?level=error"} {
		s.T().Run(path, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+path, nil)
			require.NoError(t, err)
			if s.authAPIKey != "" {
				req.Header.Set("Authorization", "Bearer "+s.authAPIKey)
			}
			started := time.Now()
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
			scanner := bufio.NewScanner(resp.Body)
			pings := 0
			for scanner.Scan() {
				if scanner.Text() == ": ping" {
					pings++
					if pings == 4 {
						require.Greater(t, time.Since(started), 30*time.Second)
						return
					}
				}
			}
			require.NoError(t, scanner.Err(), "stream disconnected before its fourth heartbeat")
			t.Fatalf("stream ended after %d heartbeats", pings)
		})
	}
}

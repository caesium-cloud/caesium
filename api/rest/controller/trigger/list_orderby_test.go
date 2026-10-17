package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestTriggerListOrderByIsAllowListed: ?order_by= accepts only column names
// and a direction; a raw expression is a 400, never SQL (identity-and-access
// B4).
func TestTriggerListOrderByIsAllowListed(t *testing.T) {
	parse := func(raw string) ([]string, error) {
		target := "/v1/triggers?order_by=" + url.QueryEscape(raw)
		c := echo.New().NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil), httptest.NewRecorder())
		req, err := parseListRequest(c)
		if err != nil {
			return nil, err
		}
		return req.OrderBy, nil
	}
	got, err := parse("alias desc, created_at")
	require.NoError(t, err)
	require.Equal(t, []string{"alias desc", "created_at asc"}, got)
	for _, raw := range []string{"(SELECT 1 FROM api_keys)", "configuration", "alias asc extra"} {
		_, err := parse(raw)
		require.Error(t, err, raw)
	}
}

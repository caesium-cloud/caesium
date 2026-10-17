package job

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// TestJobListOrderByIsAllowListed: ?order_by= accepts only column names and a
// direction. A raw expression (a subquery over rows the caller may not read)
// is a 400, never SQL (identity-and-access B4).
func TestJobListOrderByIsAllowListed(t *testing.T) {
	parse := func(raw string) ([]string, error) {
		target := "/v1/jobs?order_by=" + url.QueryEscape(raw)
		c := echo.New().NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil), httptest.NewRecorder())
		req, err := parseListRequest(c)
		if err != nil {
			return nil, err
		}
		return req.OrderBy, nil
	}
	for raw, want := range map[string][]string{
		"created_at desc":      {"created_at desc"},
		"alias, UPDATED_AT":    {"alias asc", "updated_at asc"},
		" namespace DESC , id": {"namespace desc", "id asc"},
	} {
		got, err := parse(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	for _, raw := range []string{
		"CASE WHEN EXISTS(SELECT 1 FROM jobs WHERE namespace='finance') THEN json('bad') ELSE 1 END",
		"alias; DROP TABLE jobs",
		"labels",
		"alias sideways",
	} {
		_, err := parse(raw)
		require.Error(t, err, raw)
	}
}

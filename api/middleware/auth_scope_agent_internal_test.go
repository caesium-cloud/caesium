package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestIsIncidentApprovalRoute(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/incidents/:id/approvals/:id/approve", true},
		{"/v1/incidents/:id/approvals/:id/reject", true},
		{"/v1/incidents/:id", false},
		{"/v1/incidents", false},
		{"/v1/jobs/:id", false},
		{"/v1/agent/incidents/:id/actions", false},
	}
	for _, tc := range cases {
		if got := isIncidentApprovalRoute(tc.path); got != tc.want {
			t.Fatalf("isIncidentApprovalRoute(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestConnectorScopeRefusesMalformedAgentMarkers(t *testing.T) {
	for _, scope := range []string{`{"agent":{}}`, `{"agent":{"incident_id":"00000000-0000-0000-0000-000000000000"}}`, `{"agent":"invalid"}`} {
		e := echo.New()
		c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/connectors/primary/executions/id/actions/action", nil), httptest.NewRecorder())
		_, err := authorizeScope(c, nil, []byte(scope), "/v1/connectors/:id/executions/:id/actions/:id")
		require.Error(t, err)
		require.Equal(t, http.StatusForbidden, err.(*echo.HTTPError).Code)
	}
}

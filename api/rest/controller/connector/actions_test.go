package connector

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestMessagePrincipalUsesAuthenticatedStableIdentityAndClusterRole(t *testing.T) {
	id := uuid.New()
	for _, test := range []struct {
		name           string
		kind           auth.PrincipalKind
		role           models.Role
		scope          []byte
		grants         policy.Grants
		write, allowed bool
	}{
		{name: "key operator", kind: auth.PrincipalAPIKey, role: models.RoleOperator, write: true, allowed: true},
		{name: "SSO admin", kind: auth.PrincipalUser, role: models.RoleAdmin, write: true, allowed: true},
		{name: "viewer Query", kind: auth.PrincipalAPIKey, role: models.RoleViewer, allowed: true},
		{name: "viewer action", kind: auth.PrincipalAPIKey, role: models.RoleViewer, write: true},
		{name: "runner action", kind: auth.PrincipalAPIKey, role: models.RoleRunner, write: true},
		{name: "scoped admin", kind: auth.PrincipalAPIKey, role: models.RoleAdmin, write: true, scope: []byte(`{"jobs":["local"]}`)},
		{name: "namespace admin", kind: auth.PrincipalAPIKey, role: models.RoleAdmin, write: true, grants: policy.Grants{"tenant": models.RoleAdmin}},
		{name: "agent admin", kind: auth.PrincipalAPIKey, role: models.RoleAdmin, write: true, scope: []byte(`{"agent":{"incident_id":"` + id.String() + `"}}`)},
		{name: "malformed agent admin", kind: auth.PrincipalAPIKey, role: models.RoleAdmin, write: true, scope: []byte(`{"agent":{}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			grants := test.grants
			if grants == nil {
				grants = policy.Grants{"*": test.role}
			}
			p := &auth.Principal{Kind: test.kind, KeyID: &id, UserID: &id, Grants: grants, Scope: test.scope, Subject: "server authenticated subject"}
			e := echo.New()
			c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil), httptest.NewRecorder())
			c.Set(authmw.ContextKeyPrincipal, p)
			actor, err := messagePrincipal(c, test.write)
			if !test.allowed {
				require.ErrorIs(t, err, echo.ErrForbidden)
				return
			}
			require.NoError(t, err)
			require.Equal(t, id.String(), actor.StableID)
			require.Equal(t, p.Subject, actor.Subject)
			require.Equal(t, string(p.Kind), string(actor.Kind))
			require.Equal(t, string(p.ClusterRole()), actor.Role)
		})
	}
}
func TestMessageBodyPreservesJSONNumbersAndRefusesExtraAuthority(t *testing.T) {
	for _, test := range []struct {
		name, body string
		allowed    bool
	}{
		{"valid", `{"binding_name":"publication","binding_version":"1","input":{"note":9007199254740993}}`, true},
		{"actor root", `{"binding_name":"publication","binding_version":"1","input":{},"_caesium_actor":{}}`, false},
		{"endpoint", `{"binding_name":"publication","binding_version":"1","input":{},"endpoint":"untrusted:7233"}`, false},
		{"second object", `{"binding_name":"publication","input":{}} {}`, false},
		{"oversized", `{"input":{"note":"` + strings.Repeat("x", 65536) + `"}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(test.body)), httptest.NewRecorder())
			var req messageRequest
			err := decodeMessage(c, &req)
			if !test.allowed {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "9007199254740993", req.Input["note"].(interface{ String() string }).String())
		})
	}
}

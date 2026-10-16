package connector

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestReadPrincipalRequiresUnscopedClusterViewer(t *testing.T) {
	id := uuid.New()
	scope, _ := json.Marshal(models.KeyScope{Jobs: []string{"local"}})
	cases := []struct {
		name      string
		principal *auth.Principal
		allowed   bool
	}{
		{"viewer", &auth.Principal{Kind: auth.PrincipalAPIKey, KeyID: &id, Grants: policy.Grants{"*": models.RoleViewer}}, true},
		{"SSO viewer", &auth.Principal{Kind: auth.PrincipalUser, UserID: &id, Grants: policy.Grants{"*": models.RoleViewer}}, true},
		{"job scoped admin", &auth.Principal{Kind: auth.PrincipalAPIKey, KeyID: &id, Grants: policy.Grants{"*": models.RoleAdmin}, Scope: scope}, false},
		{"namespace viewer", &auth.Principal{Kind: auth.PrincipalAPIKey, KeyID: &id, Grants: policy.Grants{"tenant": models.RoleViewer}}, false},
		{"malformed scope", &auth.Principal{Kind: auth.PrincipalAPIKey, KeyID: &id, Grants: policy.Grants{"*": models.RoleAdmin}, Scope: []byte(`{`)}, false},
		{"unauthenticated", nil, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			e := echo.New()
			c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/connectors", nil), httptest.NewRecorder())
			c.Set(authmw.ContextKeyPrincipal, test.principal)
			_, err := readPrincipal(c)
			if test.allowed {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, echo.ErrForbidden)
			}
		})
	}
}

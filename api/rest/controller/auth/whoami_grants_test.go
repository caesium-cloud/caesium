package auth

import (
	"net/http"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

// TestWhoamiReportsGrants: whoami adds grants, namespaces and cluster_role
// (design §14, §15) and keeps role equal to the cluster role, so a principal
// confined to namespaces reports an empty role, as before.
func TestWhoamiReportsGrants(t *testing.T) {
	for _, tc := range []struct {
		name       string
		grants     policy.Grants
		role       string
		grantsWant map[string]any
		namespaces []any
	}{
		{"cluster key", policy.Grants{"*": models.RoleAdmin}, "admin",
			map[string]any{"*": "admin"}, []any{"*"}},
		{"confined key", policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleViewer}, "",
			map[string]any{"marketing": "operator", "finance": "viewer"}, []any{"finance", "marketing"}},
		{"cluster viewer and namespace admin", policy.Grants{"*": models.RoleViewer, "marketing": models.RoleAdmin}, "viewer",
			map[string]any{"*": "viewer", "marketing": "admin"}, []any{"*"}},
		{"no grants", policy.Grants{}, "",
			map[string]any{}, []any{}},
		{"unrecognised role is dropped", policy.Grants{"marketing": "superuser"}, "",
			map[string]any{}, []any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := NewSSO(nil, nil, "caesium_session")
			c, rec := newAuthContext(t, http.MethodGet, "/auth/whoami", "")
			c.Set(authmw.ContextKeyPrincipal, &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_x", Grants: tc.grants})
			require.NoError(t, ctrl.Whoami(c))
			require.Equal(t, http.StatusOK, rec.Code)

			body := readJSONBody[map[string]any](t, rec)
			require.Equal(t, tc.role, body["role"])
			require.Equal(t, tc.role, body["cluster_role"])
			require.Equal(t, tc.grantsWant, body["grants"])
			require.Equal(t, tc.namespaces, body["namespaces"])
			require.Equal(t, "api_key", body["kind"])
			require.Equal(t, "csk_live_x", body["subject"])
		})
	}
}

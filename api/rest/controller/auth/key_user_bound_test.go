package auth

import (
	"errors"
	"net/http"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestUserBoundKeysInKeyAdministration: a user-bound key is listed to a
// cluster admin with its user and hidden from a namespace admin, only a
// cluster admin may revoke it, and it is never rotated (a login mints a new
// one).
func TestUserBoundKeysInKeyAdministration(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := iauth.NewService(db)
	auditor := iauth.NewAuditLogger(db)
	ctrl := New(svc, auditor)

	user := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "ula", Email: "ula@example.com", Role: models.RoleViewer, CreatedAt: time.Now().UTC()}
	require.NoError(t, db.Create(user).Error)
	userKey, err := svc.MintUserBoundKey(t.Context(), iauth.MintUserKeyRequest{UserID: user.ID})
	require.NoError(t, err)
	mktKey, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleViewer, Namespaces: datatypes.JSON(`["marketing"]`)})
	require.NoError(t, err)

	mktAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_mkt", Grants: policy.Grants{"marketing": models.RoleAdmin}}
	clusterAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_root", Grants: policy.Grants{"*": models.RoleAdmin}}

	// A namespace admin's list never shows a user-bound key.
	c, rec := newAuthContext(t, http.MethodGet, "/v1/auth/keys", "")
	c.SetRequest(c.Request().WithContext(authz.WithAllowedNamespaces(c.Request().Context(), []string{"marketing"})))
	require.NoError(t, ctrl.ListKeys(c))
	restricted := readJSONBody[[]models.APIKey](t, rec)
	require.Len(t, restricted, 1)
	require.Equal(t, mktKey.Key.ID, restricted[0].ID)

	// A cluster admin's list shows it with its user and no hash.
	c, rec = newAuthContext(t, http.MethodGet, "/v1/auth/keys", "")
	require.NoError(t, ctrl.ListKeys(c))
	require.NotContains(t, rec.Body.String(), userKey.Key.KeyHash)
	all := readJSONBody[[]map[string]any](t, rec)
	require.Len(t, all, 2)
	var listed map[string]any
	for _, key := range all {
		if key["id"] == userKey.Key.ID.String() {
			listed = key
		}
	}
	require.NotNil(t, listed)
	require.Equal(t, user.ID.String(), listed["user_id"])
	require.NotContains(t, listed, "namespaces")
	for _, key := range all {
		if key["id"] == mktKey.Key.ID.String() {
			require.NotContains(t, key, "user_id", "a plain key's row is unchanged")
		}
	}

	call := func(p *iauth.Principal, verb string) (int, error) {
		id := userKey.Key.ID.String()
		c, rec := newAuthContext(t, http.MethodPost, "/v1/auth/keys/"+id+"/"+verb, `{}`)
		c.SetPathValues(echo.PathValues{{Name: "id", Value: id}})
		c.Set(authmw.ContextKeyPrincipal, p)
		if verb == "revoke" {
			err := ctrl.RevokeKey(c)
			return rec.Code, err
		}
		err := ctrl.RotateKey(c)
		return rec.Code, err
	}

	for _, verb := range []string{"rotate", "revoke"} {
		_, err := call(mktAdmin, verb)
		denial, ok := errors.AsType[*authz.Denial](err)
		require.True(t, ok, "%s by a namespace admin: got %v", verb, err)
		require.Equal(t, []string{"*"}, denial.Namespaces, "a user-bound key is a cluster-only target")
	}

	_, err = call(clusterAdmin, "rotate")
	requireHTTPError(t, err, http.StatusConflict, "user-bound")
	stored, err := svc.ListKeys()
	require.NoError(t, err)
	require.Len(t, stored, 2, "a refused rotation mints nothing")

	code, err := call(clusterAdmin, "revoke")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	_, _, err = svc.AuthenticateKey(t.Context(), userKey.Plaintext)
	require.ErrorIs(t, err, iauth.ErrKeyRevoked)
	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyRevoke})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "*", entries[0].Namespace)
	require.Equal(t, map[string]any{"user_id": user.ID.String()}, auditMeta(t, entries[0]))
}

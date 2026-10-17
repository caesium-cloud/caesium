package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// useFilePolicy serves a policy file declaring namespaces (plus default) as
// the process policy for the test.
func useFilePolicy(t *testing.T, namespaces ...string) {
	t.Helper()
	doc := "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  default: {}\n"
	for _, ns := range namespaces {
		doc += "  " + ns + ": {}\n"
	}
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
}

// createKeyAs calls CreateKey as p and returns a reader for the 201 body.
func createKeyAs(t *testing.T, ctrl *Controller, p *iauth.Principal, body string) (func() createKeyResponse, error) {
	t.Helper()
	c, rec := newAuthContext(t, http.MethodPost, "/v1/auth/keys", body)
	if p != nil {
		c.Set(authmw.ContextKeyPrincipal, p)
	}
	err := ctrl.CreateKey(c)
	return func() createKeyResponse {
		require.Equal(t, http.StatusCreated, rec.Code)
		return readJSONBody[createKeyResponse](t, rec)
	}, err
}

func requireHTTPError(t *testing.T, err error, code int, contains string) {
	t.Helper()
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok, "want an HTTP %d, got %v", code, err)
	require.Equal(t, code, he.Code)
	if contains != "" {
		require.Contains(t, he.Message, contains)
	}
}

func TestCreateKeyWithNamespaces(t *testing.T) {
	ctrl, svc, auditor := setupControllerDeps(t)
	mktAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_mkt", Grants: policy.Grants{"marketing": models.RoleAdmin}}

	read, err := createKeyAs(t, ctrl, mktAdmin, `{"role":"operator","namespaces":["marketing"," marketing"]}`)
	require.NoError(t, err)
	resp := read()
	require.JSONEq(t, `["marketing"]`, string(resp.APIKey.Namespaces))
	require.Equal(t, "csk_live_mkt", resp.APIKey.CreatedBy)
	key, err := svc.ValidateKey(resp.Key)
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"marketing": models.RoleOperator}, iauth.PrincipalFromKey(key).Grants)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "marketing", entries[0].Namespace)
	require.Equal(t, "csk_live_mkt", entries[0].Actor)
	require.Contains(t, string(entries[0].Metadata), `"namespaces":["marketing"]`)

	// An explicit "*" is a cluster key: stored NULL, no namespaces field.
	clusterAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_root", Grants: policy.Grants{"*": models.RoleAdmin}}
	read, err = createKeyAs(t, ctrl, clusterAdmin, `{"role":"viewer","namespaces":["*"]}`)
	require.NoError(t, err)
	require.Empty(t, read().APIKey.Namespaces)
}

// TestCreateKeyMintingConstraint: the handler re-checks the minting
// constraint against the principal (the middleware checked it first).
func TestCreateKeyMintingConstraint(t *testing.T) {
	ctrl, _, auditor := setupControllerDeps(t)
	mktAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_mkt", Grants: policy.Grants{"marketing": models.RoleAdmin}}
	mktOperator := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_op", Grants: policy.Grants{"marketing": models.RoleOperator}}
	scopedAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_scoped", Grants: policy.Grants{"*": models.RoleAdmin},
		Scope: []byte(`{"jobs":["etl"]}`)}

	for _, tc := range []struct {
		name     string
		p        *iauth.Principal
		body     string
		reason   string
		required models.Role
		missing  []string
		message  string
	}{
		{"another namespace", mktAdmin, `{"role":"operator","namespaces":["finance"]}`, "insufficient_role", models.RoleOperator, []string{"finance"}, higherPrivilegeMessage},
		{"cluster-wide", mktAdmin, `{"role":"viewer"}`, "insufficient_role", models.RoleViewer, []string{"*"}, higherPrivilegeMessage},
		{"one namespace too many", mktAdmin, `{"role":"viewer","namespaces":["marketing","finance"]}`, "insufficient_role", models.RoleViewer, []string{"finance"}, higherPrivilegeMessage},
		{"within grants but not admin", mktOperator, `{"role":"viewer","namespaces":["marketing"]}`, "insufficient_role", models.RoleAdmin, []string{"marketing"}, "insufficient permissions"},
		{"job-scoped caller", scopedAdmin, `{"role":"viewer"}`, "insufficient_scope", models.RoleAdmin, []string{"*"}, "insufficient permissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createKeyAs(t, ctrl, tc.p, tc.body)
			denial, ok := errors.AsType[*authz.Denial](err)
			require.True(t, ok, "got %v", err)
			require.Equal(t, tc.reason, denial.Reason)
			require.Equal(t, tc.required, denial.Required)
			require.Equal(t, tc.missing, denial.Namespaces)
			require.Equal(t, tc.message, denial.Message)
		})
	}
	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyCreate})
	require.NoError(t, err)
	require.Empty(t, entries, "a refused mint writes no key")
}

func TestCreateKeyRejectsInvalidNamespaces(t *testing.T) {
	ctrl, _, _ := setupControllerDeps(t)
	admin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_root", Grants: policy.Grants{"*": models.RoleAdmin}}
	for body, want := range map[string]string{
		`{"role":"viewer","namespaces":[]}`:               "at least one namespace",
		`{"role":"viewer","namespaces":["Bad_NS"]}`:       `"Bad_NS"`,
		`{"role":"viewer","namespaces":["*","finance"]}`:  "cannot be combined",
		`{"role":"viewer","namespaces":["marketing",""]}`: "must not be empty",
	} {
		_, err := createKeyAs(t, ctrl, admin, body)
		requireHTTPError(t, err, http.StatusBadRequest, want)
	}
}

// TestCreateKeyRejectsAJobListNamingNoJob: blank aliases are dropped when the
// scope is read, so a list of only blanks would mint an unscoped key.
func TestCreateKeyRejectsAJobListNamingNoJob(t *testing.T) {
	ctrl, _, _ := setupControllerDeps(t)
	admin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_root", Grants: policy.Grants{"*": models.RoleAdmin}}
	for _, scope := range []string{`{"jobs":[]}`, `{"jobs":[""]}`, `{"jobs":[" ","  "]}`} {
		_, err := createKeyAs(t, ctrl, admin, `{"role":"runner","scope":`+scope+`}`)
		requireHTTPError(t, err, http.StatusBadRequest, "scope.jobs must name at least one job alias")
	}
	for _, scope := range []string{`{}`, `{"jobs":null}`, `{"jobs":[" etl "," "]}`} {
		_, err := createKeyAs(t, ctrl, admin, `{"role":"runner","scope":`+scope+`}`)
		require.NoError(t, err, scope)
	}
}

// TestCreateKeyRejectsAMalformedAgentClaim: an agent claim without an incident
// is read back as no claim at all (auth.DecodeAgentClaim), so it is refused as
// malformed and never exempts a job list that names no job — that pair used to
// mint a key that is unscoped on every job route.
func TestCreateKeyRejectsAMalformedAgentClaim(t *testing.T) {
	ctrl, svc, _ := setupControllerDeps(t)
	admin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_root", Grants: policy.Grants{"*": models.RoleAdmin}}
	for _, scope := range []string{
		`{"jobs":[" "],"agent":{}}`,
		`{"jobs":[],"agent":{}}`,
		`{"agent":{}}`,
		`{"agent":{"jobs":["etl"]}}`,
		`{"jobs":["etl"],"agent":{"incident_id":"00000000-0000-0000-0000-000000000000"}}`,
	} {
		_, err := createKeyAs(t, ctrl, admin, `{"role":"runner","scope":`+scope+`}`)
		requireHTTPError(t, err, http.StatusBadRequest, "scope.agent must name an incident_id")
	}
	keys, err := svc.ListKeys()
	require.NoError(t, err)
	require.Empty(t, keys, "a malformed agent claim mints no key")

	// A claim that names an incident still mints (REST scope.agent is
	// unchanged until A8) and is read back as an agent-session key.
	incident := uuid.New()
	read, err := createKeyAs(t, ctrl, admin, `{"role":"runner","scope":{"jobs":[" "],"agent":{"incident_id":"`+incident.String()+`"}}}`)
	require.NoError(t, err)
	claim, err := iauth.DecodeAgentClaim(read().APIKey.Scope)
	require.NoError(t, err)
	require.NotNil(t, claim)
	require.Equal(t, incident, claim.IncidentID)
}

func TestCreateKeyRejectsUndeclaredNamespaceWhenFileBacked(t *testing.T) {
	ctrl, _, _ := setupControllerDeps(t)
	admin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_root", Grants: policy.Grants{"*": models.RoleAdmin}}

	previous := policy.ProcessHolder()
	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	_, err := createKeyAs(t, ctrl, admin, `{"role":"viewer","namespaces":["nowhere"]}`)
	require.NoError(t, err, "with no policy file any namespace name is accepted")

	useFilePolicy(t, "marketing")
	_, err = createKeyAs(t, ctrl, admin, `{"role":"viewer","namespaces":["marketing","nowhere"]}`)
	requireHTTPError(t, err, http.StatusBadRequest, `namespace "nowhere" is not declared in the access policy`)
	_, err = createKeyAs(t, ctrl, admin, `{"role":"viewer","namespaces":["marketing"]}`)
	require.NoError(t, err)
	_, err = createKeyAs(t, ctrl, admin, `{"role":"viewer"}`)
	require.NoError(t, err, "a cluster key names no namespace to declare")
}

// TestCreateKeyBySSOUser: an SSO session principal can mint (the A5 fix: the
// handler used to read only the API-key context and answered 401).
func TestCreateKeyBySSOUser(t *testing.T) {
	ctrl, _, auditor := setupControllerDeps(t)
	userID := uuid.New()
	alice := &iauth.Principal{Kind: iauth.PrincipalUser, Subject: "alice@example.com", UserID: &userID, Grants: policy.Grants{"*": models.RoleAdmin}}

	read, err := createKeyAs(t, ctrl, alice, `{"role":"runner"}`)
	require.NoError(t, err)
	require.Equal(t, "alice@example.com", read().APIKey.CreatedBy)
	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "alice@example.com", entries[0].Actor)
	require.Equal(t, "*", entries[0].Namespace)
	require.NotContains(t, string(entries[0].Metadata), "namespaces", "a cluster key's audit row is unchanged")
}

func TestListKeysFiltersForARestrictedReader(t *testing.T) {
	ctrl, svc, _ := setupControllerDeps(t)
	ids := map[string]string{}
	for name, ns := range map[string]string{
		"mkt": `["marketing"]`, "fin": `["finance"]`, "both": `["finance","marketing"]`, "star": `["*"]`, "broken": `{bad`, "cluster": "",
	} {
		created, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "seed", Namespaces: datatypes.JSON(ns)})
		require.NoError(t, err)
		ids[name] = created.Key.ID.String()
	}

	c, rec := newAuthContext(t, http.MethodGet, "/v1/auth/keys", "")
	c.SetRequest(c.Request().WithContext(authz.WithAllowedNamespaces(c.Request().Context(), []string{"marketing"})))
	require.NoError(t, ctrl.ListKeys(c))
	require.True(t, authz.Filtered(c.Request().Context()), "the handler must mark the collection filtered")
	keys := readJSONBody[[]models.APIKey](t, rec)
	require.Len(t, keys, 1)
	require.Equal(t, ids["mkt"], keys[0].ID.String())

	// Unrestricted: every key, the corrupt one listed with its raw text.
	c, rec = newAuthContext(t, http.MethodGet, "/v1/auth/keys", "")
	require.NoError(t, ctrl.ListKeys(c))
	require.Equal(t, http.StatusOK, rec.Code)
	all := readJSONBody[[]map[string]any](t, rec)
	require.Len(t, all, 6)
	for _, key := range all {
		if key["id"] == ids["broken"] {
			require.Equal(t, "{bad", key["namespaces"])
		}
		if key["id"] == ids["cluster"] {
			require.NotContains(t, key, "namespaces")
		}
	}
}

func TestRevokeAndRotateCheckTheStoredKey(t *testing.T) {
	ctrl, svc, auditor := setupControllerDeps(t)
	mktAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_mkt", Grants: policy.Grants{"marketing": models.RoleAdmin}}
	seed := func(ns string, expiresAt *time.Time) *models.APIKey {
		created, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleRunner, CreatedBy: "seed", Namespaces: datatypes.JSON(ns), ExpiresAt: expiresAt})
		require.NoError(t, err)
		return created.Key
	}
	call := func(p *iauth.Principal, verb string, id uuid.UUID) error {
		c, _ := newAuthContext(t, http.MethodPost, "/v1/auth/keys/"+id.String()+"/"+verb, `{}`)
		c.SetPathValues(echo.PathValues{{Name: "id", Value: id.String()}})
		c.Set(authmw.ContextKeyPrincipal, p)
		if verb == "revoke" {
			return ctrl.RevokeKey(c)
		}
		return ctrl.RotateKey(c)
	}

	fin := seed(`["finance"]`, nil)
	for _, verb := range []string{"revoke", "rotate"} {
		err := call(mktAdmin, verb, fin.ID)
		denial, ok := errors.AsType[*authz.Denial](err)
		require.True(t, ok, "%s: got %v", verb, err)
		require.Equal(t, []string{"finance"}, denial.Namespaces)
	}
	stored, err := svc.ListKeys()
	require.NoError(t, err)
	for _, key := range stored {
		require.Nil(t, key.RevokedAt, "a refused revoke leaves the key active")
	}
	require.Len(t, stored, 1, "a refused rotation mints nothing")

	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	mkt := seed(`["marketing"]`, &expiry)
	err = call(mktAdmin, "rotate", mkt.ID)
	require.NoError(t, err)
	err = call(mktAdmin, "revoke", mkt.ID)
	require.NoError(t, err)
	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyRevoke})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "marketing", entries[0].Namespace)
	require.Equal(t, "csk_live_mkt", entries[0].Actor)
	require.Contains(t, string(entries[0].Metadata), `"namespaces":["marketing"]`)

	clusterAdmin := &iauth.Principal{Kind: iauth.PrincipalAPIKey, Subject: "csk_live_root", Grants: policy.Grants{"*": models.RoleAdmin}}
	past := time.Now().UTC().Add(-time.Minute)
	expired := seed("", &past)
	err = call(clusterAdmin, "rotate", expired.ID)
	requireHTTPError(t, err, http.StatusConflict, "expired")
	broken := seed(`{bad`, nil)
	err = call(clusterAdmin, "rotate", broken.ID)
	requireHTTPError(t, err, http.StatusConflict, "unusable")
	err = call(mktAdmin, "revoke", broken.ID)
	_, isDenial := errors.AsType[*authz.Denial](err)
	require.True(t, isDenial, "an unusable key is a cluster-only target")
	err = call(clusterAdmin, "revoke", broken.ID)
	require.NoError(t, err, "the cluster admin can still revoke it")

	useFilePolicy(t, "marketing")
	err = call(clusterAdmin, "rotate", fin.ID)
	requireHTTPError(t, err, http.StatusConflict, `namespace "finance" is not declared in the access policy`)
	err = call(clusterAdmin, "revoke", fin.ID)
	require.NoError(t, err, "revoking needs no declaration")
	err = call(clusterAdmin, "rotate", uuid.New())
	requireHTTPError(t, err, http.StatusNotFound, "")
}

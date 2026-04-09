package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func setupControllerDeps(t *testing.T) (*iauth.Service, *iauth.AuditLogger) {
	t.Helper()

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	originalService := Dependencies.Service
	originalAuditor := Dependencies.Auditor
	t.Cleanup(func() {
		Dependencies.Service = originalService
		Dependencies.Auditor = originalAuditor
	})

	svc := iauth.NewService(db)
	auditor := iauth.NewAuditLogger(db)
	Dependencies.Service = svc
	Dependencies.Auditor = auditor

	return svc, auditor
}

func newAuthContext(t *testing.T, method, target string, body string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	req.RemoteAddr = "198.51.100.8:1234"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

func readJSONBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func TestCreateKeyCreatesScopedKeyAndAuditEntry(t *testing.T) {
	svc, auditor := setupControllerDeps(t)
	c, rec := newAuthContext(t, http.MethodPost, "/v1/auth/keys", `{
		"description":"CI key",
		"role":"operator",
		"scope":{"jobs":["etl-daily"]},
		"expires_in":"2d"
	}`)

	c.Set(authmw.ContextKeyAuth, &models.APIKey{KeyPrefix: "csk_live_admin"})

	err := CreateKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)

	resp := readJSONBody[createKeyResponse](t, rec)
	require.NotEmpty(t, resp.Key)
	require.NotNil(t, resp.APIKey)
	require.Equal(t, models.RoleOperator, resp.APIKey.Role)
	require.Equal(t, "csk_live_admin", resp.APIKey.CreatedBy)
	require.Equal(t, "CI key", resp.APIKey.Description)
	require.NotNil(t, resp.APIKey.ExpiresAt)
	require.WithinDuration(t, time.Now().UTC().Add(48*time.Hour), *resp.APIKey.ExpiresAt, 5*time.Second)

	key, err := svc.ValidateKey(resp.Key)
	require.NoError(t, err)
	require.True(t, iauth.CheckScope(key.Scope, "etl-daily"))
	require.False(t, iauth.CheckScope(key.Scope, "other-job"))

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "csk_live_admin", entries[0].Actor)
	require.Equal(t, resp.APIKey.ID.String(), entries[0].ResourceID)
}

func TestCreateKeyRejectsInvalidRole(t *testing.T) {
	setupControllerDeps(t)
	c, _ := newAuthContext(t, http.MethodPost, "/v1/auth/keys", `{"role":"bogus"}`)

	err := CreateKey(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

func TestListKeysReturnsPersistedKeys(t *testing.T) {
	svc, _ := setupControllerDeps(t)
	_, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)

	c, rec := newAuthContext(t, http.MethodGet, "/v1/auth/keys", "")
	err = ListKeys(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	var keys []models.APIKey
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &keys))
	require.Len(t, keys, 1)
	require.Equal(t, models.RoleViewer, keys[0].Role)
}

func TestRevokeKeyRevokesAndAudits(t *testing.T) {
	svc, auditor := setupControllerDeps(t)
	resp, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleRunner, CreatedBy: "seed"})
	require.NoError(t, err)

	c, rec := newAuthContext(t, http.MethodPost, "/v1/auth/keys/"+resp.Key.ID.String()+"/revoke", "")
	c.SetPathValues(echo.PathValues{{Name: "id", Value: resp.Key.ID.String()}})
	c.Set(authmw.ContextKeyAuth, &models.APIKey{KeyPrefix: "csk_live_admin"})

	err = RevokeKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	_, err = svc.ValidateKey(resp.Plaintext)
	require.ErrorIs(t, err, iauth.ErrKeyRevoked)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyRevoke})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, resp.Key.ID.String(), entries[0].ResourceID)
}

func TestRotateKeyReturnsNewKeyAndAuditEntry(t *testing.T) {
	svc, auditor := setupControllerDeps(t)
	original, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleRunner, CreatedBy: "seed"})
	require.NoError(t, err)

	c, rec := newAuthContext(t, http.MethodPost, "/v1/auth/keys/"+original.Key.ID.String()+"/rotate", `{"grace_period":"2h"}`)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: original.Key.ID.String()}})
	c.Set(authmw.ContextKeyAuth, &models.APIKey{KeyPrefix: "csk_live_admin"})

	err = RotateKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)

	resp := readJSONBody[createKeyResponse](t, rec)
	require.NotEmpty(t, resp.Key)
	require.NotEqual(t, original.Key.ID, resp.APIKey.ID)

	newKey, err := svc.ValidateKey(resp.Key)
	require.NoError(t, err)
	require.Equal(t, resp.APIKey.ID, newKey.ID)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyRotate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, original.Key.ID.String(), entries[0].ResourceID)
}

func TestQueryAuditSupportsDurationAndActorFilters(t *testing.T) {
	_, auditor := setupControllerDeps(t)

	require.NoError(t, auditor.Log(iauth.AuditEntry{
		Actor:    "alpha",
		Action:   iauth.ActionKeyCreate,
		Outcome:  iauth.OutcomeSuccess,
		SourceIP: "198.51.100.1",
	}))
	require.NoError(t, auditor.Log(iauth.AuditEntry{
		Actor:    "beta",
		Action:   iauth.ActionKeyRevoke,
		Outcome:  iauth.OutcomeSuccess,
		SourceIP: "198.51.100.2",
	}))

	c, rec := newAuthContext(t, http.MethodGet, "/v1/auth/audit?since=24h&actor=beta&limit=10", "")
	err := QueryAudit(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	var entries []models.AuditLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
	require.Len(t, entries, 1)
	require.Equal(t, "beta", entries[0].Actor)
	require.Equal(t, iauth.ActionKeyRevoke, entries[0].Action)
}

func TestQueryAuditRejectsInvalidSince(t *testing.T) {
	setupControllerDeps(t)
	c, _ := newAuthContext(t, http.MethodGet, "/v1/auth/audit?since=not-a-duration-or-time", "")

	err := QueryAudit(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

func TestParseDurationSupportsDays(t *testing.T) {
	dur, err := parseDuration("3d")
	require.NoError(t, err)
	require.Equal(t, 72*time.Hour, dur)
}

func TestRevokeKeyRejectsInvalidUUID(t *testing.T) {
	setupControllerDeps(t)
	c, _ := newAuthContext(t, http.MethodPost, "/v1/auth/keys/not-a-uuid/revoke", "")
	c.SetPathValues(echo.PathValues{{Name: "id", Value: "not-a-uuid"}})

	err := RevokeKey(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

func TestRotateKeyRejectsRevokedKey(t *testing.T) {
	svc, _ := setupControllerDeps(t)
	resp, err := svc.CreateKey(&iauth.CreateKeyRequest{Role: models.RoleRunner, CreatedBy: "seed"})
	require.NoError(t, err)
	require.NoError(t, svc.RevokeKey(resp.Key.ID))

	c, _ := newAuthContext(t, http.MethodPost, "/v1/auth/keys/"+resp.Key.ID.String()+"/rotate", `{}`)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: resp.Key.ID.String()}})

	err = RotateKey(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusConflict, he.Code)
}

func TestQueryAuditAcceptsRFC3339Since(t *testing.T) {
	_, auditor := setupControllerDeps(t)
	require.NoError(t, auditor.Log(iauth.AuditEntry{
		Actor:    "alpha",
		Action:   iauth.ActionKeyCreate,
		Outcome:  iauth.OutcomeSuccess,
		SourceIP: "198.51.100.1",
	}))

	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	c, rec := newAuthContext(t, http.MethodGet, "/v1/auth/audit?since="+since, "")
	err := QueryAudit(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	var entries []models.AuditLog
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))
	require.Len(t, entries, 1)
}

func TestCreateKeyUsesUnknownActorWhenMiddlewareIdentityMissing(t *testing.T) {
	_, auditor := setupControllerDeps(t)
	c, rec := newAuthContext(t, http.MethodPost, "/v1/auth/keys", `{"role":"viewer"}`)

	err := CreateKey(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)

	entries, err := auditor.Query(&iauth.AuditQueryRequest{Action: iauth.ActionKeyCreate})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "unknown", entries[0].Actor)
}

func TestRotateKeyRejectsInvalidGracePeriod(t *testing.T) {
	setupControllerDeps(t)
	id := uuid.New().String()
	c, _ := newAuthContext(t, http.MethodPost, "/v1/auth/keys/"+id+"/rotate", `{"grace_period":"nope"}`)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: id}})

	err := RotateKey(c)
	require.Error(t, err)

	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusBadRequest, he.Code)
}

package middleware_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// storedKey seeds a key whose namespaces column holds namespaces verbatim (""
// keeps SQL NULL) and returns its id, the target of a revoke or rotate.
func (f *nsFixture) storedKey(t *testing.T, namespaces string) string {
	t.Helper()
	created, err := f.svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
	require.NoError(t, err)
	if namespaces != "" {
		require.NoError(t, f.db.Model(&models.APIKey{}).Where("id = ?", created.Key.ID).
			Update("namespaces", datatypes.JSON(namespaces)).Error)
	}
	return created.Key.ID.String()
}

// TestKeyCreateResolvesBodyNamespaces: minting needs admin in every namespace
// the body asks for, and "*" when it names none (A5, design §8.1).
func TestKeyCreateResolvesBodyNamespaces(t *testing.T) {
	f := newNSFixture(t)
	mktAdmin := f.key(t, models.RoleAdmin, `["marketing"]`)
	bothAdmin := f.key(t, models.RoleAdmin, `["finance","marketing"]`)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	clusterAdmin := f.key(t, models.RoleAdmin, "")
	create := func(body string) nsCall { return post("/v1/auth/keys", nil, body) }

	res := f.do(t, mktAdmin, create(`{"role":"operator","namespaces":[" marketing"]}`))
	require.Equal(t, http.StatusOK, res.status)
	require.Equal(t, []string{"marketing"}, res.decision.Namespaces)
	require.Equal(t, `{"role":"operator","namespaces":[" marketing"]}`, res.body, "the handler reads the whole body")

	for _, tc := range []struct {
		body      string
		namespace string
	}{
		{`{"role":"operator","namespaces":["finance"]}`, "finance"},
		{`{"role":"viewer"}`, "*"},
		{`{"role":"viewer","namespaces":null}`, "*"},
		{`{"role":"viewer","namespaces":["*"]}`, "*"},
		{`{"role":"viewer","namespaces":["marketing","finance"]}`, "*"},
	} {
		res := f.do(t, mktAdmin, create(tc.body))
		require.Equal(t, http.StatusForbidden, res.status, tc.body)
		denial := f.lastDenial(t)
		require.Equal(t, tc.namespace, denial.Namespace, tc.body)
		requireAuditMetadata(t, denial, map[string]any{"reason": "insufficient_role", "policy_key": "POST /v1/auth/keys"})
	}

	res = f.do(t, bothAdmin, create(`{"role":"admin","namespaces":["marketing","finance"]}`))
	require.Equal(t, http.StatusOK, res.status)
	require.Equal(t, []string{"finance", "marketing"}, res.decision.Namespaces)

	// An operator holds no admin anywhere: denied before the body is read.
	require.Equal(t, http.StatusForbidden, f.do(t, mktOperator, create(`{"role":"viewer","namespaces":["marketing"]}`)).status)

	// Malformed bodies answer the handler's status.
	for _, body := range []string{
		`{"role":"viewer","namespaces":[]}`,
		`{"role":"viewer","namespaces":["Bad_NS"]}`,
		`{"role":"viewer","namespaces":["*","marketing"]}`,
		`{"role":"viewer","namespaces":"marketing"}`,
		`{"role":`,
	} {
		require.Equal(t, http.StatusBadRequest, f.do(t, mktAdmin, create(body)).status, body)
	}
	textBody := create(`{"role":"viewer","namespaces":["marketing"]}`)
	textBody.contentType = "text/plain"
	require.Equal(t, http.StatusUnsupportedMediaType, f.do(t, mktAdmin, textBody).status)
	huge := `{"role":"viewer","namespaces":["marketing"],"description":"` + strings.Repeat("x", 1<<20) + `"}`
	require.Equal(t, http.StatusRequestEntityTooLarge, f.do(t, mktAdmin, create(huge)).status)

	// A cluster admin is decided by its cluster role, with nothing resolved.
	res = f.do(t, clusterAdmin, create(`{"role":"admin","namespaces":["finance"]}`))
	require.Equal(t, http.StatusOK, res.status)
	require.Nil(t, res.decision.Namespaces)
}

// TestKeyAdministrationResolvesStoredNamespaces: revoke and rotate need admin
// in every namespace the stored key is confined to; a cluster key needs it at
// "*", and an unusable value is reachable only by a cluster admin.
func TestKeyAdministrationResolvesStoredNamespaces(t *testing.T) {
	f := newNSFixture(t)
	mktAdmin := f.key(t, models.RoleAdmin, `["marketing"]`)
	clusterAdmin := f.key(t, models.RoleAdmin, "")
	mktKey := f.storedKey(t, `["marketing"]`)
	finKey := f.storedKey(t, `["finance"]`)
	bothKey := f.storedKey(t, `["finance","marketing"]`)
	clusterKey := f.storedKey(t, "")
	explicitClusterKey := f.storedKey(t, `["*"]`)
	brokenKey := f.storedKey(t, `{bad`)

	for _, pattern := range []string{"/v1/auth/keys/:id/revoke", "/v1/auth/keys/:id/rotate"} {
		call := func(id string) nsCall { return post(pattern, pv("id", id), "") }

		res := f.do(t, mktAdmin, call(mktKey))
		require.Equal(t, http.StatusOK, res.status, pattern)
		require.Equal(t, []string{"marketing"}, res.decision.Namespaces)

		for _, tc := range []struct {
			id        string
			namespace string
		}{
			{finKey, "finance"},
			{bothKey, "*"},
			{clusterKey, "*"},
			{explicitClusterKey, "*"},
			{brokenKey, "*"},
		} {
			require.Equal(t, http.StatusForbidden, f.do(t, mktAdmin, call(tc.id)).status, pattern+" "+tc.id)
			require.Equal(t, tc.namespace, f.lastDenial(t).Namespace, pattern+" "+tc.id)
		}
		require.Equal(t, http.StatusNotFound, f.do(t, mktAdmin, call(uuid.NewString())).status, pattern)
		require.Equal(t, http.StatusBadRequest, f.do(t, mktAdmin, call("not-a-uuid")).status, pattern)

		// The cluster admin reaches every key, the unusable one included.
		for _, id := range []string{finKey, clusterKey, brokenKey} {
			res := f.do(t, clusterAdmin, call(id))
			require.Equal(t, http.StatusOK, res.status, pattern+" "+id)
			require.Nil(t, res.decision.Namespaces)
		}
	}
}

package auth_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestAuditQueryNamespaceFilter: ?namespace=X is an exact filter, and an
// omitted namespace applies none — cluster, namespaced, identity ("") and
// pre-migration (NULL) rows all come back, exactly as before the column.
func TestAuditQueryNamespaceFilter(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	for actor, namespace := range map[string]string{
		"cluster":   "*",
		"marketing": "marketing",
		"finance":   "finance",
		"identity":  "",
	} {
		require.NoError(t, auditor.Log(auth.AuditEntry{Actor: actor, Action: auth.ActionJobPause, Outcome: auth.OutcomeSuccess, Namespace: namespace}))
	}
	require.NoError(t, auditor.Log(auth.AuditEntry{Actor: "legacy", Action: auth.ActionJobPause, Outcome: auth.OutcomeSuccess}))
	require.NoError(t, db.Exec("UPDATE audit_logs SET namespace = NULL WHERE actor = 'legacy'").Error)

	actors := func(req *auth.AuditQueryRequest) []string {
		entries, err := auditor.Query(req)
		require.NoError(t, err)
		out := make([]string, 0, len(entries))
		for _, entry := range entries {
			out = append(out, entry.Actor)
		}
		return out
	}

	require.ElementsMatch(t, []string{"cluster", "marketing", "finance", "identity", "legacy"}, actors(&auth.AuditQueryRequest{}))
	require.Equal(t, []string{"marketing"}, actors(&auth.AuditQueryRequest{Namespace: "marketing"}))
	require.Equal(t, []string{"cluster"}, actors(&auth.AuditQueryRequest{Namespace: "*"}))
	require.Empty(t, actors(&auth.AuditQueryRequest{Namespace: "ops"}))

	var stored models.AuditLog
	require.NoError(t, db.Where("actor = ?", "marketing").First(&stored).Error)
	require.Equal(t, "marketing", stored.Namespace)
}

func TestKeyAuditNamespace(t *testing.T) {
	for _, tc := range []struct {
		namespaces string
		want       string
	}{
		{"", "*"},
		{`["*"]`, "*"},
		{`["marketing"]`, "marketing"},
		{`["marketing","marketing"]`, "marketing"},
		{`["marketing","finance"]`, "*"},
		{`["marketing","*"]`, "*"},
		{`[]`, "*"},
		{`{bad`, "*"},
	} {
		require.Equal(t, tc.want, auth.KeyAuditNamespace(datatypes.JSON(tc.namespaces)), tc.namespaces)
	}
}

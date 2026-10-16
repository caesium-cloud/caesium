package auth_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestKnownUserGroups(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	require.Nil(t, auth.KnownUserGroups(nil))

	known, err := auth.KnownUserGroups(db)(t.Context())
	require.NoError(t, err)
	require.Empty(t, known)

	now := time.Now().UTC()
	for i, groups := range []datatypes.JSON{
		datatypes.JSON(`["marketing-eng"," spaced "]`),
		datatypes.JSON(`["CN=Admins,OU=Groups","marketing-eng"]`),
		datatypes.JSON(`not json`), // grants nothing, so matches nothing
		nil,
	} {
		require.NoError(t, db.Create(&models.User{
			ID: uuid.New(), Issuer: "oidc", Subject: uuid.NewString(), Email: "u@example.com",
			Groups: groups, Role: models.RoleViewer, CreatedAt: now.Add(time.Duration(i)),
		}).Error)
	}
	known, err = auth.KnownUserGroups(db)(t.Context())
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"marketing-eng": {}, " spaced ": {}, "CN=Admins,OU=Groups": {}}, known)

	// The loader's unmatched-group warning reads the stored groups exactly.
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {groups: ["marketing-eng", "spaced", "*"]}
    role: viewer
    namespaces: ["*"]
`), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path, KnownGroups: auth.KnownUserGroups(db)})
	require.NoError(t, err)
	require.Equal(t, []string{`bindings[0].subjects.groups: "spaced" matches no known user's groups`}, holder.Current().Warnings)

	// A database error skips the check; the load still succeeds.
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	_, err = auth.KnownUserGroups(db)(t.Context())
	require.Error(t, err)
	holder, err = policy.LoadFile(t.Context(), policy.HolderOptions{Path: path, KnownGroups: auth.KnownUserGroups(db)})
	require.NoError(t, err)
	require.Equal(t, []string{}, holder.Current().Warnings)
}

func TestServicePolicyHolderIsIndependentOfTheSource(t *testing.T) {
	holder := policy.NewEnvHolder(nil, "", nil)
	svc := auth.NewService(nil, auth.WithPolicyHolder(holder))
	require.Same(t, holder, svc.PolicyHolder())
	require.True(t, svc.PolicySource() == nil, "installing the holder must not install a grant source")

	var nilSvc *auth.Service
	require.Nil(t, nilSvc.PolicyHolder())
	require.Nil(t, auth.NewService(nil).PolicyHolder())
}

func TestPolicyReloadAuditEntry(t *testing.T) {
	require.Equal(t, policy.PollActor, auth.ActorPolicyLoader)

	ok := auth.PolicyReloadAuditEntry(policy.ReloadEvent{
		Trigger: policy.TriggerAPI, Actor: "csk_live_admin", SourceIP: "198.51.100.8", Path: "/etc/caesium/policy.yaml",
		OK: true, Changed: true, PreviousRevision: "old", Revision: "new",
	})
	require.Equal(t, auth.AuditEntry{
		Actor: "csk_live_admin", Action: auth.ActionPolicyReloaded, ResourceType: "access_policy", ResourceID: "new",
		SourceIP: "198.51.100.8", Outcome: auth.OutcomeSuccess,
		Metadata: map[string]any{
			"trigger": "api", "path": "/etc/caesium/policy.yaml", "revision": "new", "previous_revision": "old", "changed": true,
		},
	}, ok)

	failed := auth.PolicyReloadAuditEntry(policy.ReloadEvent{
		Trigger: policy.TriggerPoll, Actor: policy.PollActor, Path: "/p.yaml",
		PreviousRevision: "old", Revision: "bad", Error: `access policy file /p.yaml: bindings[0].role: invalid role "x"`,
	})
	require.Equal(t, auth.AuditEntry{
		Actor: "system:policy-loader", Action: auth.ActionPolicyReloadFailed, ResourceType: "access_policy", ResourceID: "bad",
		Outcome: auth.OutcomeError,
		Metadata: map[string]any{
			"trigger": "poll", "path": "/p.yaml", "revision": "bad", "previous_revision": "old",
			"error": `access policy file /p.yaml: bindings[0].role: invalid role "x"`,
		},
	}, failed)

	require.Equal(t, "unknown", auth.PolicyReloadAuditEntry(policy.ReloadEvent{OK: true}).Actor)
}

func TestPolicyReloadAuditorWritesRows(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := auth.NewAuditLogger(db)

	path := filepath.Join(t.TempDir(), "policy.yaml")
	good := "apiVersion: v1\nkind: AccessPolicy\n"
	require.NoError(t, os.WriteFile(path, []byte(good), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	holder.SetObserver(auth.PolicyReloadAuditor(auditor))

	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v2\nkind: AccessPolicy\n"), 0o600))
	_, err = holder.Reload(t.Context(), policy.ReloadRequest{Actor: "csk_live_admin", SourceIP: "203.0.113.4"})
	require.Error(t, err)

	rows, err := auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionPolicyReloadFailed})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "csk_live_admin", rows[0].Actor)
	require.Equal(t, "203.0.113.4", rows[0].SourceIP)
	require.Equal(t, auth.OutcomeError, rows[0].Outcome)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Metadata, &meta))
	require.Contains(t, meta["error"], `unsupported access policy apiVersion "v2"`)
	require.Equal(t, policy.Hash([]byte(good)), meta["previous_revision"])

	require.NoError(t, os.WriteFile(path, []byte(good+"bindings: []\n"), 0o600))
	_, err = holder.Reload(t.Context(), policy.ReloadRequest{Actor: "csk_live_admin"})
	require.NoError(t, err)
	rows, err = auditor.Query(&auth.AuditQueryRequest{Action: auth.ActionPolicyReloaded})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, policy.Hash([]byte(good+"bindings: []\n")), rows[0].ResourceID)

	// A nil auditor is inert.
	auth.PolicyReloadAuditor(nil)(policy.ReloadEvent{})
}

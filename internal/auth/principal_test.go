package auth

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestPrincipalFromKeyNullNamespacesIsCluster(t *testing.T) {
	for _, namespaces := range []datatypes.JSON{nil, {}} {
		p := PrincipalFromKey(&models.APIKey{ID: uuid.New(), Role: models.RoleOperator, Namespaces: namespaces})
		assert.Equal(t, policy.Grants{"*": models.RoleOperator}, p.Grants)
		assert.Equal(t, models.RoleOperator, p.ClusterRole())
		assert.Equal(t, models.RoleOperator, p.RoleIn("marketing"))
		assert.Nil(t, p.AllowedNamespaces())
	}
}

func TestPrincipalFromKeyNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name       string
		namespaces string
		want       policy.Grants
	}{
		{"confined", `["marketing","finance"]`, policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleOperator}},
		{"explicit cluster", `["*"]`, policy.Grants{"*": models.RoleOperator}},
		{"empty entries skipped", `["", "marketing"]`, policy.Grants{"marketing": models.RoleOperator}},
		{"json null denies", `null`, policy.Grants{}},
		{"empty list denies", `[]`, policy.Grants{}},
		{"malformed denies", `{bad`, policy.Grants{}},
		{"non-array denies", `"*"`, policy.Grants{}},
		{"object denies", `{"*":"admin"}`, policy.Grants{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PrincipalFromKey(&models.APIKey{
				ID:         uuid.New(),
				KeyPrefix:  "csk_live_ns",
				Role:       models.RoleOperator,
				Namespaces: datatypes.JSON(tc.namespaces),
			})
			require.NotNil(t, p.Grants)
			assert.Equal(t, tc.want, p.Grants)
			if _, cluster := tc.want["*"]; !cluster {
				assert.Equal(t, models.Role(""), p.ClusterRole())
			}
		})
	}

	confined := PrincipalFromKey(&models.APIKey{Role: models.RoleOperator, Namespaces: datatypes.JSON(`["marketing"]`)})
	assert.Equal(t, models.RoleOperator, confined.RoleIn("marketing"))
	assert.Equal(t, models.Role(""), confined.RoleIn("finance"))
	assert.Equal(t, []string{"marketing"}, confined.AllowedNamespaces())
}

func TestPrincipalFromKeyPreservesScopeSubjectKeyID(t *testing.T) {
	id := uuid.New()
	key := &models.APIKey{
		ID:        id,
		KeyPrefix: "csk_live_ab",
		Role:      models.RoleOperator,
		Scope:     []byte(`{"jobs":["x"]}`),
	}
	p := PrincipalFromKey(key)
	assert.Equal(t, PrincipalAPIKey, p.Kind)
	assert.Equal(t, models.RoleOperator, p.ClusterRole())
	assert.Equal(t, "csk_live_ab", p.Subject)
	assert.Equal(t, []byte(`{"jobs":["x"]}`), p.Scope)
	assert.Equal(t, &id, p.KeyID)
	assert.Nil(t, p.UserID)
}

func TestPrincipalFromUser(t *testing.T) {
	id := uuid.New()
	u := &models.User{ID: id, Email: "a@b.com", Role: models.RoleAdmin, Groups: datatypes.JSON(`["viewers"]`)}
	p := PrincipalFromUser(u, mustTestRoleMapper(t, "viewers=viewer"))
	assert.Equal(t, PrincipalUser, p.Kind)
	// The stored role is the cluster role at last login; it never grants.
	assert.Equal(t, models.RoleViewer, p.ClusterRole())
	assert.Equal(t, "a@b.com", p.Subject)
	assert.Equal(t, &id, p.UserID)
	assert.Nil(t, p.Scope)
	assert.Nil(t, p.KeyID)
}

func TestPrincipalFromUserResolvesPerCall(t *testing.T) {
	src := mustTestRoleMapper(t, "viewers=viewer;admins=admin")
	u := &models.User{ID: uuid.New(), Email: "bob@example.com", Groups: datatypes.JSON(`["viewers"]`)}
	require.Equal(t, models.RoleViewer, PrincipalFromUser(u, src).ClusterRole())

	u.Groups = datatypes.JSON(`["admins"]`)
	require.Equal(t, models.RoleAdmin, PrincipalFromUser(u, src).ClusterRole())

	u.Groups = datatypes.JSON(`["nobody"]`)
	p := PrincipalFromUser(u, src)
	require.NotNil(t, p.Grants)
	require.Empty(t, p.Grants)
}

func TestPrincipalFromUserUnverifiedEmailDropped(t *testing.T) {
	src := emailOnlyAdminSource(t)
	u := &models.User{ID: uuid.New(), Email: "admin@example.com", EmailVerified: false}
	p := PrincipalFromUser(u, src)
	assert.Empty(t, p.Grants)
	assert.Equal(t, models.Role(""), p.ClusterRole())
	assert.Equal(t, "admin@example.com", p.Subject, "the audit actor stays the email, verified or not")

	u.EmailVerified = true
	assert.Equal(t, policy.Grants{"*": models.RoleAdmin}, PrincipalFromUser(u, src).Grants)
}

func TestPrincipalFromUserNilSourceAndTypedNil(t *testing.T) {
	u := &models.User{ID: uuid.New(), Email: "a@b.com", EmailVerified: true, Groups: datatypes.JSON(`["viewers"]`)}

	p := PrincipalFromUser(u, nil)
	require.NotNil(t, p.Grants)
	require.Empty(t, p.Grants)

	var typedNilMapper *RoleMapper
	p = PrincipalFromUser(u, typedNilMapper)
	require.NotNil(t, p.Grants)
	require.Empty(t, p.Grants)

	var typedNilStatic *policy.Static
	p = PrincipalFromUser(u, typedNilStatic)
	require.NotNil(t, p.Grants)
	require.Empty(t, p.Grants)
}

func TestPrincipalFromUserMalformedGroupsFailsClosed(t *testing.T) {
	src := &policy.Static{DefaultRole: models.RoleViewer}
	for _, groups := range []string{`{bad`, `"viewers"`, `{"g":1}`} {
		u := &models.User{ID: uuid.New(), Groups: datatypes.JSON(groups)}
		p := PrincipalFromUser(u, src)
		require.NotNil(t, p.Grants)
		require.Empty(t, p.Grants, "groups %s must fail closed, not fall back to the default role", groups)
	}
	// Absent or null groups are no groups, so the fallback still applies.
	for _, groups := range []datatypes.JSON{nil, datatypes.JSON(`null`), datatypes.JSON(`[]`)} {
		u := &models.User{ID: uuid.New(), Groups: groups}
		require.Equal(t, policy.Grants{"*": models.RoleViewer}, PrincipalFromUser(u, src).Grants)
	}
}

func TestPrincipalNilSafeMethods(t *testing.T) {
	var p *Principal
	require.Equal(t, models.Role(""), p.RoleIn("marketing"))
	require.Equal(t, models.Role(""), p.ClusterRole())
	require.NotNil(t, p.AllowedNamespaces())
	require.Empty(t, p.AllowedNamespaces())

	empty := &Principal{}
	require.Equal(t, models.Role(""), empty.ClusterRole())
	require.NotNil(t, empty.AllowedNamespaces())
	require.Empty(t, empty.AllowedNamespaces())
}

func mustTestRoleMapper(t *testing.T, mapping string) *RoleMapper {
	t.Helper()
	m, err := NewRoleMapper(mapping, "")
	require.NoError(t, err)
	return m
}

func emailOnlyAdminSource(t *testing.T) *policy.Static {
	t.Helper()
	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {users: [admin@example.com]}
    role: admin
    namespaces: ["*"]
`))
	require.NoError(t, err)
	return &policy.Static{Policy: p}
}

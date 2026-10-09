package policy_test

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

const header = "apiVersion: v1\nkind: AccessPolicy\n"

func TestParsePolicy(t *testing.T) {
	t.Parallel()
	p, err := policy.Parse([]byte(header + `namespaces:
  marketing:
    kubernetes:
      namespace: caesium-marketing
      serviceAccountName: jobs.team
    secrets:
      allow:
        - vault/secret/data/marketing/*
        - k8s/marketing-*
        - env/MARKETING_*
    quotas:
      maxConcurrentRuns: 8
bindings:
  - subjects:
      groups: ["CN=Caesium Admins,OU=Groups,DC=example,DC=com"]
    role: admin
    namespaces: ["*"]
  - subjects:
      groups: ["marketing-eng"]
      users: ["alice@example.com"]
    role: operator
    namespaces: ["marketing"]
`))
	require.NoError(t, err)
	require.Equal(t, policy.Namespace{}, p.Namespaces[policy.DefaultNamespace])
	marketing := p.Namespaces["marketing"]
	require.Equal(t, "caesium-marketing", marketing.Kubernetes.Namespace)
	require.Equal(t, "jobs.team", marketing.Kubernetes.ServiceAccountName)
	require.Equal(t, 8, marketing.Quotas.MaxConcurrentRuns)
	require.Equal(t, []string{"vault/secret/data/marketing/*", "k8s/marketing-*", "env/MARKETING_*"}, marketing.Secrets.Allow)
	require.Equal(t, models.RoleAdmin, p.Bindings[0].Role)
	require.Equal(t, []string{"CN=Caesium Admins,OU=Groups,DC=example,DC=com"}, p.Bindings[0].Subjects.Groups)
	require.Empty(t, p.Warnings())
}

func TestParseValidPolicies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"implicit default only", ""},
		{"explicit default settings", "namespaces: {default: {quotas: {maxConcurrentRuns: 1}}}\n"},
		{"empty collections", "namespaces: {}\nbindings: []\n"},
		{"default binding without declaration", "bindings: [{subjects: {users: [alice@example.com]}, role: viewer, namespaces: [default]}]\n"},
		{"one character namespace", "namespaces: {a: {}}\n"},
		{"digit namespace", "namespaces: {'0': {}}\n"},
		{"maximum namespace length", "namespaces: {" + strings.Repeat("a", 63) + ": {}}\n"},
		{"optional kubernetes defaults", "namespaces: {team: {kubernetes: {}, quotas: {maxConcurrentRuns: 0}}}\n"},
		{"64 character service account", "namespaces: {team: {kubernetes: {serviceAccountName: " + strings.Repeat("a", 64) + "}}}\n"},
		{"maximum service account length", "namespaces: {team: {kubernetes: {serviceAccountName: " + strings.Repeat("a", 253) + "}}}\n"},
		{"deny all secret rules", "namespaces: {team: {secrets: {allow: []}}}\n"},
		{"empty secret settings deny all", "namespaces: {team: {secrets: {}}}\n"},
		{"ordinary YAML alias", "namespaces: {default: &settings {}, team: *settings}\n"},
		{"literal group pattern", "bindings: [{subjects: {groups: [team-*]}, role: runner, namespaces: ['*']}]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := policy.Parse([]byte(header + tc.body))
			require.NoError(t, err)
			require.Contains(t, p.Namespaces, policy.DefaultNamespace)
			require.NoError(t, p.Validate())
		})
	}
}

func TestParseRejectsInvalidPolicies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"empty document", "", "parse access policy"},
		{"null document", "null", "YAML mapping"},
		{"sequence root", "[]", "YAML mapping"},
		{"malformed YAML", header + "namespaces: [", "parse access policy"},
		{"missing apiVersion", "kind: AccessPolicy", "apiVersion"},
		{"unsupported apiVersion", "apiVersion: v2\nkind: AccessPolicy", "apiVersion"},
		{"missing kind", "apiVersion: v1", "kind"},
		{"unsupported kind", "apiVersion: v1\nkind: Job", "kind"},
		{"wildcard namespace key", header + "namespaces: {'*': {}}", "DNS label"},
		{"uppercase namespace", header + "namespaces: {Marketing: {}}", "DNS label"},
		{"underscore namespace", header + "namespaces: {my_team: {}}", "DNS label"},
		{"leading hyphen namespace", header + "namespaces: {'-team': {}}", "DNS label"},
		{"trailing hyphen namespace", header + "namespaces: {'team-': {}}", "DNS label"},
		{"empty namespace", header + "namespaces: {'': {}}", "DNS label"},
		{"long namespace", header + "namespaces: {" + strings.Repeat("a", 64) + ": {}}", "DNS label"},
		{"undeclared binding namespace", header + "bindings: [{subjects: {groups: [eng]}, role: viewer, namespaces: [team]}]", "undeclared namespace"},
		{"empty binding namespaces", header + "bindings: [{subjects: {groups: [eng]}, role: viewer, namespaces: []}]", "at least one namespace"},
		{"empty binding namespace entry", header + "bindings: [{subjects: {groups: [eng]}, role: viewer, namespaces: ['']}]", "undeclared namespace"},
		{"invalid role", header + "bindings: [{subjects: {groups: [eng]}, role: owner, namespaces: [default]}]", "invalid role"},
		{"missing role", header + "bindings: [{subjects: {groups: [eng]}, namespaces: [default]}]", "invalid role"},
		{"missing subjects", header + "bindings: [{role: viewer, namespaces: [default]}]", "at least one group or user"},
		{"empty subjects", header + "bindings: [{subjects: {groups: [], users: []}, role: viewer, namespaces: [default]}]", "at least one group or user"},
		{"empty group entry", header + "bindings: [{subjects: {groups: ['']}, role: viewer, namespaces: [default]}]", "must not be empty"},
		{"blank user entry", header + "bindings: [{subjects: {users: ['  ']}, role: viewer, namespaces: [default]}]", "must not be empty"},
		{"negative quota", header + "namespaces: {default: {quotas: {maxConcurrentRuns: -1}}}", "must not be negative"},
		{"invalid kubernetes namespace", header + "namespaces: {default: {kubernetes: {namespace: team.production}}}", "DNS label"},
		{"invalid service account", header + "namespaces: {default: {kubernetes: {serviceAccountName: team_foo}}}", "DNS subdomain"},
		{"long service account", header + "namespaces: {default: {kubernetes: {serviceAccountName: " + strings.Repeat("a", 254) + "}}}", "DNS subdomain"},
		{"empty service account label", header + "namespaces: {default: {kubernetes: {serviceAccountName: team..foo}}}", "DNS subdomain"},
		{"invalid secret glob", header + "namespaces: {default: {secrets: {allow: ['env/[']}}}", "invalid secret glob"},
		{"unknown root field", header + "binding: []", "unknown field"},
		{"unknown namespace field", header + "namespaces: {default: {secret: {}}}", "unknown field"},
		{"unknown kubernetes field", header + "namespaces: {default: {kubernetes: {serviceAccount: jobs}}}", "unknown field"},
		{"unknown secret field", header + "namespaces: {default: {secrets: {deny: []}}}", "unknown field"},
		{"unknown quota field", header + "namespaces: {default: {quotas: {maxRuns: 1}}}", "unknown field"},
		{"unknown binding field", header + "bindings: [{subject: {groups: [eng]}, role: viewer, namespaces: [default]}]", "unknown field"},
		{"unknown subject field", header + "bindings: [{subjects: {group: [eng]}, role: viewer, namespaces: [default]}]", "unknown field"},
		{"duplicate root key", header + "kind: AccessPolicy", "duplicate key"},
		{"duplicate namespace key", header + "namespaces: {default: {}, default: {}}", "duplicate key"},
		{"duplicate nested key", header + "namespaces: {default: {secrets: {allow: [], allow: []}}}", "duplicate key"},
		{"YAML merge override", header + "namespaces: {default: &ns {}, team: {<<: *ns}}", "YAML merge keys"},
		{"cyclic YAML alias", header + "namespaces: &ns {default: *ns}", "cyclic YAML alias"},
		{"numeric namespace key", header + "namespaces: {123: {}}", "mapping keys must be strings"},
		{"null namespace map", header + "namespaces: null", "expected a mapping"},
		{"null namespace settings", header + "namespaces: {default: null}", "expected a mapping"},
		{"null secrets", header + "namespaces: {default: {secrets: null}}", "expected a mapping"},
		{"null allow list", header + "namespaces: {default: {secrets: {allow: null}}}", "expected a sequence"},
		{"null bindings", header + "bindings: null", "expected a sequence"},
		{"scalar subject groups", header + "bindings: [{subjects: {groups: eng}, role: viewer, namespaces: [default]}]", "expected a sequence"},
		{"numeric subject group", header + "bindings: [{subjects: {groups: [123]}, role: viewer, namespaces: [default]}]", "expected a string"},
		{"boolean secret glob", header + "namespaces: {default: {secrets: {allow: [true]}}}", "expected a string"},
		{"string quota", header + "namespaces: {default: {quotas: {maxConcurrentRuns: '1'}}}", "expected an integer"},
		{"float quota", header + "namespaces: {default: {quotas: {maxConcurrentRuns: 1.5}}}", "expected an integer"},
		{"overflow quota", header + "namespaces: {default: {quotas: {maxConcurrentRuns: 999999999999999999999999999}}}", "expected an integer"},
		{"extra document", header + "---\n" + header, "single YAML document"},
		{"extra empty document", header + "---\n", "single YAML document"},
		{"malformed trailing document", header + "---\n[", "parse access policy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, err := policy.Parse([]byte(tc.yaml))
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, p)
		})
	}
}

func TestWarningsAndSecretRulePresence(t *testing.T) {
	t.Parallel()
	p, err := policy.Parse([]byte(header + `namespaces:
  default: {}
  finance: {}
  marketing: {}
  restricted: {secrets: {allow: []}}
  denied: {secrets: {}}
`))
	require.NoError(t, err)
	require.Nil(t, p.Namespaces["finance"].Secrets)
	require.NotNil(t, p.Namespaces["restricted"].Secrets)
	require.Empty(t, p.Namespaces["restricted"].Secrets.Allow)
	require.NotNil(t, p.Namespaces["denied"].Secrets)
	require.Equal(t, []string{
		`namespace "finance" has no secret rules; all secrets are allowed`,
		`namespace "marketing" has no secret rules; all secrets are allowed`,
	}, p.Warnings())
}

func TestValidateDoesNotMutatePolicy(t *testing.T) {
	t.Parallel()
	p := &policy.AccessPolicy{
		APIVersion: policy.APIVersionV1,
		Kind:       policy.KindAccessPolicy,
		Bindings: []policy.Binding{{
			Subjects: policy.Subjects{Users: []string{"alice@example.com"}},
			Role:     models.RoleViewer, Namespaces: []string{policy.DefaultNamespace},
		}},
	}
	require.NoError(t, p.Validate())
	require.Nil(t, p.Namespaces)
	var missing *policy.AccessPolicy
	require.ErrorContains(t, missing.Validate(), "required")
	require.Empty(t, missing.Warnings())
}

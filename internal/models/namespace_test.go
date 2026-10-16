package models_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func TestNormalizeObjectNamespace(t *testing.T) {
	t.Parallel()

	accepted := map[string]string{
		"":          models.DefaultNamespace,
		"  ":        models.DefaultNamespace,
		"default":   models.DefaultNamespace,
		"marketing": "marketing",
		"*":         models.ClusterNamespace,
		" * ":       models.ClusterNamespace,
		" finance ": "finance",
		"a":         "a",
		"team-1":    "team-1",
	}
	for input, want := range accepted {
		got, err := models.NormalizeObjectNamespace(input)
		require.NoError(t, err, "input %q", input)
		require.Equal(t, want, got, "input %q", input)
	}

	rejected := []string{"Marketing", "a_b", "-a", "a-", strings.Repeat("a", 64), "**", "*a", "ns/x", "default*", "a.b"}
	for _, input := range rejected {
		_, err := models.NormalizeObjectNamespace(input)
		require.Error(t, err, "input %q", input)
		require.NotContains(t, err.Error(), "metadata.namespace", "object errors must not point at the job manifest")
		require.False(t, errors.Is(err, models.ErrNamespaceImmutable))
	}
}

func TestObjectVisibleFrom(t *testing.T) {
	t.Parallel()

	cases := []struct {
		owner, object string
		want          bool
	}{
		{"marketing", "marketing", true},
		{"marketing", models.ClusterNamespace, true},
		{models.ClusterNamespace, models.ClusterNamespace, true},
		{"", "default", true},
		{"default", "", true},
		{"", "", true},
		{"marketing", "finance", false},
		{"marketing", "default", false},
		{"", "finance", false},
		// A shared owner (e.g. a "*" policy) may reference only shared objects.
		{models.ClusterNamespace, "marketing", false},
		{models.ClusterNamespace, "", false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, models.ObjectVisibleFrom(tc.owner, tc.object), "owner %q object %q", tc.owner, tc.object)
	}
}

func TestCheckNamespaceUnchanged(t *testing.T) {
	t.Parallel()

	ptr := func(s string) *string { return &s }

	require.NoError(t, models.CheckNamespaceUnchanged("marketing", nil), "omitted")
	require.NoError(t, models.CheckNamespaceUnchanged("marketing", ptr("")), "blank means no change")
	require.NoError(t, models.CheckNamespaceUnchanged("marketing", ptr("  ")), "blank means no change")
	require.NoError(t, models.CheckNamespaceUnchanged("marketing", ptr("marketing")), "echo")
	require.NoError(t, models.CheckNamespaceUnchanged("marketing", ptr(" marketing ")), "echo is normalised")
	require.NoError(t, models.CheckNamespaceUnchanged(models.ClusterNamespace, ptr("*")), "shared echo")
	require.NoError(t, models.CheckNamespaceUnchanged("", ptr("default")), "legacy empty stored value is default")

	err := models.CheckNamespaceUnchanged("marketing", ptr("finance"))
	require.ErrorIs(t, err, models.ErrNamespaceImmutable)
	require.Contains(t, err.Error(), `"marketing"`)

	require.ErrorIs(t, models.CheckNamespaceUnchanged("marketing", ptr("*")), models.ErrNamespaceImmutable, "moving to shared is a move")
	require.ErrorIs(t, models.CheckNamespaceUnchanged(models.ClusterNamespace, ptr("default")), models.ErrNamespaceImmutable, "moving out of shared is a move")
	require.ErrorIs(t, models.CheckNamespaceUnchanged("", ptr("finance")), models.ErrNamespaceImmutable)

	err = models.CheckNamespaceUnchanged("marketing", ptr("Bad_NS"))
	require.Error(t, err)
	require.False(t, errors.Is(err, models.ErrNamespaceImmutable), "a syntax error is not a move")
}

// TestObjectNamespaceSyntaxMatchesJobdef pins the models-local DNS-label copy
// to the job-manifest validator: a shared object and a job must agree on what
// a namespace is ("*" aside, which is never a job namespace).
func TestObjectNamespaceSyntaxMatchesJobdef(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"default", "marketing", "a", "0", "a-b", "a1-2b", strings.Repeat("a", 63), strings.Repeat("a", 64),
		"Marketing", "a_b", "-a", "a-", "a.b", "ns/x", "default*", "**", "*a", "a b", "é",
	}
	for _, input := range inputs {
		_, objectErr := models.NormalizeObjectNamespace(input)
		jobErr := schema.ValidateNamespace(input)
		require.Equal(t, jobErr == nil, objectErr == nil, "input %q: job err %v, object err %v", input, jobErr, objectErr)
	}
}

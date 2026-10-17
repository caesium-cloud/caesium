package jobdef

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

const declaredPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing: {}
  finance: {}
`

func fileHolder(t *testing.T, contents string) (*policy.Holder, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	return holder, path
}

func namespacedDefs(pairs ...string) []schema.Definition {
	defs := make([]schema.Definition, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		defs = append(defs, schema.Definition{Metadata: schema.Metadata{Alias: pairs[i], Namespace: pairs[i+1]}})
	}
	return defs
}

func TestUndeclaredNamespaceMessagesAgainstAFileBackedPolicy(t *testing.T) {
	holder, path := fileHolder(t, declaredPolicy)

	require.Empty(t, undeclaredNamespaceMessages(holder, namespacedDefs("a", "marketing", "b", "finance", "c", "", "d", "default")),
		"declared namespaces, blank and the always-declared default pass")
	require.Equal(t, []string{
		`metadata.namespace "sales" of job "b" is not declared in the access policy`,
		`metadata.namespace "typo" of job "a" is not declared in the access policy`,
	}, undeclaredNamespaceMessages(holder, namespacedDefs("a", "typo", "m", "marketing", "b", "sales")),
		"every offender is named with its job, sorted")

	// A reload that drops a namespace takes effect on the next check.
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n"), 0o600))
	_, err := holder.Reload(t.Context(), policy.ReloadRequest{Actor: "test"})
	require.NoError(t, err)
	require.Equal(t, []string{`metadata.namespace "finance" of job "f" is not declared in the access policy`},
		undeclaredNamespaceMessages(holder, namespacedDefs("f", "finance", "m", "marketing")))
}

// TestDeclaredNamespacesNeedAPolicyFile: with no policy file (no holder, or
// the env translation, even one that names namespaces) every syntactically
// valid namespace is accepted, as B1 shipped namespaces.
func TestDeclaredNamespacesNeedAPolicyFile(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	defs := namespacedDefs("a", "anything-goes", "b", "marketing")

	policy.SetProcessHolder(nil)
	require.NoError(t, ValidateDeclaredNamespaces(defs))
	require.Nil(t, UndeclaredNamespaceMessages(defs))
	require.NoError(t, ValidateDeclaredNamespace("a", "anything-goes"))

	view := policy.EmptyPolicy()
	view.Namespaces["marketing"] = policy.Namespace{}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	require.NoError(t, ValidateDeclaredNamespaces(defs))
	require.Nil(t, UndeclaredNamespaceMessages(defs))
}

// TestValidateBatchRejectsUndeclaredNamespace drives the process-wide holder
// through ValidateBatch (REST apply, git sync and the incident patch all call
// it) and the legacy create check.
func TestValidateBatchRejectsUndeclaredNamespace(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	holder, _ := fileHolder(t, declaredPolicy)
	policy.SetProcessHolder(holder)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	importer := NewImporter(db)

	def, err := schema.Parse([]byte(testutil.SampleJob))
	require.NoError(t, err)
	def.Metadata.Namespace = "marketing"
	require.NoError(t, importer.ValidateBatch(t.Context(), []schema.Definition{*def}))

	def.Metadata.Namespace = "sales"
	err = importer.ValidateBatch(t.Context(), []schema.Definition{*def})
	require.ErrorIs(t, err, ErrNamespaceNotDeclared)
	require.EqualError(t, err, `metadata.namespace "sales" of job "`+def.Metadata.Alias+`" is not declared in the access policy`)

	require.NoError(t, ValidateDeclaredNamespace("legacy", ""), "blank is the default namespace")
	require.ErrorIs(t, ValidateDeclaredNamespace("legacy", "sales"), ErrNamespaceNotDeclared)

	var undeclared *UndeclaredNamespaceError
	require.True(t, errors.As(ValidateDeclaredNamespaces(namespacedDefs("x", "one", "y", "two")), &undeclared))
	require.Len(t, undeclared.Messages, 2)
}

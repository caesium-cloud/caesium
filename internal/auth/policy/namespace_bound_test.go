package policy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func aliasedRulesPolicy(ruleLen, refs int) string {
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing:\n    secrets:\n      allow:\n")
	fmt.Fprintf(&b, "        - &rule env/%s*\n", strings.Repeat("A", ruleLen))
	for range refs {
		b.WriteString("        - *rule\n")
	}
	return b.String()
}

// TestNamespaceSettingsAreBoundedAfterAliasExpansion: a small file whose YAML
// aliases expand one namespace's settings past what an owner can carry to a
// worker (identity-and-access §10.1) is invalid, so every policy that loads is
// transportable; an aliased policy under the bound still loads.
func TestNamespaceSettingsAreBoundedAfterAliasExpansion(t *testing.T) {
	bomb := aliasedRulesPolicy(100_000, 69)
	require.Less(t, len(bomb), MaxFileBytes, "the file itself is small")
	_, err := Parse([]byte(bomb))
	require.ErrorContains(t, err, "once expanded")

	ok := aliasedRulesPolicy(1_000, 10)
	parsed, err := Parse([]byte(ok))
	require.NoError(t, err)
	encoded, err := json.Marshal(parsed.Namespaces["marketing"])
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), MaxEncodedNamespaceBytes)
}

package jobdef

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func aliasDefinition(alias string) *Definition {
	var def Definition
	manifest := "apiVersion: v1\nkind: Job\nmetadata:\n  alias: placeholder\ntrigger:\n  type: cron\n  configuration:\n    cron: \"* * * * *\"\nsteps:\n  - name: a\n    image: busybox:1.36.1\n"
	if err := yaml.Unmarshal([]byte(manifest), &def); err != nil {
		panic(err)
	}
	def.Metadata.Alias = alias
	return &def
}

// A non-canonical alias is refused for every writer, naming the alias quoted
// so the whitespace is visible; case is significant and accepted.
func TestValidateRejectsNonCanonicalAlias(t *testing.T) {
	for _, alias := range []string{" payroll", "payroll ", "payroll ", "\tpayroll", "payroll\n", " payroll"} {
		err := aliasDefinition(alias).Validate()
		require.Error(t, err, "%q", alias)
		require.Contains(t, err.Error(), fmt.Sprintf("metadata.alias %q must not have leading or trailing whitespace", alias))
		require.Error(t, ValidateAlias(alias))
	}
	for _, alias := range []string{"payroll", "Payroll", "pay roll", "payroll-v2"} {
		require.NoError(t, aliasDefinition(alias).Validate(), "%q", alias)
		require.NoError(t, ValidateAlias(alias))
	}
	require.EqualError(t, aliasDefinition("   ").Validate(), "metadata.alias is required")
	require.NoError(t, ValidateAlias(""), "an empty alias is the caller's own check")
}

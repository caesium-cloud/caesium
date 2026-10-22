package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

func lookupFlag(c *cobra.Command, name string) *pflag.Flag {
	if f := c.Flags().Lookup(name); f != nil {
		return f
	}
	if f := c.PersistentFlags().Lookup(name); f != nil {
		return f
	}
	return c.InheritedFlags().Lookup(name)
}

// TestEveryAPIKeyCommandHasAServerFlag pins the coupling the credential
// resolver relies on (cmd/cliutil.ResolveAPIKey): the stored `caesium login`
// credential is looked up by the command's own --server flag, so a command
// that takes --api-key but has no --server would silently never use it.
func TestEveryAPIKeyCommandHasAServerFlag(t *testing.T) {
	var walk func(c *cobra.Command)
	checked := 0
	walk = func(c *cobra.Command) {
		if c.Runnable() && lookupFlag(c, "api-key") != nil {
			checked++
			require.NotNil(t, lookupFlag(c, "server"), "%q takes --api-key but has no --server", c.CommandPath())
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	for _, c := range cmds {
		walk(c)
	}
	require.Greater(t, checked, 20, "the walk found the credential-taking commands")
}

// TestServerCommandsResolveCredentials pins that every runnable command
// talking to a server (one with --server) accepts --api-key, so it goes
// through the shared resolver (flag, CAESIUM_API_KEY, stored login). The
// login verbs manage the stored credential themselves.
func TestServerCommandsResolveCredentials(t *testing.T) {
	exempt := map[string]bool{
		"caesium login":  true, // obtains the credential
		"caesium logout": true, // acts only on the stored credential
	}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() && lookupFlag(c, "server") != nil && !exempt[c.CommandPath()] {
			require.NotNil(t, lookupFlag(c, "api-key"), "%q talks to a server but takes no --api-key", c.CommandPath())
		}
		for _, child := range c.Commands() {
			walk(child)
		}
	}
	root := &cobra.Command{Use: "caesium"}
	for _, c := range cmds {
		root.AddCommand(c)
	}
	walk(root)
}

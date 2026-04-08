package auth

import "github.com/spf13/cobra"

// Cmd is the parent command for authentication operations.
var Cmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage API authentication and authorization",
}

// key is the parent command for API key operations.
var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "Manage API keys",
}

func init() {
	Cmd.AddCommand(keyCmd)
}

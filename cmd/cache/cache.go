package cache

import "github.com/spf13/cobra"

// Cmd is the parent command for cache operations.
var Cmd = &cobra.Command{
	Use:   "cache",
	Short: "Manage task cache",
}

package cache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var pruneServer string

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Args:  cobra.NoArgs,
	Short: "Prune expired cache entries",
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(pruneServer, "/")
		url := fmt.Sprintf("%s/v1/cache/prune", server)

		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodPost, url, nil, make(http.Header))
		if err := clihttp.ResponseError("cache prune", status, body, readErr); err != nil {
			return err
		}

		var result struct {
			Pruned int `json:"pruned"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			cmd.Print(string(body))
			return nil
		}
		cmd.Printf("Pruned %d expired cache entries\n", result.Pruned)
		return nil
	},
}

func init() {
	pruneCmd.Flags().StringVar(&pruneServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(pruneCmd)
}

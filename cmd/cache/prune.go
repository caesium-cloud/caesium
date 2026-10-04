package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

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

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, nil)
		if err != nil {
			return err
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, readErr := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			if readErr != nil {
				return fmt.Errorf("cache prune failed (%d): %s (reading response: %w)", resp.StatusCode, strings.TrimSpace(string(body)), readErr)
			}
			return fmt.Errorf("cache prune failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		if readErr != nil {
			return fmt.Errorf("reading cache prune response: %w", readErr)
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

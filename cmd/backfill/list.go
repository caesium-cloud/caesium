package backfill

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

var (
	listJobID  string
	listServer string
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List backfills for a job",
	RunE: func(cmd *cobra.Command, args []string) error {
		if listJobID == "" {
			return fmt.Errorf("--job-id is required")
		}

		server := strings.TrimSuffix(listServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/backfills", server, listJobID)

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, url, nil)
		if err != nil {
			return err
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("backfill list failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		// Pretty-print the JSON response.
		var out interface{}
		if err := json.Unmarshal(body, &out); err != nil {
			cmd.Print(string(body))
			return nil
		}
		pretty, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			cmd.Print(string(body))
			return nil
		}
		cmd.Println(string(pretty))
		return nil
	},
}

func init() {
	listCmd.Flags().StringVar(&listJobID, "job-id", "", "Job ID to list backfills for (required)")
	listCmd.Flags().StringVar(&listServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(listCmd)
}

package cache

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
	Short: "List cache entries for a job",
	RunE: func(cmd *cobra.Command, args []string) error {
		if listJobID == "" {
			return fmt.Errorf("--job-id is required")
		}

		server := strings.TrimSuffix(listServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/cache", server, listJobID)

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
			return fmt.Errorf("cache list failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		// NOTE: write machine-readable output via cmd.OutOrStdout(), NOT
		// cmd.Print/Println — cobra's Print* helpers route to stderr (the root
		// command sets no output writer), which left this command's JSON
		// unpipeable and unassertable. Same fix as `caesium why`.
		stdout := cmd.OutOrStdout()
		var out interface{}
		if err := json.Unmarshal(body, &out); err != nil {
			_, _ = stdout.Write(body)
			return nil
		}
		pretty, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			_, _ = stdout.Write(body)
			return nil
		}
		_, _ = fmt.Fprintln(stdout, string(pretty))
		return nil
	},
}

func init() {
	listCmd.Flags().StringVar(&listJobID, "job-id", "", "Job ID to list cache entries for (required)")
	listCmd.Flags().StringVar(&listServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(listCmd)
}

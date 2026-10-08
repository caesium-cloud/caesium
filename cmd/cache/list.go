package cache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var (
	listJobID  string
	listServer string
)

var listCmd = &cobra.Command{
	Use:   "list",
	Args:  cobra.NoArgs,
	Short: "List cache entries for a job",
	RunE: func(cmd *cobra.Command, args []string) error {
		if listJobID == "" {
			return fmt.Errorf("--job-id is required")
		}

		server := strings.TrimSuffix(listServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/cache", server, listJobID)

		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodGet, url, nil, make(http.Header))
		if err := clihttp.ResponseError("cache list", status, body, readErr); err != nil {
			return err
		}

		// NOTE: write machine-readable output via cmd.OutOrStdout(), NOT
		// cmd.Print/Println — cobra's Print* helpers route to stderr (the root
		// command sets no output writer), which left this command's JSON
		// unpipeable and unassertable. Same fix as `caesium why`.
		stdout := cmd.OutOrStdout()
		var out any
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

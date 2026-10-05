package backfill

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
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
	Short: "List backfills for a job",
	RunE: func(cmd *cobra.Command, args []string) error {
		if listJobID == "" {
			return fmt.Errorf("--job-id is required")
		}

		server := strings.TrimSuffix(listServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/backfills", server, listJobID)

		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodGet, url, nil, make(http.Header))
		if err := clihttp.ResponseError("backfill list", status, body, readErr); err != nil {
			return err
		}

		// stdout is the listing and nothing else, so `backfill list | jq` works.
		// It used cobra's Println (→ OutOrStderr), which made the listing
		// unpipeable; the old raw-body fallback is gone too, because emitting a
		// non-JSON body onto stdout is the same defect in a different disguise.
		return cliutil.WritePrettyJSON(cmd, body, "backfill list response")
	},
}

func init() {
	listCmd.Flags().StringVar(&listJobID, "job-id", "", "Job ID to list backfills for (required)")
	listCmd.Flags().StringVar(&listServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(listCmd)
}

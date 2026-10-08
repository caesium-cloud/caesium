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
	cancelJobID      string
	cancelBackfillID string
	cancelServer     string
)

var cancelCmd = &cobra.Command{
	Use:   "cancel",
	Args:  cobra.NoArgs,
	Short: "Cancel a running backfill",
	RunE: func(cmd *cobra.Command, args []string) error {
		if cancelJobID == "" {
			return fmt.Errorf("--job-id is required")
		}
		if cancelBackfillID == "" {
			return fmt.Errorf("--backfill-id is required")
		}

		server := strings.TrimSuffix(cancelServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/backfills/%s/cancel", server, cancelJobID, cancelBackfillID)

		body, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodPut, url, nil, make(http.Header))
		if err := clihttp.ResponseError("backfill cancel", status, body, readErr); err != nil {
			return err
		}

		// The endpoint answers with the updated backfill record
		// (api/rest/controller/backfill/backfill.go Cancel), so stdout carries
		// that record and nothing else — same contract as create and list. The
		// human confirmation goes to stderr.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Backfill %s cancelled\n", cancelBackfillID)
		return cliutil.WritePrettyJSON(cmd, body, "backfill cancel response")
	},
}

func init() {
	cancelCmd.Flags().StringVar(&cancelJobID, "job-id", "", "Job ID owning the backfill (required)")
	cancelCmd.Flags().StringVar(&cancelBackfillID, "backfill-id", "", "Backfill ID to cancel (required)")
	cancelCmd.Flags().StringVar(&cancelServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(cancelCmd)
}

package backfill

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

var (
	cancelJobID      string
	cancelBackfillID string
	cancelServer     string
)

var cancelCmd = &cobra.Command{
	Use:   "cancel",
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

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPut, url, nil)
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
			return fmt.Errorf("backfill cancel failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		cmd.Printf("Backfill %s cancelled\n", cancelBackfillID)
		return nil
	},
}

func init() {
	cancelCmd.Flags().StringVar(&cancelJobID, "job-id", "", "Job ID owning the backfill (required)")
	cancelCmd.Flags().StringVar(&cancelBackfillID, "backfill-id", "", "Backfill ID to cancel (required)")
	cancelCmd.Flags().StringVar(&cancelServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(cancelCmd)
}

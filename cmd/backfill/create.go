package backfill

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

var (
	createJobID     string
	createStart     string
	createEnd       string
	createMaxConc   int
	createReprocess string
	createServer    string
)

var createCmd = &cobra.Command{
	Use:   "create",
	Args:  cobra.NoArgs,
	Short: "Start a backfill for a job",
	Long: `Start a backfill that queues runs for each cron fire time in [start, end).

Reprocess policies:
  none    Skip dates that already have any run (default)
  failed  Skip dates whose latest run succeeded
  all     Queue a new run for every date regardless of existing runs`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if createJobID == "" {
			return fmt.Errorf("--job-id is required")
		}
		if createStart == "" || createEnd == "" {
			return fmt.Errorf("--start and --end are required")
		}

		start, err := time.Parse(time.RFC3339, createStart)
		if err != nil {
			return fmt.Errorf("invalid --start (expected RFC3339, e.g. 2024-01-01T00:00:00Z): %w", err)
		}
		end, err := time.Parse(time.RFC3339, createEnd)
		if err != nil {
			return fmt.Errorf("invalid --end (expected RFC3339, e.g. 2024-02-01T00:00:00Z): %w", err)
		}

		body := map[string]any{
			"start": start,
			"end":   end,
		}
		if createMaxConc > 0 {
			body["max_concurrent"] = createMaxConc
		}
		if createReprocess != "" {
			body["reprocess"] = createReprocess
		}

		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}

		server := strings.TrimSuffix(createServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/backfill", server, createJobID)

		headers := make(http.Header)
		headers.Set("Content-Type", "application/json")
		respBody, status, readErr := clihttp.Exchange(cmd.Context(), http.DefaultClient, http.MethodPost, url, bytes.NewReader(payload), headers)
		if err := clihttp.ResponseError("backfill create", status, respBody, readErr); err != nil {
			return err
		}

		// stdout is the created record and NOTHING else, so
		// `caesium backfill create … | jq .id` works. The human framing goes to
		// stderr — same split as `caesium receipt get` (cmd/receipt/get.go),
		// where stdout stays the receipt bytes and every note is on stderr.
		// (Before this it was cobra's Printf, which writes to OutOrStderr, so
		// the record reached neither stream usefully.)
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Backfill started:")
		return cliutil.WritePrettyJSON(cmd, respBody, "backfill create response")
	},
}

func init() {
	createCmd.Flags().StringVar(&createJobID, "job-id", "", "Job ID to backfill (required)")
	createCmd.Flags().StringVar(&createStart, "start", "", "Backfill window start in RFC3339 format (required)")
	createCmd.Flags().StringVar(&createEnd, "end", "", "Backfill window end in RFC3339 format (required)")
	createCmd.Flags().IntVar(&createMaxConc, "max-concurrent", 1, "Maximum number of concurrent backfill runs")
	createCmd.Flags().StringVar(&createReprocess, "reprocess", "none", "Reprocess policy: none, failed, or all")
	createCmd.Flags().StringVar(&createServer, "server", "http://localhost:8080", "Caesium server base URL")

	Cmd.AddCommand(createCmd)
}

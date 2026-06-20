package receipt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	ireceipt "github.com/caesium-cloud/caesium/internal/receipt"
	"github.com/spf13/cobra"
)

var (
	getJobID  string
	getRunID  string
	getServer string
	getOutput string
)

var getCmd = &cobra.Command{
	Use:   "get",
	Short: "Fetch the reproducibility receipt for a run",
	Long: "Fetch the content-addressed reproducibility receipt for a run from " +
		"the server and print it (or write it to a file with --output). Commit " +
		"the receipt into git next to your pipeline so `caesium verify` can later " +
		"prove what ran.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if getJobID == "" || getRunID == "" {
			return fmt.Errorf("--job-id and --run-id are required")
		}

		server := strings.TrimSuffix(getServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/receipt", server, getJobID, getRunID)

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
			return fmt.Errorf("receipt get failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		// Pretty-print canonically so the committed file is stable and diffable.
		var r ireceipt.Receipt
		if err := json.Unmarshal(body, &r); err != nil {
			// Server returned something unexpected; surface it verbatim rather
			// than swallowing it.
			return writeOut(cmd, body)
		}
		pretty, err := json.MarshalIndent(&r, "", "  ")
		if err != nil {
			return writeOut(cmd, body)
		}
		pretty = append(pretty, '\n')

		if r.Degraded {
			// A degraded receipt is emitted (it is still the honest record) but
			// the operator must know it does not attest reproducibility.
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: receipt is DEGRADED — %d task(s) ran on unpinned image tags and are not verifiable: %s\n",
				len(r.DegradedTasks), strings.Join(r.DegradedTasks, ", "))
		}
		return writeOut(cmd, pretty)
	},
}

// writeOut writes the receipt bytes to --output (if set) or stdout.
func writeOut(cmd *cobra.Command, data []byte) error {
	if getOutput == "" {
		_, err := cmd.OutOrStdout().Write(data)
		return err
	}
	if err := os.WriteFile(getOutput, data, 0o644); err != nil {
		return fmt.Errorf("write receipt to %s: %w", getOutput, err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Wrote receipt to %s\n", getOutput)
	return nil
}

func init() {
	getCmd.Flags().StringVar(&getJobID, "job-id", "", "Job ID (required)")
	getCmd.Flags().StringVar(&getRunID, "run-id", "", "Run ID (required)")
	getCmd.Flags().StringVar(&getServer, "server", "http://localhost:8080", "Caesium server base URL")
	getCmd.Flags().StringVarP(&getOutput, "output", "o", "", "Write the receipt to this file (default: stdout)")
	Cmd.AddCommand(getCmd)
}

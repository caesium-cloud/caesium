package run

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

const cancelDownstreamNote = "Cancellation is not a task failure. Unfinished tasks are cancelled with the run, including cleanup steps a tolerant trigger rule (all_done, always, one_success, all_failed) would run after a failure. CAESIUM_TASK_FAILURE_POLICY is unchanged. Logs and outcomes already recorded are kept."

var (
	cancelRunID  string
	cancelJobID  string
	cancelServer string
	cancelAPIKey string
	cancelAsJSON bool
)

var cancelCmd = &cobra.Command{
	Use:   "cancel --run-id <run-id> --job-id <job-id>",
	Short: "Cancel a running run",
	Long: "Cancel one running run of a job through POST /v1/jobs/:id/runs/:run_id/cancel.\n\n" +
		"The caller needs operator in the job's current namespace and in the run's persisted namespace. " +
		"A run that is not running is a conflict and the command exits non-zero.\n\n" +
		cancelDownstreamNote + "\n\n" +
		"With --json, stdout is only the cancelled run document. The note above is written to stderr.",
	Args: cobra.NoArgs,
	RunE: runCancel,
}

func runCancel(cmd *cobra.Command, _ []string) error {
	if err := requireRunRef(cancelJobID, cancelRunID); err != nil {
		return err
	}
	cmd.SilenceUsage = true
	apiKey := resolveRunDiffAPIKey(cmd, cancelAPIKey)
	view, raw, err := postRunCancel(cmd, inspectHTTPClient, cancelServer, apiKey, cancelJobID, cancelRunID)
	if err != nil {
		return err
	}
	if cancelAsJSON {
		if len(raw) == 0 || raw[len(raw)-1] != '\n' {
			raw = append(raw, '\n')
		}
		_, err := cmd.OutOrStdout().Write(raw)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.ErrOrStderr(), cancelDownstreamNote)
		return err
	}
	runID, jobID := view.ID, view.JobID
	if strings.TrimSpace(runID) == "" {
		runID = strings.TrimSpace(cancelRunID)
	}
	if strings.TrimSpace(jobID) == "" {
		jobID = strings.TrimSpace(cancelJobID)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Cancelled run %s (job %s)\n", runID, jobID); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.ErrOrStderr(), cancelDownstreamNote)
	return err
}

func postRunCancel(cmd *cobra.Command, client *http.Client, server, apiKey, jobID, runID string) (*runView, []byte, error) {
	rawURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/cancel",
		serverBase(server),
		url.PathEscape(strings.TrimSpace(jobID)),
		url.PathEscape(strings.TrimSpace(runID)))
	headers := make(http.Header)
	if apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	body, status, err := clihttp.Exchange(cmd.Context(), client, http.MethodPost, rawURL, nil, headers)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, nil, statusError("run cancel", status, body)
	}
	var view runView
	if err := json.Unmarshal(body, &view); err != nil {
		return nil, nil, fmt.Errorf("run cancel response was not valid JSON: %w", err)
	}
	return &view, body, nil
}

func init() {
	cancelCmd.Flags().StringVar(&cancelRunID, "run-id", "", "Run ID to cancel (required)")
	cancelCmd.Flags().StringVar(&cancelJobID, "job-id", "", "Job ID that owns the run (required)")
	cancelCmd.Flags().StringVar(&cancelServer, "server", "http://localhost:8080", "Caesium server base URL")
	cancelCmd.Flags().StringVar(&cancelAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	cancelCmd.Flags().BoolVar(&cancelAsJSON, "json", false, "Print the cancelled run as JSON on stdout")
	Cmd.AddCommand(cancelCmd)
}

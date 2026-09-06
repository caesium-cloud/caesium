package run

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	retryJobID           string
	retryRunID           string
	retryCallbacksServer string
	retryCallbacksAPIKey string
)

var retryCallbacksCmd = &cobra.Command{
	Use:   "retry-callbacks",
	Short: "Retry failed callbacks for a job run",
	Long: "Retry failed callbacks for a completed job run. Only callbacks that previously failed will be re-run.\n\n" +
		"Passing --server explicitly routes the request through the REST API " +
		"(POST /v1/jobs/:id/runs/:run_id/callbacks/retry) instead of opening the in-process store; " +
		"leaving it at its default keeps the local-mode behaviour unchanged.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if retryJobID == "" {
			return fmt.Errorf("--job-id is required")
		}
		if retryRunID == "" {
			return fmt.Errorf("--run-id is required")
		}

		jobID, err := uuid.Parse(retryJobID)
		if err != nil {
			return fmt.Errorf("invalid job id: %w", err)
		}
		runID, err := uuid.Parse(retryRunID)
		if err != nil {
			return fmt.Errorf("invalid run id: %w", err)
		}

		ctx := cmd.Context()

		// Past argument validation: a server/store failure is not a usage
		// error, and cobra prints the whole usage block after ANY RunE error.
		// Same convention as `caesium run retry`.
		cmd.SilenceUsage = true

		// `--server` decides transport exactly the way `caesium run retry`
		// decides it (cmd/run/retry.go: cmd.Flags().Changed("server")). The
		// in-process branch below opens a NATIVE dqlite node bound to
		// CAESIUM_NODE_ADDRESS, which only works on a host that is not already
		// running one — so any caller talking to a remote (or containerised)
		// server must pass --server.
		if cmd.Flags().Changed("server") {
			return retryCallbacksOverServer(ctx, cmd, retryJobID, retryRunID)
		}

		runEntry, err := runsvc.New(ctx).Get(runID)
		if err != nil {
			return err
		}
		if runEntry.JobID != jobID {
			return fmt.Errorf("run %s does not belong to job %s", runID, jobID)
		}

		if err := callback.Default().RetryFailed(ctx, runID); err != nil {
			return err
		}

		// stdout, matching retryCallbacksOverServer — cobra's Printf writes to
		// OutOrStderr, so the two transports would otherwise disagree on which
		// stream the success line lands on.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Retried failed callbacks for run %s\n", runID)
		return nil
	},
}

// retryCallbacksOverServer retries a run's failed callbacks through the shipped
// POST /v1/jobs/:id/runs/:run_id/callbacks/retry endpoint
// (api/rest/controller/job/run/retry_callbacks.go). The success line is kept
// byte-for-byte identical to the in-process path's so scripts that scrape it do
// not need to branch on transport.
func retryCallbacksOverServer(ctx context.Context, cmd *cobra.Command, jobID, runID string) error {
	server := strings.TrimSuffix(retryCallbacksServer, "/")
	if server == "" {
		server = "http://localhost:8080"
	}

	target := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/callbacks/retry", server, jobID, runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey := resolveRunDiffAPIKey(cmd, retryCallbacksAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := retryPartitionHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading retry-callbacks response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("retry callbacks failed (%d): %s", resp.StatusCode, replayErrorMessage(body))
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Retried failed callbacks for run %s\n", runID)
	return err
}

func init() {
	retryCallbacksCmd.Flags().StringVar(&retryJobID, "job-id", "", "Job ID owning the run (required)")
	retryCallbacksCmd.Flags().StringVar(&retryRunID, "run-id", "", "Run ID to retry callbacks for (required)")
	retryCallbacksCmd.Flags().StringVar(&retryCallbacksServer, "server", "http://localhost:8080",
		"Caesium server base URL; passing --server explicitly routes the retry through the REST API instead of the in-process store")
	retryCallbacksCmd.Flags().StringVar(&retryCallbacksAPIKey, "api-key", "",
		"API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings; used only with an explicit --server)")
	retryCallbacksCmd.MarkFlagRequired("job-id") //nolint:errcheck
	retryCallbacksCmd.MarkFlagRequired("run-id") //nolint:errcheck

	Cmd.AddCommand(retryCallbacksCmd)
}

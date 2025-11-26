package run

import (
	"fmt"

	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/callback"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	retryJobID string
	retryRunID string
)

var retryCallbacksCmd = &cobra.Command{
	Use:   "retry-callbacks",
	Short: "Retry failed callbacks for a job run",
	Long:  "Retry failed callbacks for a completed job run. Only callbacks that previously failed will be re-run.",
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

		cmd.Printf("Retried failed callbacks for run %s\n", runID)
		return nil
	},
}

func init() {
	retryCallbacksCmd.Flags().StringVar(&retryJobID, "job-id", "", "Job ID owning the run (required)")
	retryCallbacksCmd.Flags().StringVar(&retryRunID, "run-id", "", "Run ID to retry callbacks for (required)")
	retryCallbacksCmd.MarkFlagRequired("job-id") //nolint:errcheck
	retryCallbacksCmd.MarkFlagRequired("run-id") //nolint:errcheck

	Cmd.AddCommand(retryCallbacksCmd)
}

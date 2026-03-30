package run

import (
	"context"
	"fmt"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/internal/job"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	retryFromFailureJobID string
	retryFromFailureRunID string
)

var retryCmd = &cobra.Command{
	Use:   "retry",
	Short: "Retry a failed run, preserving succeeded/cached tasks",
	Long:  "Retry a failed or completed run. Tasks that previously succeeded or were served from cache are preserved; only failed, skipped, and pending tasks are re-executed.",
	RunE: func(cmd *cobra.Command, args []string) error {
		if retryFromFailureJobID == "" {
			return fmt.Errorf("--job-id is required")
		}
		if retryFromFailureRunID == "" {
			return fmt.Errorf("--run-id is required")
		}

		jobID, err := uuid.Parse(retryFromFailureJobID)
		if err != nil {
			return fmt.Errorf("invalid job id: %w", err)
		}
		runID, err := uuid.Parse(retryFromFailureRunID)
		if err != nil {
			return fmt.Errorf("invalid run id: %w", err)
		}

		ctx := cmd.Context()

		j, err := jsvc.Service(ctx).Get(jobID)
		if err != nil {
			return fmt.Errorf("failed to get job: %w", err)
		}

		runEntry, err := runsvc.New(ctx).Get(runID)
		if err != nil {
			return fmt.Errorf("failed to get run: %w", err)
		}
		if runEntry.JobID != jobID {
			return fmt.Errorf("run %s does not belong to job %s", runID, jobID)
		}

		store := runstorage.Default()
		r, err := store.RetryFromFailure(runID)
		if err != nil {
			return fmt.Errorf("failed to retry run: %w", err)
		}

		go func() {
			runCtx := runstorage.WithContext(context.Background(), r.ID)
			if err := job.New(j, job.WithTriggerID(nil), job.WithParams(r.Params)).Run(runCtx); err != nil {
				log.Error("job retry run failure", "id", j.ID, "run_id", r.ID, "error", err)
			}
		}()

		cmd.Printf("Retrying run %s (job %s)\n", runID, jobID)
		return nil
	},
}

func init() {
	retryCmd.Flags().StringVar(&retryFromFailureJobID, "job-id", "", "Job ID owning the run (required)")
	retryCmd.Flags().StringVar(&retryFromFailureRunID, "run-id", "", "Run ID to retry (required)")
	retryCmd.MarkFlagRequired("job-id") //nolint:errcheck
	retryCmd.MarkFlagRequired("run-id") //nolint:errcheck

	Cmd.AddCommand(retryCmd)
}

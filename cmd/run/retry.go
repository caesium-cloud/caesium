package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	runsvc "github.com/caesium-cloud/caesium/api/rest/service/run"
	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/job"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	retryFromFailureJobID     string
	retryFromFailureRunID     string
	retryFromFailurePartition string
	retryFromFailureTask      string
	retryFromFailureServer    string
	retryFromFailureAPIKey    string

	retryPartitionHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
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

		if retryFromFailurePartition != "" {
			if retryFromFailureTask == "" {
				return fmt.Errorf("--task is required with --partition")
			}
			return retrySinglePartition(cmd.Context(), cmd, retryFromFailureJobID, retryFromFailureRunID, retryFromFailureTask, retryFromFailurePartition)
		}

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
	retryCmd.Flags().StringVar(&retryFromFailurePartition, "partition", "", "Retry a single fan-out instance by partition value (does not re-run succeeded siblings)")
	retryCmd.Flags().StringVar(&retryFromFailureTask, "task", "", "Task name or ID (required with --partition)")
	retryCmd.Flags().StringVar(&retryFromFailureServer, "server", "http://localhost:8080", "Caesium server base URL (used only with --partition)")
	retryCmd.Flags().StringVar(&retryFromFailureAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings; used only with --partition)")
	retryCmd.MarkFlagRequired("job-id") //nolint:errcheck
	retryCmd.MarkFlagRequired("run-id") //nolint:errcheck

	Cmd.AddCommand(retryCmd)
}

func retrySinglePartition(ctx context.Context, cmd *cobra.Command, jobID, runID, task, value string) error {
	server := strings.TrimSuffix(retryFromFailureServer, "/")
	if server == "" {
		server = "http://localhost:8080"
	}
	listURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions", server, jobID, runID, task)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return err
	}
	if apiKey := resolveRunDiffAPIKey(cmd, retryFromFailureAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := retryPartitionHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("partitions: %s: %s", resp.Status, body)
	}
	var parsed struct {
		Partitions []struct {
			Value  string `json:"value"`
			Index  int    `json:"index"`
			Status string `json:"status"`
		} `json:"partitions"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return err
	}
	index := -1
	for _, p := range parsed.Partitions {
		if p.Value == value {
			index = p.Index
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("partition %q not found", value)
	}
	retryURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions/%d/retry", server, jobID, runID, task, index)
	postReq, err := http.NewRequestWithContext(ctx, http.MethodPost, retryURL, nil)
	if err != nil {
		return err
	}
	postReq.Header.Set("Content-Type", "application/json")
	if apiKey := resolveRunDiffAPIKey(cmd, retryFromFailureAPIKey); apiKey != "" {
		postReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	post, err := retryPartitionHTTPClient.Do(postReq)
	if err != nil {
		return err
	}
	defer func() { _ = post.Body.Close() }()
	postBody, err := io.ReadAll(post.Body)
	if err != nil {
		return err
	}
	if post.StatusCode >= 300 {
		return fmt.Errorf("retry partition: %s: %s", post.Status, postBody)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Retried partition %q (index %d); succeeded siblings are not re-run\n", value, index)
	return err
}

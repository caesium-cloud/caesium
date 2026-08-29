package run

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

		if retryFromFailurePartition != "" && retryFromFailureTask == "" {
			return fmt.Errorf("--task is required with --partition")
		}

		ctx := cmd.Context()

		// Past argument validation. Everything below talks to the server or the
		// store, and those failures are not usage errors: cobra prints the whole
		// usage block after ANY error from RunE, which buried the actionable
		// line — a per-partition retry against a local-mode server answers 409
		// with "requires distributed execution mode", and that message was
		// followed by forty lines of flag help. Same convention as cmd/verify:
		// flipped here rather than declared on the command, so a missing or
		// malformed flag above still gets its usage block.
		cmd.SilenceUsage = true

		if retryFromFailurePartition != "" {
			return retrySinglePartition(ctx, cmd, retryFromFailureJobID, retryFromFailureRunID, retryFromFailureTask, retryFromFailurePartition)
		}

		// `--server` decides transport for the whole-run path the same way
		// `caesium job lint` decides it (cmd/job/lint.go: serverMode :=
		// cmd.Flags().Changed("server")): an explicit --server routes the request
		// through the REST API; leaving it at its default keeps today's in-process
		// runstorage.Default() behavior so `caesium run retry` with no flags is
		// unchanged for local-mode callers.
		if cmd.Flags().Changed("server") {
			return retryWholeRunOverServer(ctx, cmd, retryFromFailureJobID, retryFromFailureRunID)
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
	retryCmd.Flags().StringVar(&retryFromFailureServer, "server", "http://localhost:8080", "Caesium server base URL; --partition always requires it, and passing --server explicitly also routes a whole-run retry through the REST API instead of the in-process store")
	retryCmd.Flags().StringVar(&retryFromFailureAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings; used only with --partition or an explicit --server)")
	retryCmd.MarkFlagRequired("job-id") //nolint:errcheck
	retryCmd.MarkFlagRequired("run-id") //nolint:errcheck

	Cmd.AddCommand(retryCmd)
}

// keyedPartition is the one instance a keyed lookup resolves to.
type keyedPartition struct {
	Value  string `json:"value"`
	Index  int    `json:"index"`
	Status string `json:"status"`
}

// lookupPartitionByValue resolves one partition value to its instance via the
// server's KEYED lookup (`?partition=<value>`).
//
// It deliberately does not list-and-scan. The listing endpoint is paginated
// (default page 100, hard cap 1000), so scanning "the" response only ever saw
// page one: `--partition` against a group larger than a page failed with
// `partition "x" not found` for every instance past the first page, and the
// larger the fan-out the more likely that is. The keyed lookup is O(1) on the
// server and cannot be truncated.
//
// The response is accepted in either shape the endpoint may return — a bare
// instance object, or the list envelope narrowed to one row — so the CLI is not
// coupled to which of the two the server settled on.
func lookupPartitionByValue(ctx context.Context, cmd *cobra.Command, server, jobID, runID, task, value string) (*keyedPartition, error) {
	lookupURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/tasks/%s/partitions?%s",
		server, jobID, runID, task, url.Values{"partition": {value}}.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lookupURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := resolveRunDiffAPIKey(cmd, retryFromFailureAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := retryPartitionHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("partition %q not found in task %q of run %s", value, task, runID)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("partitions: %s: %s", resp.Status, body)
	}

	var envelope struct {
		Partitions []keyedPartition `json:"partitions"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil && len(envelope.Partitions) > 0 {
		for i := range envelope.Partitions {
			if envelope.Partitions[i].Value == value {
				return &envelope.Partitions[i], nil
			}
		}
		return nil, fmt.Errorf("partition %q not found in task %q of run %s", value, task, runID)
	}

	var bare keyedPartition
	if err := json.Unmarshal(body, &bare); err != nil {
		return nil, fmt.Errorf("partitions response was not valid JSON: %w", err)
	}
	if bare.Value != value {
		return nil, fmt.Errorf("partition %q not found in task %q of run %s", value, task, runID)
	}
	return &bare, nil
}

// retryWholeRunOverServer retries a whole run through the existing
// POST /v1/jobs/:id/runs/:run_id/retry endpoint (api/rest/controller/job/run/retry.go),
// the same endpoint `caesium run retry --partition` already reuses for the
// keyed lookup/retry pair. It is selected when --server is passed explicitly
// (see the Changed("server") check in retryCmd.RunE); output is kept
// byte-for-byte identical to the in-process path's success line so scripts
// that scrape it do not need to branch on transport.
func retryWholeRunOverServer(ctx context.Context, cmd *cobra.Command, jobID, runID string) error {
	server := strings.TrimSuffix(retryFromFailureServer, "/")
	if server == "" {
		server = "http://localhost:8080"
	}
	retryURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/retry", server, jobID, runID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, retryURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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
		return fmt.Errorf("reading retry response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("retry run failed (%d): %s", resp.StatusCode, replayErrorMessage(body))
	}

	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Retrying run %s (job %s)\n", runID, jobID)
	return err
}

func retrySinglePartition(ctx context.Context, cmd *cobra.Command, jobID, runID, task, value string) error {
	server := strings.TrimSuffix(retryFromFailureServer, "/")
	if server == "" {
		server = "http://localhost:8080"
	}
	instance, err := lookupPartitionByValue(ctx, cmd, server, jobID, runID, task, value)
	if err != nil {
		return err
	}
	index := instance.Index
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

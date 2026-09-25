package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/spf13/cobra"
)

var (
	startJobID          string
	startServer         string
	startAPIKey         string
	startParams         []string
	startPriority       string
	startIdempotencyKey string

	startHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

type startRequest struct {
	Params   map[string]string `json:"params,omitempty"`
	Priority string            `json:"priority,omitempty"`
}

// startResponse is the 202 body of POST /v1/jobs/:id/run: the run itself when
// one was created (outcome "created"), otherwise the outcome of the start.
type startResponse struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	RunID   string `json:"run_id"`
	QueueID string `json:"queue_id"`

	replayed bool
}

var startCmd = &cobra.Command{
	Use:   "start --job-id <job-id> [--params k=v] [--priority high|normal|low] [--idempotency-key <key>]",
	Short: "Start a job run",
	Long: "Start a job run and print its run ID on stdout.\n\n" +
		"With --idempotency-key, retrying the same command returns the original start's outcome " +
		"instead of starting another run. A start the job's concurrency policy queues prints nothing " +
		"on stdout; rerun with the same key to get its run ID once it starts. A start that is skipped " +
		"(concurrency policy or a held upstream dataset) exits non-zero.",
	Args: cobra.NoArgs,
	RunE: runStart,
}

func runStart(cmd *cobra.Command, args []string) error {
	jobID := strings.TrimSpace(startJobID)
	if jobID == "" {
		return fmt.Errorf("--job-id is required")
	}

	params, err := parseRunStartParams(startParams)
	if err != nil {
		return err
	}

	priority := strings.TrimSpace(startPriority)
	if priority != "" {
		if _, err := runstorage.PriorityValue(priority); err != nil {
			return err
		}
	}

	key := strings.TrimSpace(startIdempotencyKey)
	resp, err := postStart(cmd, jobID, key, startRequest{
		Params:   params,
		Priority: priority,
	})
	if err != nil {
		return err
	}
	return reportStart(cmd, key, resp)
}

// reportStart keeps stdout to the run ID alone, so `RUN=$(caesium run start
// ...)` stays scriptable; everything else is guidance on stderr.
func reportStart(cmd *cobra.Command, key string, resp *startResponse) error {
	stderr := cmd.ErrOrStderr()
	if resp.replayed {
		_, _ = fmt.Fprintf(stderr, "idempotency key %q matched an earlier start; reporting its outcome\n", key)
	}
	switch resp.Outcome {
	case "", "created":
		if strings.TrimSpace(resp.ID) == "" {
			if resp.Outcome == "" {
				// A server that predates start outcomes answers a queued or
				// skipped start with an empty 202.
				_, _ = fmt.Fprintln(stderr, "run start accepted without creating a run (queued or skipped by the job's concurrency policy)")
				return nil
			}
			return fmt.Errorf("run start response did not include id")
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.ID)
		return nil
	case "queued":
		msg := fmt.Sprintf("run queued by the job's concurrency policy (queue id %s)", resp.QueueID)
		if key != "" {
			msg += "; rerun with the same --idempotency-key to get its run ID once it starts"
		}
		_, _ = fmt.Fprintln(stderr, msg)
		return nil
	case "skipped":
		if resp.RunID != "" {
			return fmt.Errorf("run start skipped (%s); recorded as skipped run %s", resp.Reason, resp.RunID)
		}
		return fmt.Errorf("run start skipped (%s)", resp.Reason)
	case "dropped":
		return fmt.Errorf("run start was queued (queue id %s) but its queue entry was removed before it ran", resp.QueueID)
	default:
		return fmt.Errorf("run start returned unknown outcome %q", resp.Outcome)
	}
}

func parseRunStartParams(values []string) (map[string]string, error) {
	params := make(map[string]string, len(values))
	for _, raw := range values {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--params must be k=v; got %q", raw)
		}
		params[strings.TrimSpace(key)] = value
	}
	if len(params) == 0 {
		return nil, nil
	}
	return params, nil
}

func postStart(cmd *cobra.Command, jobID, idempotencyKey string, payload startRequest) (*startResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	server := strings.TrimSuffix(startServer, "/")
	reqURL := fmt.Sprintf("%s/v1/jobs/%s/run", server, url.PathEscape(jobID))
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if apiKey := resolveRunDiffAPIKey(cmd, startAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := startHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading run start response: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("run start failed (%d): %s", resp.StatusCode, replayErrorMessage(respBody))
	}

	var decoded startResponse
	if len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, &decoded); err != nil {
			return nil, fmt.Errorf("run start response was not valid JSON (status %d): %w", resp.StatusCode, err)
		}
	}
	decoded.replayed = strings.EqualFold(resp.Header.Get("Idempotent-Replayed"), "true")
	return &decoded, nil
}

func init() {
	startCmd.Flags().StringVar(&startJobID, "job-id", "", "Job ID to start (required)")
	startCmd.Flags().StringVar(&startServer, "server", "http://localhost:8080", "Caesium server base URL")
	startCmd.Flags().StringVar(&startAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	startCmd.Flags().StringArrayVar(&startParams, "params", nil, "Run parameter as k=v (repeatable)")
	startCmd.Flags().StringVar(&startPriority, "priority", "", "Run priority override: high, normal, or low")
	startCmd.Flags().StringVar(&startIdempotencyKey, "idempotency-key", "", "Make the start idempotent: a retry with the same key returns the original start instead of starting another run")
	startCmd.MarkFlagRequired("job-id") //nolint:errcheck

	Cmd.AddCommand(startCmd)
}

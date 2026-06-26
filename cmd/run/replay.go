package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const (
	replayAwaitTimeout = 2 * time.Minute
	replayPollInterval = 500 * time.Millisecond
)

var (
	replayJobID          string
	replayServer         string
	replayAPIKey         string
	replayJSON           bool
	replayDiff           bool
	replaySets           []string
	replayIdempotencyKey string
)

type replayRequest struct {
	Set map[string]string `json:"set,omitempty"`
}

type replayResponse struct {
	RunID      string `json:"run_id"`
	Status     string `json:"status"`
	Quarantine bool   `json:"quarantine"`
}

type replayRunStatusResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

var replayCmd = &cobra.Command{
	Use:   "replay <run-id> --job-id <job-id> [--set k=v]",
	Short: "Fire a quarantined replay for a completed run",
	Long: "Fire a quarantined replay through the job-scoped replay REST endpoint. " +
		"--job-id is required because run lookup is intentionally scoped. " +
		"Pass --idempotency-key on manual retries to dedupe to the existing replay; " +
		"omitting it on a re-run intentionally starts another replay. When omitted, " +
		"the generated key is printed to stderr before dispatch so it can be reused. " +
		"Dedup is scoped per API key/principal: a retry must reuse the same key AND " +
		"the same credentials to resolve to the existing replay.",
	Args: cobra.ExactArgs(1),
	RunE: runReplay,
}

func runReplay(cmd *cobra.Command, args []string) error {
	baselineRunID := strings.TrimSpace(args[0])
	jobID := strings.TrimSpace(replayJobID)
	if jobID == "" {
		return fmt.Errorf("--job-id is required")
	}

	set, err := parseReplaySet(replaySets)
	if err != nil {
		return err
	}

	key, err := resolveReplayIdempotencyKey(cmd, replayIdempotencyKey, cmd.Flags().Changed("idempotency-key"), generateReplayIdempotencyKey)
	if err != nil {
		return err
	}

	resp, err := postReplay(cmd, jobID, baselineRunID, key, set)
	if err != nil {
		return err
	}

	if replayDiff {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "awaiting replay run %s\n", resp.RunID)
		run, err := awaitReplayRun(cmd, jobID, resp.RunID, replayAwaitTimeout)
		if err != nil {
			return err
		}
		replayFailed := strings.EqualFold(strings.TrimSpace(run.Status), "failed")
		diff, err := fetchReplayDiff(cmd, jobID, baselineRunID, resp.RunID)
		if err != nil {
			return err
		}
		if replayJSON {
			var out interface{}
			if err := json.Unmarshal(diff, &out); err != nil {
				return fmt.Errorf("run diff response was not valid JSON: %w", err)
			}
			pretty, _ := json.MarshalIndent(out, "", "  ")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(pretty))
			if replayFailed {
				return fmt.Errorf("replay run %s failed", resp.RunID)
			}
			return nil
		}
		var rendered runDiffResponse
		if err := json.Unmarshal(diff, &rendered); err != nil {
			return fmt.Errorf("run diff response was not valid JSON: %w", err)
		}
		renderRunDiffTable(cmd, &rendered)
		if replayFailed {
			return fmt.Errorf("replay run %s failed", resp.RunID)
		}
		return nil
	}

	if replayJSON {
		pretty, _ := json.MarshalIndent(resp, "", "  ")
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(pretty))
		return nil
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.RunID)
	return nil
}

func parseReplaySet(values []string) (map[string]string, error) {
	set := make(map[string]string, len(values))
	for _, raw := range values {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("--set must be k=v; got %q", raw)
		}
		set[strings.TrimSpace(key)] = value
	}
	return set, nil
}

func resolveReplayIdempotencyKey(cmd *cobra.Command, supplied string, suppliedSet bool, generate func() (string, error)) (string, error) {
	if suppliedSet {
		return supplied, nil
	}
	key, err := generate()
	if err != nil {
		return "", fmt.Errorf("generate replay idempotency key: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "replay idempotency key: %s\n", key)
	return key, nil
}

func generateReplayIdempotencyKey() (string, error) {
	return "replay-" + uuid.NewString(), nil
}

func postReplay(cmd *cobra.Command, jobID, baselineRunID, idempotencyKey string, set map[string]string) (*replayResponse, error) {
	body, err := json.Marshal(replayRequest{Set: set})
	if err != nil {
		return nil, err
	}

	server := strings.TrimSuffix(replayServer, "/")
	reqURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/replay", server, url.PathEscape(jobID), url.PathEscape(baselineRunID))
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	if apiKey := resolveRunDiffAPIKey(cmd, replayAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading replay response: %w", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		return nil, replayStatusError(resp.StatusCode, respBody)
	}

	var decoded replayResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("replay response was not valid JSON (status %d): %w", resp.StatusCode, err)
	}
	if strings.TrimSpace(decoded.RunID) == "" {
		return nil, fmt.Errorf("replay response did not include run_id")
	}
	return &decoded, nil
}

func awaitReplayRun(cmd *cobra.Command, jobID, runID string, timeout time.Duration) (*replayRunStatusResponse, error) {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(replayPollInterval)
	defer ticker.Stop()
	var lastStatus string
	for {
		run, err := fetchReplayRunStatus(cmd, jobID, runID)
		if err != nil {
			return nil, err
		}
		lastStatus = run.Status
		if isReplayTerminalStatus(run.Status) {
			return run, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for replay run %s to finish (last status %q)", runID, lastStatus)
		}

		select {
		case <-cmd.Context().Done():
			return nil, cmd.Context().Err()
		case <-ticker.C:
		}
	}
}

func fetchReplayRunStatus(cmd *cobra.Command, jobID, runID string) (*replayRunStatusResponse, error) {
	server := strings.TrimSuffix(replayServer, "/")
	reqURL := fmt.Sprintf("%s/v1/jobs/%s/runs/%s", server, url.PathEscape(jobID), url.PathEscape(runID))
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := resolveRunDiffAPIKey(cmd, replayAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading run status response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("fetch replay run status failed (%d): %s", resp.StatusCode, replayErrorMessage(body))
	}
	var run replayRunStatusResponse
	if err := json.Unmarshal(body, &run); err != nil {
		return nil, fmt.Errorf("run status response was not valid JSON (status %d): %w", resp.StatusCode, err)
	}
	return &run, nil
}

func fetchReplayDiff(cmd *cobra.Command, jobID, baselineRunID, replayRunID string) ([]byte, error) {
	server := strings.TrimSuffix(replayServer, "/")
	params := url.Values{}
	params.Set("left", baselineRunID)
	params.Set("right", replayRunID)
	reqURL := fmt.Sprintf("%s/v1/jobs/%s/runs/diff?%s", server, url.PathEscape(jobID), params.Encode())

	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := resolveRunDiffAPIKey(cmd, replayAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading run diff response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("run diff failed (%d): %s", resp.StatusCode, replayErrorMessage(body))
	}
	return body, nil
}

func isReplayTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "failed":
		return true
	default:
		return false
	}
}

func replayStatusError(status int, body []byte) error {
	msg := replayErrorMessage(body)
	switch status {
	case http.StatusBadRequest:
		return fmt.Errorf("replay request rejected (400): %s", msg)
	case http.StatusNotFound:
		return fmt.Errorf("replay target not found (404): %s", msg)
	case http.StatusConflict:
		return fmt.Errorf("replay refused (409): %s", msg)
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("replay-safe refusal (422): %s", msg)
	default:
		return fmt.Errorf("replay failed (%d): %s", status, msg)
	}
}

func replayErrorMessage(body []byte) string {
	var decoded struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err == nil {
		if strings.TrimSpace(decoded.Message) != "" {
			return strings.TrimSpace(decoded.Message)
		}
		if strings.TrimSpace(decoded.Error) != "" {
			return strings.TrimSpace(decoded.Error)
		}
	}
	return strings.TrimSpace(string(body))
}

func init() {
	replayCmd.Flags().StringVar(&replayJobID, "job-id", "", "Job ID that owns the baseline run (required)")
	replayCmd.Flags().StringVar(&replayServer, "server", "http://localhost:8080", "Caesium server base URL")
	replayCmd.Flags().StringVar(&replayAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	replayCmd.Flags().StringArrayVar(&replaySets, "set", nil, "Override a run parameter for the replay as k=v (repeatable)")
	replayCmd.Flags().StringVar(&replayIdempotencyKey, "idempotency-key", "", "Replay idempotency key to reuse on retries; omit only when intentionally starting a distinct replay")
	replayCmd.Flags().BoolVar(&replayDiff, "diff", false, "Wait for the replay to finish and render the run diff against the baseline")
	replayCmd.Flags().BoolVar(&replayJSON, "json", false, "Emit machine-readable JSON")

	Cmd.AddCommand(replayCmd)
}

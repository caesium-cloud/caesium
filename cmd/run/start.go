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
	startJobID    string
	startServer   string
	startAPIKey   string
	startParams   []string
	startPriority string

	startHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

type startRequest struct {
	Params   map[string]string `json:"params,omitempty"`
	Priority string            `json:"priority,omitempty"`
}

type startResponse struct {
	ID string `json:"id"`
}

var startCmd = &cobra.Command{
	Use:   "start --job-id <job-id> [--params k=v] [--priority high|normal|low]",
	Short: "Start a job run",
	Args:  cobra.NoArgs,
	RunE:  runStart,
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

	resp, err := postStart(cmd, jobID, startRequest{
		Params:   params,
		Priority: priority,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.ID)
	return nil
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

func postStart(cmd *cobra.Command, jobID string, payload startRequest) (*startResponse, error) {
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
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("run start response was not valid JSON (status %d): %w", resp.StatusCode, err)
	}
	if strings.TrimSpace(decoded.ID) == "" {
		return nil, fmt.Errorf("run start response did not include id")
	}
	return &decoded, nil
}

func init() {
	startCmd.Flags().StringVar(&startJobID, "job-id", "", "Job ID to start (required)")
	startCmd.Flags().StringVar(&startServer, "server", "http://localhost:8080", "Caesium server base URL")
	startCmd.Flags().StringVar(&startAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	startCmd.Flags().StringArrayVar(&startParams, "params", nil, "Run parameter as k=v (repeatable)")
	startCmd.Flags().StringVar(&startPriority, "priority", "", "Run priority override: high, normal, or low")
	startCmd.MarkFlagRequired("job-id") //nolint:errcheck

	Cmd.AddCommand(startCmd)
}

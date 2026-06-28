package trigger

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

const apiKeyEnvVar = "CAESIUM_API_KEY"

var (
	eventsServer string
	eventsAPIKey string
	eventsType   string
	eventsSource string
	eventsLimit  uint64
	eventsOffset uint64
)

type jobSummary struct {
	ID        string `json:"id"`
	Alias     string `json:"alias"`
	TriggerID string `json:"trigger_id"`
}

var eventsCmd = &cobra.Command{
	Use:   "events <alias>",
	Short: "List durable events matched by a trigger",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(eventsServer, "/")
		triggerID, err := resolveTriggerID(cmd, server, strings.TrimSpace(args[0]))
		if err != nil {
			return err
		}

		params := url.Values{}
		if eventsType != "" {
			params.Set("type", eventsType)
		}
		if eventsSource != "" {
			params.Set("source", eventsSource)
		}
		if eventsLimit > 0 {
			params.Set("limit", fmt.Sprintf("%d", eventsLimit))
		}
		if eventsOffset > 0 {
			params.Set("offset", fmt.Sprintf("%d", eventsOffset))
		}

		reqURL := fmt.Sprintf("%s/v1/triggers/%s/events", server, url.PathEscape(triggerID))
		if encoded := params.Encode(); encoded != "" {
			reqURL += "?" + encoded
		}
		body, err := get(cmd, reqURL, "trigger events")
		if err != nil {
			return err
		}
		return writePrettyJSON(cmd, body, "trigger events response")
	},
}

func resolveTriggerID(cmd *cobra.Command, server, alias string) (string, error) {
	if alias == "" {
		return "", fmt.Errorf("trigger alias is required")
	}
	if _, err := uuid.Parse(alias); err == nil {
		return alias, nil
	}

	body, err := get(cmd, server+"/v1/jobs", "job lookup")
	if err != nil {
		return "", err
	}
	var jobs []jobSummary
	if err := json.Unmarshal(body, &jobs); err != nil {
		return "", fmt.Errorf("job lookup returned invalid JSON: %w", err)
	}
	for _, job := range jobs {
		if job.Alias == alias {
			if strings.TrimSpace(job.TriggerID) == "" {
				return "", fmt.Errorf("job alias %q has no trigger_id", alias)
			}
			return job.TriggerID, nil
		}
	}
	return "", fmt.Errorf("job alias %q not found", alias)
}

func get(cmd *cobra.Command, reqURL, label string) ([]byte, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := resolveAPIKey(cmd, eventsAPIKey); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", label, err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("%s failed (%d): %s", label, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func resolveAPIKey(cmd *cobra.Command, flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: --api-key is visible in process listings; prefer %s\n", apiKeyEnvVar)
		return strings.TrimSpace(flagValue)
	}
	return strings.TrimSpace(os.Getenv(apiKeyEnvVar))
}

func writePrettyJSON(cmd *cobra.Command, body []byte, label string) error {
	var out any
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("%s was not valid JSON: %w", label, err)
	}
	pretty, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("re-encoding %s: %w", label, err)
	}
	_, _ = cmd.OutOrStdout().Write(pretty)
	_, _ = fmt.Fprintln(cmd.OutOrStdout())
	return nil
}

func init() {
	eventsCmd.Flags().StringVar(&eventsServer, "server", "http://localhost:8080", "Caesium server base URL")
	eventsCmd.Flags().StringVar(&eventsAPIKey, "api-key", "", "API key for authentication (prefer "+apiKeyEnvVar+"; --api-key is visible in process listings)")
	eventsCmd.Flags().StringVar(&eventsType, "type", "", "Filter by event type")
	eventsCmd.Flags().StringVar(&eventsSource, "source", "", "Filter by event source")
	eventsCmd.Flags().Uint64Var(&eventsLimit, "limit", 0, "Maximum number of events to return")
	eventsCmd.Flags().Uint64Var(&eventsOffset, "offset", 0, "Number of events to skip")
}

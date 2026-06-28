package event

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

const (
	eventIngestAPIKeyEnvVar = "CAESIUM_EVENT_INGEST_API_KEY"
	apiKeyEnvVar            = "CAESIUM_API_KEY"
)

var (
	pushType   string
	pushSource string
	pushData   string
	pushServer string
	pushAPIKey string
)

type pushRequest struct {
	Type   string          `json:"type"`
	Source string          `json:"source,omitempty"`
	Data   json.RawMessage `json:"data"`
}

var pushCmd = &cobra.Command{
	Use:   "push --type <type> [--source <source>] --data '{}'",
	Short: "Push an event into the event-trigger router",
	RunE: func(cmd *cobra.Command, args []string) error {
		eventType := strings.TrimSpace(pushType)
		if eventType == "" {
			return fmt.Errorf("--type is required")
		}
		data, err := parseEventData(pushData)
		if err != nil {
			return err
		}

		payload, err := json.Marshal(pushRequest{
			Type:   eventType,
			Source: strings.TrimSpace(pushSource),
			Data:   data,
		})
		if err != nil {
			return err
		}

		server := strings.TrimSuffix(pushServer, "/")
		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, server+"/v1/events", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey := resolveEventAPIKey(cmd, pushAPIKey); apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("reading event push response: %w", err)
		}
		if resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("event push failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		return writePrettyJSON(cmd, body, "event push response")
	},
}

func parseEventData(raw string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	data := json.RawMessage(raw)
	if !json.Valid(data) {
		return nil, fmt.Errorf("--data must be valid JSON")
	}
	return data, nil
}

func resolveEventAPIKey(cmd *cobra.Command, flagValue string) string {
	if strings.TrimSpace(flagValue) != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: --api-key is visible in process listings; prefer %s\n", eventIngestAPIKeyEnvVar)
		return strings.TrimSpace(flagValue)
	}
	if value := strings.TrimSpace(os.Getenv(eventIngestAPIKeyEnvVar)); value != "" {
		return value
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
	pushCmd.Flags().StringVar(&pushType, "type", "", "Event type to ingest (required)")
	pushCmd.Flags().StringVar(&pushSource, "source", "", "Event source")
	pushCmd.Flags().StringVar(&pushData, "data", "{}", "JSON event payload")
	pushCmd.Flags().StringVar(&pushServer, "server", "http://localhost:8080", "Caesium server base URL")
	pushCmd.Flags().StringVar(&pushAPIKey, "api-key", "", "Event ingest API key for authentication (prefer "+eventIngestAPIKeyEnvVar+"; --api-key is visible in process listings)")
}

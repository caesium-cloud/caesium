// Package system implements `caesium system`: cluster membership inspection
// and the supported removal of a stale dqlite member.
package system

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	serverFlag string
	apiKeyFlag string
)

var httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}

// Cmd is the root `caesium system` command group.
var Cmd = newCommand()

// newCommand builds the command tree. Tests build a fresh one per case so flag
// values never leak between executions.
func newCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Inspect and administer the Caesium cluster",
	}
	cmd.PersistentFlags().StringVar(&serverFlag, "server", "http://localhost:8080", "Caesium server base URL")
	cmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "",
		"API key for authentication (prefer "+cliutil.APIKeyEnvVar+"; --api-key is visible in process listings)")
	nodes := &cobra.Command{
		Use:   "nodes",
		Short: "List dqlite cluster members, or remove a stale one",
	}
	nodes.AddCommand(newListCommand(), newRemoveCommand())
	cmd.AddCommand(nodes)
	return cmd
}

// response is one HTTP exchange: the status code and the raw body.
type response struct {
	status int
	body   []byte
}

func request(cmd *cobra.Command, method, path string) (response, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), method, strings.TrimSuffix(serverFlag, "/")+path, nil)
	if err != nil {
		return response{}, err
	}
	if apiKey := cliutil.ResolveAPIKey(cmd, apiKeyFlag, cliutil.APIKeyEnvVar); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return response{}, fmt.Errorf("reading %s %s response: %w", method, path, err)
	}
	return response{status: resp.StatusCode, body: body}, nil
}

// serverMessage extracts the human message from an error body, falling back to
// the raw text.
func serverMessage(body []byte) string {
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Message != "" {
		return payload.Message
	}
	return strings.TrimSpace(string(body))
}

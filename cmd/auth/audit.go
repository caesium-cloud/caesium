package auth

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	auditSince  string
	auditActor  string
	auditAction string
	auditLimit  int
	auditServer string
	auditAPIKey string
)

var auditCmd = &cobra.Command{
	Use:     "audit",
	Short:   "Query the audit log",
	Example: `  caesium auth audit --since 24h --actor csk_live_a1b2`,
	RunE: func(cmd *cobra.Command, args []string) error {
		server := strings.TrimSuffix(auditServer, "/")
		apiKey := resolveAPIKey(cmd, auditAPIKey)

		params := url.Values{}
		if auditSince != "" {
			params.Set("since", auditSince)
		}
		if auditActor != "" {
			params.Set("actor", auditActor)
		}
		if auditAction != "" {
			params.Set("action", auditAction)
		}
		if auditLimit > 0 {
			params.Set("limit", fmt.Sprintf("%d", auditLimit))
		}

		reqURL := fmt.Sprintf("%s/v1/auth/audit?%s", server, params.Encode())

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("audit query failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		// The audit log is this command's whole output; cobra's Print* helpers
		// write to OutOrStderr, so `caesium auth audit | jq` read nothing.
		return cliutil.WritePrettyJSON(cmd, body, "auth audit response")
	},
}

func init() {
	auditCmd.Flags().StringVar(&auditSince, "since", "", "Show entries since duration (e.g. 24h) or RFC3339 timestamp")
	auditCmd.Flags().StringVar(&auditActor, "actor", "", "Filter by actor (key prefix or OIDC subject)")
	auditCmd.Flags().StringVar(&auditAction, "action", "", "Filter by action (e.g. job.create, api_key.revoke)")
	auditCmd.Flags().IntVar(&auditLimit, "limit", 100, "Maximum number of entries to return")
	auditCmd.Flags().StringVar(&auditServer, "server", "http://localhost:8080", "Caesium server base URL")
	auditCmd.Flags().StringVar(&auditAPIKey, "api-key", "", apiKeyFlagUsage("Admin"))

	Cmd.AddCommand(auditCmd)
}

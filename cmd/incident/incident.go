package incident

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

const incidentAPIKeyEnvVar = "CAESIUM_INCIDENT_API_KEY"

var (
	serverFlag string
	apiKeyFlag string
	jsonFlag   bool

	listStatus        string
	listClass         string
	listNeedsApproval bool
	listJobID         string
	listLimit         int
	listOffset        int

	approvalID    string
	approveReason string
	rejectReason  string

	httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

// Cmd is the `caesium incident` command group.
var Cmd = &cobra.Command{
	Use:   "incident",
	Short: "Inspect and decide agent-remediation incidents",
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List incidents",
	RunE: func(cmd *cobra.Command, args []string) error {
		params := url.Values{}
		if status := strings.TrimSpace(listStatus); status != "" {
			params.Set("status", status)
		}
		if class := strings.TrimSpace(listClass); class != "" {
			params.Set("class", class)
		}
		if cmd.Flags().Changed("needs-approval") {
			params.Set("needs_approval", strconv.FormatBool(listNeedsApproval))
		}
		if jobID := strings.TrimSpace(listJobID); jobID != "" {
			params.Set("job_id", jobID)
		}
		if listLimit < 0 {
			return fmt.Errorf("--limit must be greater than or equal to 0")
		}
		if listLimit > 0 {
			params.Set("limit", strconv.Itoa(listLimit))
		}
		if listOffset < 0 {
			return fmt.Errorf("--offset must be greater than or equal to 0")
		}
		if listOffset > 0 {
			params.Set("offset", strconv.Itoa(listOffset))
		}

		reqURL := strings.TrimSuffix(serverFlag, "/") + "/v1/incidents"
		if qs := params.Encode(); qs != "" {
			reqURL += "?" + qs
		}

		body, err := doRequest(cmd, http.MethodGet, reqURL, nil, http.StatusOK, "incident list")
		if err != nil {
			return err
		}
		return cliutil.WritePrettyJSON(cmd, body, "incident list")
	},
}

var getCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get an incident timeline",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := strings.TrimSpace(args[0])
		if id == "" {
			return fmt.Errorf("incident id is required")
		}

		reqURL := strings.TrimSuffix(serverFlag, "/") + "/v1/incidents/" + url.PathEscape(id)
		body, err := doRequest(cmd, http.MethodGet, reqURL, nil, http.StatusOK, "incident get")
		if err != nil {
			return err
		}
		return cliutil.WritePrettyJSON(cmd, body, "incident get")
	},
}

var approveCmd = &cobra.Command{
	Use:   "approve <id> --approval <approval-id>",
	Short: "Approve a pending incident action",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return decide(cmd, strings.TrimSpace(args[0]), true, approveReason)
	},
}

var rejectCmd = &cobra.Command{
	Use:   "reject <id> --approval <approval-id> [--reason <reason>]",
	Short: "Reject a pending incident action",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return decide(cmd, strings.TrimSpace(args[0]), false, rejectReason)
	},
}

func decide(cmd *cobra.Command, incidentID string, approve bool, reason string) error {
	if incidentID == "" {
		return fmt.Errorf("incident id is required")
	}
	approval := strings.TrimSpace(approvalID)
	if approval == "" {
		return fmt.Errorf("--approval is required")
	}

	action := "reject"
	if approve {
		action = "approve"
	}
	reqURL := strings.TrimSuffix(serverFlag, "/") +
		"/v1/incidents/" + url.PathEscape(incidentID) +
		"/approvals/" + url.PathEscape(approval) +
		"/" + action

	var body io.Reader
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		payload, err := json.Marshal(map[string]string{"reason": trimmed})
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}

	respBody, err := doRequest(cmd, http.MethodPost, reqURL, body, http.StatusOK, "incident "+action)
	if err != nil {
		return err
	}
	return cliutil.WritePrettyJSON(cmd, respBody, "incident "+action)
}

func doRequest(cmd *cobra.Command, method, reqURL string, body io.Reader, wantStatus int, label string) ([]byte, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), method, reqURL, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if k := cliutil.ResolveAPIKey(cmd, apiKeyFlag, incidentAPIKeyEnvVar, cliutil.APIKeyEnvVar); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s response: %w", label, err)
	}
	if resp.StatusCode != wantStatus {
		return nil, fmt.Errorf("%s failed (%d): %s", label, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

func init() {
	Cmd.PersistentFlags().StringVar(&serverFlag, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "Incident API key for authentication (prefer "+incidentAPIKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.PersistentFlags().BoolVar(&jsonFlag, "json", false, "Print machine-readable JSON")

	listCmd.Flags().StringVar(&listStatus, "status", "", "Filter incidents by status")
	listCmd.Flags().StringVar(&listClass, "class", "", "Filter incidents by class")
	listCmd.Flags().BoolVar(&listNeedsApproval, "needs-approval", false, "Filter incidents that need approval")
	listCmd.Flags().StringVar(&listJobID, "job-id", "", "Filter incidents by job ID")
	listCmd.Flags().IntVar(&listLimit, "limit", 0, "Maximum incidents to return")
	listCmd.Flags().IntVar(&listOffset, "offset", 0, "Incident list offset")

	approveCmd.Flags().StringVar(&approvalID, "approval", "", "Approval request ID (required)")
	approveCmd.Flags().StringVar(&approveReason, "reason", "", "Optional approval reason")
	_ = approveCmd.MarkFlagRequired("approval")

	rejectCmd.Flags().StringVar(&approvalID, "approval", "", "Approval request ID (required)")
	rejectCmd.Flags().StringVar(&rejectReason, "reason", "", "Optional rejection reason")
	_ = rejectCmd.MarkFlagRequired("approval")

	Cmd.AddCommand(listCmd, getCmd, approveCmd, rejectCmd)
}

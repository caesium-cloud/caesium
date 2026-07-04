package agentprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const agentProfileAPIKeyEnvVar = "CAESIUM_AGENTPROFILE_API_KEY"

var (
	serverFlag string
	apiKeyFlag string
	jsonFlag   bool

	listLimit   int
	listOffset  int
	listOrderBy string

	applyPath string
	applyID   string

	httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

// Cmd is the `caesium agentprofile` command group.
var Cmd = &cobra.Command{
	Use:   "agentprofile",
	Short: "Manage agent remediation profiles",
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List agent profiles",
	RunE: func(cmd *cobra.Command, args []string) error {
		params := url.Values{}
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
		if orderBy := strings.TrimSpace(listOrderBy); orderBy != "" {
			params.Set("order_by", orderBy)
		}

		reqURL := strings.TrimSuffix(serverFlag, "/") + "/v1/agentprofiles"
		if qs := params.Encode(); qs != "" {
			reqURL += "?" + qs
		}

		body, err := doRequest(cmd, http.MethodGet, reqURL, nil, http.StatusOK, "agentprofile list")
		if err != nil {
			return err
		}
		return cliutil.WritePrettyJSON(cmd, body, "agentprofile list")
	},
}

var getCmd = &cobra.Command{
	Use:   "get <id>",
	Short: "Get an agent profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := strings.TrimSpace(args[0])
		if id == "" {
			return fmt.Errorf("agent profile id is required")
		}

		reqURL := strings.TrimSuffix(serverFlag, "/") + "/v1/agentprofiles/" + url.PathEscape(id)
		body, err := doRequest(cmd, http.MethodGet, reqURL, nil, http.StatusOK, "agentprofile get")
		if err != nil {
			return err
		}
		return cliutil.WritePrettyJSON(cmd, body, "agentprofile get")
	},
}

var applyCmd = &cobra.Command{
	Use:   "apply --path <file|-|stdin>",
	Short: "Create or update an agent profile",
	Long: "Create an AgentProfile from a YAML or JSON document. " +
		"Use --id to update an existing profile by ID; updates are sent with PATCH.",
	RunE: func(cmd *cobra.Command, args []string) error {
		profile, err := readProfileFile(applyPath)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(profile.toRequest())
		if err != nil {
			return err
		}

		// Only an explicit --id selects PATCH (update by ID). A file-embedded
		// `id` must NOT force an update: a profile document copied across
		// environments would otherwise clobber an existing profile with the same
		// ID instead of creating a new one (greptile P1, #286).
		id := strings.TrimSpace(applyID)

		method := http.MethodPost
		wantStatus := http.StatusCreated
		reqURL := strings.TrimSuffix(serverFlag, "/") + "/v1/agentprofiles"
		label := "agentprofile apply"
		if id != "" {
			method = http.MethodPatch
			wantStatus = http.StatusOK
			reqURL += "/" + url.PathEscape(id)
			label = "agentprofile patch"
		}

		body, err := doRequest(cmd, method, reqURL, bytes.NewReader(payload), wantStatus, label)
		if err != nil {
			return err
		}
		return cliutil.WritePrettyJSON(cmd, body, label)
	},
}

type profileFile struct {
	ID         string                 `json:"id,omitempty" yaml:"id,omitempty"`
	Name       *string                `json:"name,omitempty" yaml:"name,omitempty"`
	Image      *string                `json:"image,omitempty" yaml:"image,omitempty"`
	Engine     *string                `json:"engine,omitempty" yaml:"engine,omitempty"`
	Limits     map[string]interface{} `json:"limits,omitempty" yaml:"limits,omitempty"`
	SecretRefs map[string]string      `json:"secret_refs,omitempty" yaml:"secret_refs,omitempty"`
	Budgets    map[string]interface{} `json:"budgets,omitempty" yaml:"budgets,omitempty"`
	Playbook   map[string]interface{} `json:"playbook,omitempty" yaml:"playbook,omitempty"`
}

type profileRequest struct {
	Name       *string                `json:"name,omitempty"`
	Image      *string                `json:"image,omitempty"`
	Engine     *string                `json:"engine,omitempty"`
	Limits     map[string]interface{} `json:"limits,omitempty"`
	SecretRefs map[string]string      `json:"secret_refs,omitempty"`
	Budgets    map[string]interface{} `json:"budgets,omitempty"`
	Playbook   map[string]interface{} `json:"playbook,omitempty"`
}

func (p profileFile) toRequest() profileRequest {
	return profileRequest{
		Name:       p.Name,
		Image:      p.Image,
		Engine:     p.Engine,
		Limits:     p.Limits,
		SecretRefs: p.SecretRefs,
		Budgets:    p.Budgets,
		Playbook:   p.Playbook,
	}
}

func readProfileFile(path string) (*profileFile, error) {
	path = strings.TrimSpace(path)
	var (
		data []byte
		err  error
	)
	if path == "" || path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("agent profile input is empty")
	}

	var profile profileFile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		if path == "" || path == "-" {
			return nil, fmt.Errorf("stdin: %w", err)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &profile, nil
}

func doRequest(cmd *cobra.Command, method, reqURL string, body io.Reader, wantStatus int, label string) ([]byte, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), method, reqURL, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if k := cliutil.ResolveAPIKey(cmd, apiKeyFlag, agentProfileAPIKeyEnvVar, cliutil.APIKeyEnvVar); k != "" {
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
	Cmd.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "AgentProfile API key for authentication (prefer "+agentProfileAPIKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.PersistentFlags().BoolVar(&jsonFlag, "json", false, "Print machine-readable JSON")

	listCmd.Flags().IntVar(&listLimit, "limit", 0, "Maximum agent profiles to return")
	listCmd.Flags().IntVar(&listOffset, "offset", 0, "Agent profile list offset")
	listCmd.Flags().StringVar(&listOrderBy, "order-by", "", "Order by clause passed to the API")

	applyCmd.Flags().StringVar(&applyPath, "path", "-", "Path to a YAML/JSON agent profile, or - for stdin")
	applyCmd.Flags().StringVar(&applyID, "id", "", "Existing agent profile ID to update with PATCH")

	Cmd.AddCommand(listCmd, getCmd, applyCmd)
}

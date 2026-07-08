package contract

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}

type featuresResponse struct {
	ContractEnforcementEnabled bool `json:"contract_enforcement_enabled"`
}

func serverBase() string {
	return strings.TrimSuffix(serverFlag, "/")
}

func request(cmd *cobra.Command, apiKey, method, reqURL string, body io.Reader, label string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), method, reqURL, body)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading %s response: %w", label, err)
	}
	return data, resp.StatusCode, nil
}

func ensureContractEnforcementEnabled(cmd *cobra.Command, apiKey string) error {
	body, status, err := request(cmd, apiKey, http.MethodGet, serverBase()+"/v1/system/features", nil, "system features")
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return disabledError("contract enforcement feature status is unavailable")
	}
	if status >= http.StatusBadRequest {
		return fmt.Errorf("system features failed (%d): %s", status, strings.TrimSpace(string(body)))
	}

	var features featuresResponse
	if err := decodeJSON(body, &features, "system features"); err != nil {
		return err
	}
	if !features.ContractEnforcementEnabled {
		return disabledError("contract enforcement is disabled on the server")
	}
	return nil
}

func disabledError(prefix string) error {
	return fmt.Errorf("%s; set CAESIUM_CONTRACT_ENFORCEMENT=fail (or warn) on the Caesium server and restart it", prefix)
}

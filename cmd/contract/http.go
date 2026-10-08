package contract

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
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
	headers := make(http.Header)
	if body != nil {
		headers.Set("Content-Type", "application/json")
	}
	if apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	data, status, err := clihttp.Exchange(cmd.Context(), httpClient, method, reqURL, body, headers)
	if err != nil {
		if status == 0 {
			return nil, 0, err
		}
		return nil, status, fmt.Errorf("reading %s response: %w", label, err)
	}
	return data, status, nil
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

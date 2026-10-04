package dataset

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/clihttp"
	"github.com/spf13/cobra"
)

const apiKeyEnvVar = cliutil.APIKeyEnvVar

var httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}

type httpStatusError struct {
	StatusCode int
	Body       string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("dataset request failed (%d): %s", e.StatusCode, e.Body)
}

func request(cmd *cobra.Command, method, reqURL string, body io.Reader) ([]byte, error) {
	headers := make(http.Header)
	if body != nil {
		headers.Set("Content-Type", "application/json")
	}
	if apiKey := cliutil.ResolveAPIKey(cmd, apiKeyFlag, apiKeyEnvVar); apiKey != "" {
		headers.Set("Authorization", "Bearer "+apiKey)
	}
	data, status, err := clihttp.Exchange(cmd.Context(), httpClient, method, reqURL, body, headers)
	if err != nil {
		if status == 0 {
			return nil, err
		}
		return nil, fmt.Errorf("reading dataset response: %w", err)
	}
	if status >= http.StatusBadRequest {
		return nil, &httpStatusError{StatusCode: status, Body: strings.TrimSpace(string(data))}
	}
	return data, nil
}

func serverBase() string {
	return strings.TrimSuffix(serverFlag, "/")
}

func datasetPath(namespace, name string) string {
	nsSegment := namespace
	if nsSegment == "" {
		nsSegment = "_"
	}
	return "/v1/datasets/" + url.PathEscape(nsSegment) + "/" + url.PathEscape(name)
}

// splitDatasetRef resolves a dataset argument into (namespace, name). Dataset
// names are free-form identifiers that routinely contain dots (e.g.
// "raw.vendor_x"), and the namespace is a separate, distinct axis (unused in
// v1), so the whole argument is taken as the name. A namespace, when needed, is
// supplied explicitly via --namespace rather than parsed out of the name.
func splitDatasetRef(raw string) (string, string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", "", fmt.Errorf("dataset name is required")
	}
	return datasetNamespace(), name, nil
}

func datasetNamespace() string {
	ns := strings.TrimSpace(namespaceFlag)
	if ns == "_" {
		ns = ""
	}
	return ns
}

package dataset

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

const apiKeyEnvVar = cliutil.APIKeyEnvVar

var httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}

func request(cmd *cobra.Command, method, reqURL string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), method, reqURL, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if apiKey := cliutil.ResolveAPIKey(cmd, apiKeyFlag, apiKeyEnvVar); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading dataset response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("dataset request failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
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

func splitDatasetRef(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("dataset reference is required")
	}
	ns, name, ok := strings.Cut(raw, ".")
	if !ok {
		return "", raw, nil
	}
	ns = strings.TrimSpace(ns)
	name = strings.TrimSpace(name)
	if ns == "" || name == "" {
		return "", "", fmt.Errorf("dataset reference must be <namespace.name> or <name>")
	}
	if ns == "_" {
		ns = ""
	}
	return ns, name, nil
}

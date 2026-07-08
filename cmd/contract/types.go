package contract

import (
	"encoding/json"
	"fmt"
	"strings"
)

type graphResponse struct {
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"edges"`
}

type graphNode struct {
	ID      string            `json:"id"`
	Kind    string            `json:"kind"`
	Alias   string            `json:"alias,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Dataset *datasetRef       `json:"dataset,omitempty"`
}

type graphEdge struct {
	ID       string         `json:"id"`
	From     string         `json:"from"`
	To       string         `json:"to"`
	Class    string         `json:"class"`
	Verdict  string         `json:"verdict,omitempty"`
	Findings []graphFinding `json:"findings,omitempty"`
	Dataset  *datasetRef    `json:"dataset,omitempty"`
}

type graphFinding struct {
	Kind    string `json:"kind,omitempty"`
	Path    string `json:"path,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Verdict string `json:"verdict"`
}

type datasetRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func decodeJSON(body []byte, target any, label string) error {
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%s response was not valid JSON: %w", label, err)
	}
	return nil
}

func datasetName(ref *datasetRef) string {
	if ref == nil {
		return "-"
	}
	if strings.TrimSpace(ref.Namespace) == "" {
		return ref.Name
	}
	return ref.Namespace + "/" + ref.Name
}

func cleanCell(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "-"
	}
	replacer := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ")
	return replacer.Replace(value)
}

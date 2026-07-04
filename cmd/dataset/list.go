package dataset

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	listStatus string
	listJSON   bool
)

type listResponse struct {
	Datasets []datasetState `json:"datasets"`
	Total    int64          `json:"total"`
	Limit    int            `json:"limit"`
	Offset   int            `json:"offset"`
}

type datasetState struct {
	Namespace string    `json:"namespace,omitempty"`
	Name      string    `json:"name"`
	Watermark string    `json:"watermark"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List freshness dataset states",
	RunE: func(cmd *cobra.Command, args []string) error {
		params := url.Values{}
		if status := strings.TrimSpace(listStatus); status != "" {
			params.Set("status", status)
		}
		reqURL := serverBase() + "/v1/datasets"
		if encoded := params.Encode(); encoded != "" {
			reqURL += "?" + encoded
		}

		body, err := request(cmd, http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}
		if listJSON {
			return cliutil.WritePrettyJSON(cmd, body, "datasets")
		}

		var result listResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("datasets response was not valid JSON: %w", err)
		}
		renderDatasetList(cmd, result.Datasets)
		return nil
	},
}

func renderDatasetList(cmd *cobra.Command, rows []datasetState) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAMESPACE\tNAME\tSTATUS\tWATERMARK\tUPDATED\tREASON")
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			displayNamespace(row.Namespace),
			row.Name,
			row.Status,
			row.Watermark,
			formatTime(row.UpdatedAt),
			row.Reason,
		)
	}
	_ = w.Flush()
}

func displayNamespace(namespace string) string {
	if strings.TrimSpace(namespace) == "" {
		return "_"
	}
	return namespace
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func init() {
	listCmd.Flags().StringVar(&listStatus, "status", "", "Filter by dataset status")
	listCmd.Flags().BoolVar(&listJSON, "json", false, "Print JSON")
}

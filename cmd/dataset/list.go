package dataset

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

type listResponse struct {
	Datasets []datasetState `json:"datasets"`
	Total    int64          `json:"total"`
	Limit    int            `json:"limit"`
	Offset   int            `json:"offset"`
}

type datasetState struct {
	HoldStatus string    `json:"hold_status,omitempty"`
	Namespace  string    `json:"namespace,omitempty"`
	Name       string    `json:"name"`
	Watermark  string    `json:"watermark"`
	Status     string    `json:"status"`
	Reason     string    `json:"reason,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

var listCmd = newListCommand()

func newListCommand() *cobra.Command {
	var status string
	var jsonOutput bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List freshness dataset states",
		Long:  "List a page of dataset states, most recently updated first. Use --limit and --offset to inspect other pages; held datasets may appear on any page.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validatePagination(limit, offset); err != nil {
				return err
			}
			params := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
			if status := strings.TrimSpace(status); status != "" {
				params.Set("status", status)
			}
			body, err := request(cmd, http.MethodGet, serverBase()+"/v1/datasets?"+params.Encode(), nil)
			if err != nil {
				return err
			}
			if jsonOutput {
				return cliutil.WritePrettyJSON(cmd, body, "datasets")
			}

			var result listResponse
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Errorf("datasets response was not valid JSON: %w", err)
			}
			return renderDatasetList(cmd, result)
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Filter by dataset status")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum datasets per page (1-200)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Number of datasets to skip (most recently updated first)")
	return cmd
}

func renderDatasetList(cmd *cobra.Command, result listResponse) error {
	rows := result.Datasets
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	held := false
	for _, row := range rows {
		held = held || row.HoldStatus != ""
	}
	header := "NAMESPACE\tNAME\tSTATUS\tWATERMARK\tUPDATED\tREASON"
	if held {
		header += "\tHOLD"
	}
	_, _ = fmt.Fprintln(w, header)
	for _, row := range rows {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s",
			displayNamespace(row.Namespace),
			row.Name,
			row.Status,
			row.Watermark,
			formatTime(row.UpdatedAt),
			row.Reason,
		)
		if held {
			_, _ = fmt.Fprintf(w, "\t%s", row.HoldStatus)
		}
		_, _ = fmt.Fprintln(w)
	}
	renderPagination(w, "datasets", len(rows), result.Total, result.Limit, result.Offset)
	return w.Flush()
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

package dataset

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

type datasetHold struct {
	ID              string    `json:"id"`
	Namespace       string    `json:"namespace"`
	Name            string    `json:"name"`
	Status          string    `json:"status"`
	Reason          string    `json:"reason"`
	OccurrenceCount int       `json:"occurrence_count"`
	OpenedAt        time.Time `json:"opened_at"`
}

type holdsResponse struct {
	Holds  []datasetHold `json:"holds"`
	Total  int64         `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

var holdsCmd = newHoldsCommand()

func newHoldsCommand() *cobra.Command {
	var status string
	var jsonOutput bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "holds",
		Short: "List dataset holds",
		Long:  "List a page of dataset holds, newest first. Use --limit and --offset to inspect older holds; table output includes the page size and total.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > 200 {
				return fmt.Errorf("--limit must be between 1 and 200")
			}
			if offset < 0 {
				return fmt.Errorf("--offset must be non-negative")
			}
			params := url.Values{"status": {status}, "limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
			if cmd.Flags().Changed("namespace") {
				params.Set("namespace", namespaceFlag)
			}
			body, err := request(cmd, http.MethodGet, serverBase()+"/v1/datasets/holds?"+params.Encode(), nil)
			if err != nil {
				return err
			}
			if jsonOutput {
				return cliutil.WritePrettyJSON(cmd, body, "dataset holds")
			}
			var result holdsResponse
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Errorf("dataset holds response was not valid JSON: %w", err)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAMESPACE\tNAME\tSTATUS\tREASON\tOCCURRENCES\tOPENED\tHOLD ID")
			for _, hold := range result.Holds {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", displayNamespace(hold.Namespace), hold.Name, hold.Status, hold.Reason, hold.OccurrenceCount, formatTime(hold.OpenedAt), hold.ID)
			}
			_, _ = fmt.Fprintf(w, "\nShowing %d of %d holds (limit %d, offset %d).\n", len(result.Holds), result.Total, result.Limit, result.Offset)
			if next := result.Offset + len(result.Holds); int64(next) < result.Total && len(result.Holds) > 0 {
				_, _ = fmt.Fprintf(w, "More holds available; use --offset %d for the next page.\n", next)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&status, "status", "active", "Filter holds: active, released, or all")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum holds per page (1-200)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Number of holds to skip (newest first)")
	return cmd
}

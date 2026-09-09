package dataset

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var holdsStatus string
var holdsJSON bool

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

var holdsCmd = &cobra.Command{
	Use:   "holds",
	Short: "List dataset holds",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		params := url.Values{"status": {holdsStatus}}
		if cmd.Flags().Changed("namespace") {
			params.Set("namespace", namespaceFlag)
		}
		body, err := request(cmd, http.MethodGet, serverBase()+"/v1/datasets/holds?"+params.Encode(), nil)
		if err != nil {
			return err
		}
		if holdsJSON {
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
		return w.Flush()
	},
}

func init() {
	holdsCmd.Flags().StringVar(&holdsStatus, "status", "active", "Filter holds: active, released, or all")
	holdsCmd.Flags().BoolVar(&holdsJSON, "json", false, "Print JSON")
}

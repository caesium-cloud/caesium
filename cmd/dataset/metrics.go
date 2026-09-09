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

var metricsMetric string
var metricsJSON bool

var metricsCmd = &cobra.Command{
	Use:   "metrics <name> --metric <metric> [--namespace <ns>]",
	Short: "Show recent metric samples and the clean rolling baseline",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace, name, err := splitDatasetRef(args[0])
		if err != nil {
			return err
		}
		metric := strings.TrimSpace(metricsMetric)
		if metric == "" || metric == "dataset" {
			return fmt.Errorf("--metric must name an emitted metric (e.g. rowCount)")
		}
		body, err := request(cmd, http.MethodGet, serverBase()+datasetPath(namespace, name)+"/metrics?"+url.Values{"metric": {metric}}.Encode(), nil)
		if err != nil {
			return err
		}
		if metricsJSON {
			return cliutil.WritePrettyJSON(cmd, body, "dataset metrics")
		}
		var result struct {
			Baseline struct {
				Samples int     `json:"samples"`
				Median  float64 `json:"median"`
				P10     float64 `json:"p10"`
				P90     float64 `json:"p90"`
			} `json:"baseline"`
			Seeding bool `json:"seeding"`
			Series  []struct {
				Value     float64   `json:"value"`
				Violated  bool      `json:"violated"`
				CreatedAt time.Time `json:"created_at"`
			} `json:"series"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("dataset metrics response was not valid JSON: %w", err)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "DATASET\t%s/%s\nMETRIC\t%s\nCLEAN SAMPLES\t%d\nMEDIAN\t%g\nP10\t%g\nP90\t%g\nSEEDING\t%t\n", displayNamespace(namespace), name, metric, result.Baseline.Samples, result.Baseline.Median, result.Baseline.P10, result.Baseline.P90, result.Seeding)
		_, _ = fmt.Fprintln(w, "OBSERVED AT\tVALUE\tVIOLATED")
		for _, point := range result.Series {
			_, _ = fmt.Fprintf(w, "%s\t%g\t%t\n", formatTime(point.CreatedAt), point.Value, point.Violated)
		}
		return w.Flush()
	},
}

func init() {
	metricsCmd.Flags().StringVar(&metricsMetric, "metric", "", "Emitted metric key (required)")
	metricsCmd.Flags().BoolVar(&metricsJSON, "json", false, "Print JSON")
}

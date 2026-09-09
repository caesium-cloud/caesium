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

type metricsResponse struct {
	Baseline struct {
		Samples int     `json:"samples"`
		Median  float64 `json:"median"`
		P10     float64 `json:"p10"`
		P90     float64 `json:"p90"`
	} `json:"baseline"`
	Window  int   `json:"window"`
	Seeding bool  `json:"seeding"`
	Total   int64 `json:"total"`
	Limit   int   `json:"limit"`
	Offset  int   `json:"offset"`
	Series  []struct {
		Value      float64   `json:"value"`
		Violated   bool      `json:"violated"`
		InBaseline bool      `json:"in_baseline"`
		CreatedAt  time.Time `json:"created_at"`
	} `json:"series"`
}

var metricsCmd = newMetricsCommand()

func newMetricsCommand() *cobra.Command {
	var metric string
	var jsonOutput bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "metrics <name> --metric <metric> [--namespace <ns>]",
		Short: "Show metric samples and the clean rolling baseline",
		Long:  "Show a page of metric samples, oldest first within each page. Use --offset to reach older samples; pagination counts from the newest sample. The rolling baseline window is independent of the displayed page. IN BASELINE identifies samples contributing to that baseline.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, name, err := splitDatasetRef(args[0])
			if err != nil {
				return err
			}
			metric := strings.TrimSpace(metric)
			if metric == "" || metric == "dataset" {
				return fmt.Errorf("--metric must name an emitted metric (e.g. rowCount)")
			}
			if err := validatePagination(limit, offset); err != nil {
				return err
			}
			params := url.Values{"metric": {metric}, "limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
			body, err := request(cmd, http.MethodGet, serverBase()+datasetPath(namespace, name)+"/metrics?"+params.Encode(), nil)
			if err != nil {
				return err
			}
			if jsonOutput {
				return cliutil.WritePrettyJSON(cmd, body, "dataset metrics")
			}
			var result metricsResponse
			if err := json.Unmarshal(body, &result); err != nil {
				return fmt.Errorf("dataset metrics response was not valid JSON: %w", err)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintf(w, "DATASET\t%s/%s\nMETRIC\t%s\nBASELINE WINDOW\t%d\nCLEAN SAMPLES\t%d\nMEDIAN\t%g\nP10\t%g\nP90\t%g\nSEEDING\t%t\n", displayNamespace(namespace), name, metric, result.Window, result.Baseline.Samples, result.Baseline.Median, result.Baseline.P10, result.Baseline.P90, result.Seeding)
			_, _ = fmt.Fprintln(w, "OBSERVED AT\tVALUE\tVIOLATED\tIN BASELINE")
			for _, point := range result.Series {
				_, _ = fmt.Fprintf(w, "%s\t%g\t%t\t%t\n", formatTime(point.CreatedAt), point.Value, point.Violated, point.InBaseline)
			}
			renderPagination(w, "samples", len(result.Series), result.Total, result.Limit, result.Offset)
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&metric, "metric", "", "Emitted metric key (required)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum raw samples per page (1-200; independent of the baseline window)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Number of raw samples to skip (newest first)")
	return cmd
}

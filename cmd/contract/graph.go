package contract

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/spf13/cobra"
)

var (
	graphDataset string
	graphJSON    bool
)

var graphCmd = &cobra.Command{
	Use:   "graph [--dataset ns/name]",
	Short: "Fetch the contract graph from the server",
	RunE: func(cmd *cobra.Command, args []string) error {
		apiKey := cliutil.ResolveAPIKey(cmd, apiKeyFlag, cliutil.APIKeyEnvVar)
		if err := ensureContractEnforcementEnabled(cmd, apiKey); err != nil {
			return err
		}

		reqURL := serverBase() + "/v1/contracts/graph"
		if dataset := strings.TrimSpace(graphDataset); dataset != "" {
			params := url.Values{}
			params.Set("dataset", dataset)
			reqURL += "?" + params.Encode()
		}

		body, status, err := request(cmd, apiKey, http.MethodGet, reqURL, nil, "contract graph")
		if err != nil {
			return err
		}
		if status == http.StatusNotFound {
			return disabledError("GET /v1/contracts/graph is not registered")
		}
		if status >= http.StatusBadRequest {
			return fmt.Errorf("contract graph failed (%d): %s", status, strings.TrimSpace(string(body)))
		}
		if graphJSON {
			return cliutil.WritePrettyJSON(cmd, body, "contract graph")
		}

		var graph graphResponse
		if err := decodeJSON(body, &graph, "contract graph"); err != nil {
			return err
		}
		return renderGraph(cmd, graph)
	},
}

func renderGraph(cmd *cobra.Command, graph graphResponse) error {
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Contract graph: %d node(s), %d edge(s)\n", len(graph.Nodes), len(graph.Edges)); err != nil {
		return err
	}

	if len(graph.Nodes) > 0 {
		if _, err := fmt.Fprintln(out, "\nNodes:"); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "KIND\tID\tALIAS/DATASET\tLABELS")
		for _, node := range graph.Nodes {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
				cleanCell(node.Kind),
				cleanCell(node.ID),
				cleanCell(nodeDisplay(node)),
				cleanCell(formatLabels(node.Labels)),
			)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(graph.Edges) > 0 {
		if _, err := fmt.Fprintln(out, "\nEdges:"); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "FROM\tTO\tCLASS\tVERDICT\tDATASET\tFINDINGS")
		for _, edge := range graph.Edges {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				cleanCell(edge.From),
				cleanCell(edge.To),
				cleanCell(edge.Class),
				cleanCell(edge.Verdict),
				cleanCell(datasetName(edge.Dataset)),
				cleanCell(formatGraphFindings(edge.Findings)),
			)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func nodeDisplay(node graphNode) string {
	if node.Alias != "" {
		return node.Alias
	}
	return datasetName(node.Dataset)
}

func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+labels[key])
	}
	return strings.Join(parts, ",")
}

func formatGraphFindings(findings []graphFinding) string {
	if len(findings) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(findings))
	for _, finding := range findings {
		parts = append(parts, fmt.Sprintf("%s:%s %s %s",
			finding.Verdict,
			finding.Kind,
			finding.Path,
			finding.Detail,
		))
	}
	return strings.Join(parts, "; ")
}

func init() {
	graphCmd.Flags().StringVar(&graphDataset, "dataset", "", "Filter graph edges by dataset namespace/name")
	graphCmd.Flags().BoolVar(&graphJSON, "json", false, "Print the raw graph JSON")
}

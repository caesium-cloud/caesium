package dataset

import (
	"encoding/json"
	"fmt"
	"net/http"
	"text/tabwriter"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var statusJSON bool

type statusResponse struct {
	State        datasetState       `json:"state"`
	SLO          *datasetSLO        `json:"slo,omitempty"`
	Producing    *producingJob      `json:"producing_job,omitempty"`
	LastDecision *datasetDerivation `json:"last_decision,omitempty"`
	Declaration  any                `json:"declaration,omitempty"`
}

type datasetSLO struct {
	Freshness     string `json:"freshness,omitempty"`
	MaxStaleness  string `json:"max_staleness,omitempty"`
	ExpectedEvery string `json:"expected_every,omitempty"`
}

type producingJob struct {
	ID       uuid.UUID `json:"id"`
	Alias    string    `json:"alias"`
	StepName string    `json:"step_name,omitempty"`
}

type datasetDerivation struct {
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

var statusCmd = &cobra.Command{
	Use:   "status <name> [--namespace <ns>]",
	Short: "Show one freshness dataset state",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace, name, err := splitDatasetRef(args[0])
		if err != nil {
			return err
		}

		body, err := request(cmd, http.MethodGet, serverBase()+datasetPath(namespace, name), nil)
		if err != nil {
			return err
		}
		if statusJSON {
			return cliutil.WritePrettyJSON(cmd, body, "dataset status")
		}

		var result statusResponse
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("dataset status response was not valid JSON: %w", err)
		}
		renderDatasetStatus(cmd, result)
		return nil
	},
}

func renderDatasetStatus(cmd *cobra.Command, result statusResponse) {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "NAMESPACE\t%s\n", displayNamespace(result.State.Namespace))
	_, _ = fmt.Fprintf(w, "NAME\t%s\n", result.State.Name)
	_, _ = fmt.Fprintf(w, "STATUS\t%s\n", result.State.Status)
	_, _ = fmt.Fprintf(w, "WATERMARK\t%s\n", result.State.Watermark)
	_, _ = fmt.Fprintf(w, "UPDATED\t%s\n", formatTime(result.State.UpdatedAt))
	if result.State.Reason != "" {
		_, _ = fmt.Fprintf(w, "REASON\t%s\n", result.State.Reason)
	}
	if result.SLO != nil {
		if result.SLO.Freshness != "" {
			_, _ = fmt.Fprintf(w, "FRESHNESS\t%s\n", result.SLO.Freshness)
		}
		if result.SLO.MaxStaleness != "" {
			_, _ = fmt.Fprintf(w, "MAX STALENESS\t%s\n", result.SLO.MaxStaleness)
		}
		if result.SLO.ExpectedEvery != "" {
			_, _ = fmt.Fprintf(w, "EXPECTED EVERY\t%s\n", result.SLO.ExpectedEvery)
		}
	}
	if result.Producing != nil {
		_, _ = fmt.Fprintf(w, "PRODUCER\t%s\n", result.Producing.Alias)
		if result.Producing.StepName != "" {
			_, _ = fmt.Fprintf(w, "PRODUCER STEP\t%s\n", result.Producing.StepName)
		}
	}
	if result.LastDecision != nil {
		_, _ = fmt.Fprintf(w, "LAST DECISION\t%s\n", result.LastDecision.Decision)
		if result.LastDecision.Reason != "" {
			_, _ = fmt.Fprintf(w, "LAST DECISION REASON\t%s\n", result.LastDecision.Reason)
		}
		_, _ = fmt.Fprintf(w, "LAST DECISION AT\t%s\n", formatTime(result.LastDecision.CreatedAt))
	}
	_ = w.Flush()
}

func init() {
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Print JSON")
}

package contract

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/cmd/job"
	"github.com/spf13/cobra"
)

var (
	checkPaths []string
	checkJSON  bool
)

var checkCmd = &cobra.Command{
	Use:   "check --path jobs/",
	Short: "Check local job definitions against persisted contract state",
	RunE: func(cmd *cobra.Command, args []string) error {
		defs, err := job.LoadDefinitions(checkPaths)
		if err != nil {
			return err
		}
		if len(defs) == 0 {
			if checkJSON {
				return writeContractJSON(cmd, job.ServerContractSummary{
					Breaking: []job.ServerContractFinding{},
					Warnings: []job.ServerContractFinding{},
					Edges:    0,
				})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "No job definitions found.")
			return err
		}

		apiKey := cliutil.ResolveAPIKey(cmd, apiKeyFlag, cliutil.APIKeyEnvVar)
		if err := ensureContractEnforcementEnabled(cmd, apiKey); err != nil {
			return err
		}

		lintResp, _, err := job.PostServerLint(cmd.Context(), serverBase(), apiKey, defs)
		if err != nil {
			return err
		}
		if len(lintResp.Errors) > 0 {
			return fmt.Errorf("server lint failed: %s", lintErrorSummary(lintResp.Errors))
		}
		if lintResp.Contracts == nil {
			return disabledError("server lint did not include contract findings")
		}

		if checkJSON {
			if err := writeContractJSON(cmd, *lintResp.Contracts); err != nil {
				return err
			}
		} else if err := renderContractSummary(cmd, lintResp.Contracts); err != nil {
			return err
		}
		if len(lintResp.Contracts.Breaking) > 0 {
			return fmt.Errorf("contract check failed: %d breaking finding(s)", len(lintResp.Contracts.Breaking))
		}
		return nil
	},
}

func renderContractSummary(cmd *cobra.Command, summary *job.ServerContractSummary) error {
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Contract findings: %d edge(s), %d breaking, %d warning(s)\n", summary.Edges, len(summary.Breaking), len(summary.Warnings)); err != nil {
		return err
	}
	if len(summary.Breaking) > 0 {
		if _, err := fmt.Fprintln(out, "Breaking:"); err != nil {
			return err
		}
		if err := renderContractFindings(cmd, summary.Breaking); err != nil {
			return err
		}
	}
	if len(summary.Warnings) > 0 {
		if _, err := fmt.Fprintln(out, "Warnings:"); err != nil {
			return err
		}
		if err := renderContractFindings(cmd, summary.Warnings); err != nil {
			return err
		}
	}
	return nil
}

func renderContractFindings(cmd *cobra.Command, findings []job.ServerContractFinding) error {
	out := cmd.OutOrStdout()
	for _, finding := range findings {
		if _, err := fmt.Fprintf(out, "  - %s -> %s [%s] %s %s: %s\n",
			cleanCell(finding.From),
			cleanCell(finding.To),
			cleanCell(finding.EdgeClass),
			cleanCell(finding.Kind),
			cleanCell(finding.Path),
			cleanCell(finding.Detail),
		); err != nil {
			return err
		}
	}
	return nil
}

func writeContractJSON(cmd *cobra.Command, summary job.ServerContractSummary) error {
	body, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	return cliutil.WritePrettyJSON(cmd, body, "contract findings")
}

func lintErrorSummary(messages []job.ServerLintMessage) string {
	parts := make([]string, 0, len(messages))
	for _, msg := range messages {
		parts = append(parts, msg.Message)
	}
	return strings.Join(parts, "; ")
}

func init() {
	checkCmd.Flags().StringSliceVarP(&checkPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	checkCmd.Flags().BoolVar(&checkJSON, "json", false, "Print contract findings JSON")
}

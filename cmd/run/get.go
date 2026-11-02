package run

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var (
	getRunID  string
	getJobID  string
	getServer string
	getAPIKey string
	getAsJSON bool
)

var getCmd = &cobra.Command{
	Use:   "get --run-id <run-id> --job-id <job-id>",
	Short: "Print a run's status",
	Long: "Print a run's status, job alias or id, and task summary.\n\n" +
		"This is the status read; there is no separate `run status` command. " +
		"With --json, stdout is only the JSON document. " +
		"A failed or still-running run is printed and the command exits 0; " +
		"use `run wait` to exit non-zero on failure or timeout.",
	Args: cobra.NoArgs,
	RunE: runGet,
}

func runGet(cmd *cobra.Command, _ []string) error {
	if err := requireRunRef(getJobID, getRunID); err != nil {
		return err
	}
	cmd.SilenceUsage = true
	apiKey := resolveRunDiffAPIKey(cmd, getAPIKey)
	view, err := fetchRun(cmd, inspectHTTPClient, getServer, apiKey, getJobID, getRunID)
	if err != nil {
		return err
	}
	if getAsJSON {
		return writeRunJSON(cmd, view, false)
	}
	return writeGetHuman(cmd, view)
}

func writeGetHuman(cmd *cobra.Command, view *runView) error {
	if view == nil {
		view = &runView{}
	}
	summary := summarizeTasks(view.Tasks)
	status := view.Status
	if strings.TrimSpace(status) == "" {
		status = "unknown"
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "status: %s\njob: %s\ntasks: total=%d succeeded=%d failed=%d\n",
		status, jobLabel(view), summary.Total, summary.Succeeded, summary.Failed); err != nil {
		return err
	}
	if view.Error != "" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "error: %s\n", view.Error); err != nil {
			return err
		}
	}
	for _, task := range view.Tasks {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), formatTaskLine(task)); err != nil {
			return err
		}
	}
	return nil
}

func init() {
	getCmd.Flags().StringVar(&getRunID, "run-id", "", "Run ID to read (required)")
	getCmd.Flags().StringVar(&getJobID, "job-id", "", "Job ID that owns the run (required)")
	getCmd.Flags().StringVar(&getServer, "server", "http://localhost:8080", "Caesium server base URL")
	getCmd.Flags().StringVar(&getAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	getCmd.Flags().BoolVar(&getAsJSON, "json", false, "Print the run status as JSON on stdout")
	Cmd.AddCommand(getCmd)
}

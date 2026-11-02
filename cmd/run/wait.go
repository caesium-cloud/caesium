package run

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var (
	waitRunID   string
	waitJobID   string
	waitServer  string
	waitAPIKey  string
	waitJSON    bool
	waitTimeout = 5 * time.Minute

	// waitPollInterval is how long wait sleeps between GETs. Tests shorten it.
	waitPollInterval = 250 * time.Millisecond
)

var waitCmd = &cobra.Command{
	Use:   "wait --run-id <run-id> --job-id <job-id> [--timeout 30s]",
	Short: "Wait until a run is terminal",
	Long: "Poll a run until it is terminal (succeeded, failed, cancelled, or skipped).\n\n" +
		"Exit 0 when the run succeeded and no task failed. Exit non-zero when the run " +
		"or a task failed, the run was cancelled or skipped, or --timeout elapsed. " +
		"The terminal status (or the last status on timeout) is printed on stdout. " +
		"With --json, stdout is only that JSON document; the task summary is included. " +
		"The default timeout is 5m.",
	Args: cobra.NoArgs,
	RunE: runWait,
}

func runWait(cmd *cobra.Command, _ []string) error {
	if err := requireRunRef(waitJobID, waitRunID); err != nil {
		return err
	}
	cmd.SilenceUsage = true
	if waitTimeout <= 0 {
		return fmt.Errorf("--timeout must be greater than zero")
	}
	apiKey := resolveRunDiffAPIKey(cmd, waitAPIKey)

	deadline := time.NewTimer(waitTimeout)
	defer deadline.Stop()

	var last *runView
	next := time.Duration(0)
	for {
		timer := time.NewTimer(next)
		select {
		case <-cmd.Context().Done():
			timer.Stop()
			return cmd.Context().Err()
		case <-deadline.C:
			timer.Stop()
			return finishWait(cmd, last, true)
		case <-timer.C:
			view, err := fetchRun(cmd, inspectHTTPClient, waitServer, apiKey, waitJobID, waitRunID)
			if err != nil {
				return err
			}
			last = view
			if runStatusTerminal(view.Status) {
				return finishWait(cmd, view, false)
			}
			next = waitPollInterval
			if next <= 0 {
				next = 250 * time.Millisecond
			}
		}
	}
}

func finishWait(cmd *cobra.Command, view *runView, timedOut bool) error {
	if view == nil {
		view = &runView{}
	}
	if waitJSON {
		if err := writeRunJSON(cmd, view, timedOut); err != nil {
			return err
		}
	} else if err := writeWaitHuman(cmd, view); err != nil {
		return err
	}
	runID := firstNonEmpty(view.ID, waitRunID)
	if timedOut {
		return fmt.Errorf("timed out waiting for run %s (last status %s)", runID, displayStatus(view.Status))
	}
	if runSucceeded(view) {
		return nil
	}
	if view.Status == "succeeded" {
		return fmt.Errorf("run %s status %s but %d task(s) failed", runID, view.Status, summarizeTasks(view.Tasks).Failed)
	}
	return fmt.Errorf("run %s ended %s", runID, displayStatus(view.Status))
}

func writeWaitHuman(cmd *cobra.Command, view *runView) error {
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), displayStatus(view.Status)); err != nil {
		return err
	}
	summary := summarizeTasks(view.Tasks)
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "job: %s\ntasks: total=%d succeeded=%d failed=%d\n",
		jobLabel(view), summary.Total, summary.Succeeded, summary.Failed)
	return nil
}

func displayStatus(status string) string {
	if strings.TrimSpace(status) == "" {
		return "unknown"
	}
	return status
}

func init() {
	waitCmd.Flags().StringVar(&waitRunID, "run-id", "", "Run ID to wait for (required)")
	waitCmd.Flags().StringVar(&waitJobID, "job-id", "", "Job ID that owns the run (required)")
	waitCmd.Flags().DurationVar(&waitTimeout, "timeout", 5*time.Minute, "How long to poll before giving up")
	waitCmd.Flags().StringVar(&waitServer, "server", "http://localhost:8080", "Caesium server base URL")
	waitCmd.Flags().StringVar(&waitAPIKey, "api-key", "", "API key for authentication (prefer "+runDiffAPIKeyEnvVar+"; --api-key is visible in process listings)")
	waitCmd.Flags().BoolVar(&waitJSON, "json", false, "Print the terminal run status as JSON on stdout")
	Cmd.AddCommand(waitCmd)
}

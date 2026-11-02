package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// errWaitDeadline is the wait loop's own deadline, distinct from a cancelled
// command context and from the shared HTTP client's timeout.
var errWaitDeadline = errors.New("wait deadline exceeded")

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
	deadline := time.Now().Add(waitTimeout)

	var last *runView
	next := time.Duration(0)
	for {
		if err := sleepBeforePoll(cmd.Context(), next, deadline); err != nil {
			if errors.Is(err, errWaitDeadline) {
				return finishWait(cmd, last, true)
			}
			return err
		}
		// Remaining time until the original deadline, not a fresh client
		// timeout. The shared client's Timeout would otherwise let one GET
		// ignore --timeout and return success after the deadline.
		view, err := fetchRunBeforeDeadline(cmd, deadline, apiKey)
		if errors.Is(err, errWaitDeadline) {
			if view != nil {
				last = view
			}
			return finishWait(cmd, last, true)
		}
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

// sleepBeforePoll waits for the poll interval, but not past deadline.
func sleepBeforePoll(ctx context.Context, delay time.Duration, deadline time.Time) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return errWaitDeadline
	}
	if delay <= 0 {
		return nil
	}
	if delay > remaining {
		delay = remaining
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		if !time.Now().Before(deadline) {
			return errWaitDeadline
		}
		return nil
	}
}

// fetchRunBeforeDeadline cancels the GET when the wait deadline is reached.
// A body that arrives after that deadline is returned with errWaitDeadline
// so the caller prints it and still exits non-zero.
func fetchRunBeforeDeadline(cmd *cobra.Command, deadline time.Time, apiKey string) (*runView, error) {
	if !time.Now().Before(deadline) {
		return nil, errWaitDeadline
	}
	ctx, cancel := context.WithDeadline(cmd.Context(), deadline)
	defer cancel()
	view, err := fetchRunContext(ctx, inspectHTTPClient, waitServer, apiKey, waitJobID, waitRunID)
	if err != nil {
		if cmd.Context().Err() != nil {
			return nil, cmd.Context().Err()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || !time.Now().Before(deadline) {
			return nil, errWaitDeadline
		}
		return nil, err
	}
	if !time.Now().Before(deadline) {
		return view, errWaitDeadline
	}
	return view, nil
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

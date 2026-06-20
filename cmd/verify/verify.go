// Package verify is the `caesium verify <receipt>` CLI command: re-derive a
// committed reproducibility receipt against a run's current persisted state and
// flag drift.
package verify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	ireceipt "github.com/caesium-cloud/caesium/internal/receipt"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	verifyServer string
	verifyJSON   bool
)

// Cmd is `caesium verify <receipt-file>`.
var Cmd = &cobra.Command{
	Use:   "verify <receipt-file>",
	Short: "Verify a committed reproducibility receipt against a run's state",
	Long: "Re-derive the reproducibility receipt named by <receipt-file> from the " +
		"run's persisted state and report drift: a moved image tag (digest " +
		"mismatch), a changed manifest, a changed input. It does NOT resurrect " +
		"deleted source data — it re-derives the signature and proves what ran.\n\n" +
		"The job and run IDs are read from the receipt file. Exits non-zero when " +
		"the run drifted from the receipt OR cannot be soundly verified (it ran " +
		"on an unpinned, mutable image tag).",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		committed, err := loadReceipt(args[0])
		if err != nil {
			return err
		}
		if committed.RunID == uuid.Nil || committed.JobID == uuid.Nil {
			return fmt.Errorf("receipt %s has no run_id/job_id; is it a valid caesium receipt?", args[0])
		}

		server := strings.TrimSuffix(verifyServer, "/")
		url := fmt.Sprintf("%s/v1/jobs/%s/runs/%s/receipt/verify", server, committed.JobID, committed.RunID)

		payload, err := json.Marshal(committed)
		if err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= http.StatusBadRequest {
			return fmt.Errorf("verify failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}

		var result ireceipt.VerifyResult
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("decode verify response: %w", err)
		}

		if verifyJSON {
			pretty, mErr := json.MarshalIndent(&result, "", "  ")
			if mErr != nil {
				return mErr
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(pretty))
		} else {
			printHuman(cmd, &result)
		}

		// Non-zero exit when the run is not a clean, sound match so this command
		// is usable as a CI gate. SilenceUsage/SilenceErrors keep cobra from
		// printing usage on a legitimate drift exit.
		if !result.Match {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			if result.Degraded {
				return fmt.Errorf("UNVERIFIABLE: run ran on unpinned image tag(s); not reproducible")
			}
			return fmt.Errorf("DRIFT: run no longer matches the committed receipt")
		}
		return nil
	},
}

// loadReceipt reads and decodes a committed receipt from a file.
func loadReceipt(path string) (*ireceipt.Receipt, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read receipt %s: %w", path, err)
	}
	var r ireceipt.Receipt
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse receipt %s: %w", path, err)
	}
	return &r, nil
}

// printHuman renders a concise, honest verdict and any drift to stdout.
func printHuman(cmd *cobra.Command, result *ireceipt.VerifyResult) {
	out := cmd.OutOrStdout()
	switch {
	case result.Match:
		_, _ = fmt.Fprintf(out, "OK: run %s matches the committed receipt (digest %s)\n",
			result.RunID, shortDigest(result.ActualDigest))
	case result.Degraded:
		_, _ = fmt.Fprintf(out, "UNVERIFIABLE: run %s ran on unpinned image tag(s); the receipt cannot attest reproducibility.\n",
			result.RunID)
		if len(result.DegradedTasks) > 0 {
			_, _ = fmt.Fprintf(out, "  not digest-pinned: %s\n", strings.Join(result.DegradedTasks, ", "))
		}
	default:
		_, _ = fmt.Fprintf(out, "DRIFT: run %s no longer matches the committed receipt.\n", result.RunID)
		_, _ = fmt.Fprintf(out, "  expected digest: %s\n  actual digest:   %s\n",
			shortDigest(result.ExpectedDigest), shortDigest(result.ActualDigest))
	}

	for _, d := range result.Drifts {
		if d.Task != "" {
			_, _ = fmt.Fprintf(out, "  - [%s] task %q: %s\n", d.Kind, d.Task, d.Detail)
		} else {
			_, _ = fmt.Fprintf(out, "  - [%s] %s\n", d.Kind, d.Detail)
		}
		if d.Expected != "" || d.Actual != "" {
			_, _ = fmt.Fprintf(out, "      expected: %s\n      actual:   %s\n", d.Expected, d.Actual)
		}
	}
}

// shortDigest abbreviates a sha256 hex digest for human output, keeping the
// full value available in --json mode.
func shortDigest(d string) string {
	if len(d) <= 16 {
		return d
	}
	return d[:16] + "…"
}

func init() {
	Cmd.Flags().StringVar(&verifyServer, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.Flags().BoolVar(&verifyJSON, "json", false, "Emit the full machine-readable verify result as JSON")
}

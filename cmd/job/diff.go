package job

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/spf13/cobra"
)

const (
	defaultDiffServer    = "http://localhost:8080"
	jobDiffJSONVersion   = 1
	jobDiffWouldPruneHdr = "Would delete if --prune"
)

var (
	diffPaths  []string
	diffServer string
	diffAPIKey string
	diffJSON   bool
	diffPrune  bool

	diffHTTPClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}
)

var errJobDiffInScope = errors.New("in-scope job definition changes")

var diffCmd = &cobra.Command{
	Use:   "diff",
	Short: "Show changes between local job definitions and the server",
	Long: `Compare job definition manifests against a Caesium server via POST /v1/jobdefs/diff,
using the same --server / --api-key authentication as job apply.

By default the output is what a non-pruning apply would do: creates and updates
for jobs in --path. Jobs present on the server but missing from --path are prune
candidates. They are listed as deletes only with --prune; without --prune they
appear under "Would delete if --prune" and do not fail the command.

--json writes versioned JSON to stdout (logs stay on stderr).

Exit status:
  0  no in-scope changes (creates/updates, and deletes only when --prune is set)
  1  in-scope changes, or a parse / validation / request error`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true

		defs, err := collectDefinitions(diffPaths)
		if err != nil {
			return fmt.Errorf("load job definitions: %w", err)
		}
		if len(defs) == 0 {
			return errors.New("no job definitions selected")
		}

		server := strings.TrimSuffix(diffServer, "/")
		apiKey := cliutil.ResolveAPIKey(cmd, diffAPIKey, cliutil.APIKeyEnvVar)
		resp, err := sendDiffRequest(cmd.Context(), server, apiKey, defs)
		if err != nil {
			return err
		}

		scoped := scopeJobDiff(resp, diffPrune)
		if diffJSON {
			if err := writeJobDiffJSON(cmd, scoped); err != nil {
				return err
			}
		} else if err := renderJobDiff(cmd, scoped); err != nil {
			return err
		}

		if err := jobDiffInScopeError(scoped); err != nil {
			cmd.SilenceErrors = true
			return err
		}
		return nil
	},
}

func init() {
	diffCmd.Flags().StringSliceVarP(&diffPaths, "path", "p", nil, "Paths to job definition files or directories (default: current directory)")
	diffCmd.Flags().StringVar(&diffServer, "server", defaultDiffServer, "Caesium server base URL")
	diffCmd.Flags().StringVar(&diffAPIKey, "api-key", "", "API key for authentication (prefer "+cliutil.APIKeyEnvVar+"; --api-key is visible in process listings)")
	diffCmd.Flags().BoolVar(&diffJSON, "json", false, "Print versioned JSON to stdout")
	diffCmd.Flags().BoolVar(&diffPrune, "prune", false, "Treat server jobs missing from --path as in-scope deletes (matches job apply --prune)")
}

type jobDiffRequest struct {
	Definitions []schema.Definition `json:"definitions"`
}

type jobDiffResponse struct {
	Added    []json.RawMessage `json:"added"`
	Removed  []json.RawMessage `json:"removed"`
	Modified []json.RawMessage `json:"modified"`
}

type scopedJobDiff struct {
	Added      []json.RawMessage
	Modified   []json.RawMessage
	Removed    []json.RawMessage
	WouldPrune []json.RawMessage
}

type jobDiffJSON struct {
	Version    int               `json:"version"`
	Added      []json.RawMessage `json:"added"`
	Modified   []json.RawMessage `json:"modified"`
	Removed    []json.RawMessage `json:"removed"`
	WouldPrune []json.RawMessage `json:"wouldPrune,omitempty"`
}

type jobDiffAlias struct {
	Alias string `json:"alias"`
	Diff  string `json:"diff"`
}

func sendDiffRequest(ctx context.Context, server, apiKey string, defs []schema.Definition) (*jobDiffResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(jobDiffRequest{Definitions: defs})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/v1/jobdefs/diff", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := diffHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading job diff response: %w", err)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("job diff failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var diffResp jobDiffResponse
	if err := json.Unmarshal(body, &diffResp); err != nil {
		return nil, fmt.Errorf("job diff response was not valid JSON: %w", err)
	}
	return &diffResp, nil
}

func emptyRaw(items []json.RawMessage) []json.RawMessage {
	if items == nil {
		return []json.RawMessage{}
	}
	return items
}

func scopeJobDiff(resp *jobDiffResponse, prune bool) scopedJobDiff {
	if resp == nil {
		resp = &jobDiffResponse{}
	}
	out := scopedJobDiff{
		Added:    emptyRaw(resp.Added),
		Modified: emptyRaw(resp.Modified),
		Removed:  []json.RawMessage{},
	}
	removed := emptyRaw(resp.Removed)
	if prune {
		out.Removed = removed
	} else {
		out.WouldPrune = removed
	}
	return out
}

func (s scopedJobDiff) inScope() bool {
	return len(s.Added) > 0 || len(s.Modified) > 0 || len(s.Removed) > 0
}

func (s scopedJobDiff) jsonOutput() jobDiffJSON {
	out := jobDiffJSON{
		Version:  jobDiffJSONVersion,
		Added:    emptyRaw(s.Added),
		Modified: emptyRaw(s.Modified),
		Removed:  emptyRaw(s.Removed),
	}
	if len(s.WouldPrune) > 0 {
		out.WouldPrune = s.WouldPrune
	}
	return out
}

func jobDiffInScopeError(scoped scopedJobDiff) error {
	if !scoped.inScope() {
		return nil
	}
	var parts []string
	if n := len(scoped.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("%d to create", n))
	}
	if n := len(scoped.Modified); n > 0 {
		parts = append(parts, fmt.Sprintf("%d to update", n))
	}
	if n := len(scoped.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d to delete", n))
	}
	return fmt.Errorf("%w (%s)", errJobDiffInScope, strings.Join(parts, ", "))
}

func writeJobDiffJSON(cmd *cobra.Command, scoped scopedJobDiff) error {
	payload, err := json.MarshalIndent(scoped.jsonOutput(), "", "  ")
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), string(payload)); err != nil {
		return err
	}
	return nil
}

func renderJobDiff(cmd *cobra.Command, scoped scopedJobDiff) error {
	if !scoped.inScope() && len(scoped.WouldPrune) == 0 {
		return writeCmdOut(cmd, "No changes detected.\n")
	}

	wrote := false
	if !scoped.inScope() {
		if err := writeCmdOut(cmd, "No changes detected.\n"); err != nil {
			return err
		}
		wrote = true
	}

	if len(scoped.Added) > 0 {
		if err := writeCmdOut(cmd, "Creates:\n"); err != nil {
			return err
		}
		for _, spec := range sortedRawByAlias(scoped.Added) {
			if err := writeCmdOut(cmd, "  - %s\n", rawAlias(spec)); err != nil {
				return err
			}
		}
		if err := writeCmdOut(cmd, "\n"); err != nil {
			return err
		}
		wrote = true
	}

	if len(scoped.Modified) > 0 {
		if err := writeCmdOut(cmd, "Updates:\n"); err != nil {
			return err
		}
		for _, spec := range sortedRawByAlias(scoped.Modified) {
			alias, diffText := rawAlias(spec), rawDiff(spec)
			if err := writeCmdOut(cmd, "  - %s\n", alias); err != nil {
				return err
			}
			if strings.TrimSpace(diffText) != "" {
				if err := writeCmdOut(cmd, "%s\n", indent(diffText, "    ")); err != nil {
					return err
				}
			}
		}
		if err := writeCmdOut(cmd, "\n"); err != nil {
			return err
		}
		wrote = true
	}

	if len(scoped.Removed) > 0 {
		if err := writeCmdOut(cmd, "Deletes:\n"); err != nil {
			return err
		}
		for _, spec := range sortedRawByAlias(scoped.Removed) {
			if err := writeCmdOut(cmd, "  - %s\n", rawAlias(spec)); err != nil {
				return err
			}
		}
		wrote = true
	}

	if len(scoped.WouldPrune) > 0 {
		if wrote {
			if err := writeCmdOut(cmd, "\n"); err != nil {
				return err
			}
		}
		if err := writeCmdOut(cmd, "%s:\n", jobDiffWouldPruneHdr); err != nil {
			return err
		}
		for _, spec := range sortedRawByAlias(scoped.WouldPrune) {
			if err := writeCmdOut(cmd, "  - %s\n", rawAlias(spec)); err != nil {
				return err
			}
		}
	}
	return nil
}

func rawAlias(raw json.RawMessage) string {
	var spec jobDiffAlias
	if err := json.Unmarshal(raw, &spec); err != nil {
		return ""
	}
	return spec.Alias
}

func rawDiff(raw json.RawMessage) string {
	var spec jobDiffAlias
	if err := json.Unmarshal(raw, &spec); err != nil {
		return ""
	}
	return spec.Diff
}

func sortedRawByAlias(items []json.RawMessage) []json.RawMessage {
	out := append([]json.RawMessage(nil), items...)
	slices.SortFunc(out, func(a, b json.RawMessage) int {
		return cmp.Compare(rawAlias(a), rawAlias(b))
	})
	return out
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

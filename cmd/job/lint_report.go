package job

import (
	"errors"
	"fmt"
	"os"
	"sort"

	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	lintpkg "github.com/caesium-cloud/caesium/internal/jobdef/lint"
	"github.com/caesium-cloud/caesium/internal/offlinereport"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/spf13/cobra"
)

const (
	lintTriggerScopeNote = "Note: trigger-cycle lint is file-scoped; cross-job cycles against persisted triggers are validated at apply."
	lintRemediationNote  = "Note: metadata.remediation.profile / escalation.channel references are unverified offline; verified by server-side lint (POST /v1/jobdefs/lint) and at apply."
)

// runOfflineLint writes the version-1 report for `job lint --json` without
// --server. The server document path is unchanged. Stdout is only the report;
// human diagnostics go to stderr. A failing lint still prints the report and
// returns a nonzero error.
func runOfflineLint(cmd *cobra.Command) error {
	cmd.SilenceUsage = true
	paths := lintPaths
	if len(paths) == 0 {
		paths = []string{"."}
	}

	report := offlinereport.Report{
		Command: "lint",
		Scope: &offlinereport.Scope{
			Kind:  "lint",
			Paths: append([]string(nil), paths...),
		},
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError(p, err)...)
			continue
		}
		if !info.IsDir() && !internaljobdef.IsYAML(p) {
			report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
				Severity: offlinereport.SeverityError,
				File:     p,
				Message:  fmt.Sprintf("%s is not a YAML file", p),
			})
		}
	}

	files, err := internaljobdef.ResolveYAMLFiles(paths)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError("", err)...)
	} else {
		sort.Strings(files)
		report.Files = files
	}

	var valid []jobdef.Definition
	var aliases []string
	for _, file := range report.Files {
		found, ferr := internaljobdef.CollectDefinitions([]string{file}, false)
		if ferr != nil {
			report.Discovered++
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError(file, ferr)...)
			continue
		}
		report.Discovered += len(found)
		for i := range found {
			def := found[i]
			report.Selected++
			aliases = append(aliases, def.Metadata.Alias)
			if verr := def.Validate(); verr != nil {
				report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
					Severity: offlinereport.SeverityError,
					File:     file,
					Message:  verr.Error(),
				})
				report.Definitions = append(report.Definitions, offlinereport.Definition{
					Alias:  def.Metadata.Alias,
					File:   file,
					Passed: false,
					Error:  verr.Error(),
				})
				continue
			}
			if summary := contractSummary(def.Steps); summary != "" {
				report.Definitions = append(report.Definitions, offlinereport.Definition{
					Alias:     def.Metadata.Alias,
					File:      file,
					Passed:    true,
					Contracts: summary,
				})
			}
			valid = append(valid, def)
		}
	}
	report.Scope.Aliases = aliases
	report.Summary.Definitions = report.Selected
	report.Summary.Passed = len(valid)
	if failed := report.Discovered - len(valid); failed > 0 {
		report.Summary.Failed = failed
	}

	if report.Discovered == 0 && !lintHasError(report.Diagnostics) {
		report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
			Severity: offlinereport.SeverityError,
			Message:  "no job definitions selected",
		})
		report.Summary.Failed = 1
	}

	if len(valid) > 0 && !lintHasError(report.Diagnostics) {
		ctx := cmd.Context()
		if err := internaljobdef.ValidateTriggerChains(ctx, nil, valid); err != nil {
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError("", err)...)
		}
		if err := internaljobdef.ValidateDatasetGraph(ctx, nil, valid); err != nil {
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError("", err)...)
		}
		if lintCheckSecrets {
			resolver, rerr := buildLintResolver()
			if rerr != nil {
				report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError("", rerr)...)
			} else {
				for _, msg := range lintpkg.CheckSecrets(ctx, resolver, valid) {
					report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
						Severity: offlinereport.SeverityError,
						Message:  msg,
					})
				}
			}
		}
	}

	for _, msg := range lintpkg.CheckVolumeWriters(valid) {
		report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
			Severity: offlinereport.SeverityWarning,
			Message:  msg,
		})
	}

	if len(valid) > 0 && !lintHasError(report.Diagnostics) {
		report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
			Severity: offlinereport.SeverityNote,
			Message:  lintTriggerScopeNote,
		})
		for i := range valid {
			if valid[i].Metadata.Remediation != nil {
				report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
					Severity: offlinereport.SeverityNote,
					Message:  lintRemediationNote,
				})
				break
			}
		}
	}

	return emitOfflineReport(cmd, &report, errors.New("lint failed"))
}

func lintHasError(diagnostics []offlinereport.Diagnostic) bool {
	for _, d := range diagnostics {
		if d.Severity != offlinereport.SeverityWarning && d.Severity != offlinereport.SeverityNote {
			return true
		}
	}
	return false
}

func emitOfflineReport(cmd *cobra.Command, report *offlinereport.Report, failure error) error {
	if err := offlinereport.Write(cmd.OutOrStdout(), report); err != nil {
		return err
	}
	if err := offlinereport.WriteHumanDiagnostics(cmd.ErrOrStderr(), report.Diagnostics); err != nil {
		return err
	}
	if !report.OK {
		return failure
	}
	return nil
}

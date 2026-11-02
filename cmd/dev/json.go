package dev

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/localrun"
	"github.com/caesium-cloud/caesium/internal/offlinereport"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

type loadedDef struct {
	file string
	def  schema.Definition
}

// runDevJSON prints one version-1 execution report for `dev --once --json`.
// Watch mode is rejected before this runs. Prose status and logs stay off stdout.
func runDevJSON(ctx context.Context, cmd *cobra.Command, paths []string) error {
	cmd.SilenceUsage = true
	report := offlinereport.Report{
		Command: "dev",
		Scope: &offlinereport.Scope{
			Kind:  "dev",
			Paths: append([]string(nil), paths...),
		},
	}

	for _, p := range paths {
		info, statErr := os.Stat(p)
		if statErr != nil {
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError(p, statErr)...)
			continue
		}
		if !info.IsDir() && !jobdef.IsYAML(p) {
			report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
				Severity: offlinereport.SeverityError,
				File:     p,
				Message:  fmt.Sprintf("%s is not a YAML file", p),
			})
		}
	}

	files, err := jobdef.ResolveYAMLFiles(paths)
	if err != nil {
		report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError("", err)...)
	} else {
		report.Files = files
	}

	var runnable []loadedDef
	for _, file := range report.Files {
		found, ferr := jobdef.CollectDefinitions([]string{file}, true)
		if ferr != nil {
			report.Discovered++
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError(file, ferr)...)
			continue
		}
		report.Discovered += len(found)
		report.Selected += len(found)
		for i := range found {
			runnable = append(runnable, loadedDef{file: file, def: found[i]})
			report.Scope.Aliases = append(report.Scope.Aliases, found[i].Metadata.Alias)
		}
	}
	report.Summary.Definitions = report.Selected

	if report.Discovered == 0 && !hasReportError(report.Diagnostics) {
		report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
			Severity: offlinereport.SeverityError,
			Message:  "no job definitions selected",
		})
		report.Summary.Failed = 1
	}

	if hasReportError(report.Diagnostics) {
		if report.Summary.Failed == 0 {
			report.Summary.Failed = 1
		}
		return emitDevReport(cmd, &report)
	}

	runner := localrun.New(localrun.Config{
		MaxParallel:   maxParallel,
		TaskTimeout:   taskTimeout,
		RunTimeout:    runTimeout,
		Params:        devParams,
		EngineFactory: testEngineFactory,
	})

	for i := range runnable {
		def := &runnable[i].def
		report.Scope.Executed++
		result, runErr := runner.RunWithResult(ctx, def)
		runReport := devRunReport(def.Metadata.Alias, result, runErr)
		report.Runs = append(report.Runs, runReport)
		report.Summary.Tasks += len(runReport.Tasks)
		if runErr != nil || (result != nil && result.Status != "succeeded") {
			report.Summary.Failed++
			message := "run failed"
			if runErr != nil {
				message = runErr.Error()
			} else if result != nil && result.Error != "" {
				message = result.Error
			}
			report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
				Severity: offlinereport.SeverityError,
				File:     runnable[i].file,
				Message:  def.Metadata.Alias + ": " + message,
			})
			// Match dev's text path: a failed definition stops the batch.
			break
		}
		report.Summary.Passed++
	}

	return emitDevReport(cmd, &report)
}

func devRunReport(alias string, result *localrun.RunResult, runErr error) offlinereport.Run {
	runReport := offlinereport.Run{Alias: alias}
	if result == nil {
		if runErr != nil {
			runReport.Error = runErr.Error()
			runReport.Status = "failed"
		}
		return runReport
	}
	if result.RunID != uuid.Nil {
		runReport.RunID = result.RunID.String()
	}
	if result.JobID != uuid.Nil {
		runReport.JobID = result.JobID.String()
	}
	if result.Alias != "" {
		runReport.Alias = result.Alias
	}
	runReport.Status = result.Status
	runReport.Error = result.Error
	if runErr != nil && runReport.Error == "" {
		runReport.Error = runErr.Error()
	}
	runReport.Tasks = taskReports(result.Tasks)
	return runReport
}

func taskReports(tasks []localrun.TaskResult) []offlinereport.Task {
	if len(tasks) == 0 {
		return nil
	}
	out := make([]offlinereport.Task, 0, len(tasks))
	for _, task := range tasks {
		item := offlinereport.Task{
			Name:             task.Name,
			Partition:        task.Partition,
			Status:           task.Status,
			Attempt:          task.Attempt,
			ExitCode:         task.ExitCode,
			Output:           task.Output,
			SchemaViolations: task.SchemaViolations,
			CacheHit:         task.CacheHit,
			Error:            task.Error,
			Log:              task.LogText,
			LogTruncated:     task.LogTruncated,
			LogScrubbed:      task.LogScrubbed,
		}
		if task.TaskID != uuid.Nil {
			item.TaskID = task.TaskID.String()
		}
		out = append(out, item)
	}
	return out
}

func emitDevReport(cmd *cobra.Command, report *offlinereport.Report) error {
	if err := offlinereport.Write(cmd.OutOrStdout(), report); err != nil {
		return err
	}
	if err := offlinereport.WriteHumanDiagnostics(cmd.ErrOrStderr(), report.Diagnostics); err != nil {
		return err
	}
	if !report.OK {
		return errors.New("run failed")
	}
	return nil
}

func hasReportError(diagnostics []offlinereport.Diagnostic) bool {
	for _, d := range diagnostics {
		if d.Severity != offlinereport.SeverityWarning && d.Severity != offlinereport.SeverityNote {
			return true
		}
	}
	return false
}

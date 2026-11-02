package test

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/caesium-cloud/caesium/internal/dag"
	"github.com/caesium-cloud/caesium/internal/harness"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/localrun"
	"github.com/caesium-cloud/caesium/internal/offlinereport"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

func runSchemaJSON(cmd *cobra.Command) error {
	cmd.SilenceUsage = true
	paths := testPaths
	if len(paths) == 0 {
		paths = []string{"."}
	}
	report := offlinereport.Report{
		Command: "test",
		Scope: &offlinereport.Scope{
			Kind:  "schema",
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
		sort.Strings(files)
		report.Files = files
	}

	var parsed []schema.Definition
	for _, file := range report.Files {
		found, ferr := jobdef.CollectDefinitions([]string{file}, false)
		if ferr != nil {
			report.Discovered++
			report.Summary.Failed++
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError(file, ferr)...)
			continue
		}
		report.Discovered += len(found)
		for i := range found {
			def := found[i]
			report.Selected++
			parsed = append(parsed, def)
			report.Scope.Aliases = append(report.Scope.Aliases, def.Metadata.Alias)
			outcome := offlinereport.Definition{Alias: def.Metadata.Alias, File: file}
			if verr := def.Validate(); verr != nil {
				outcome.Passed = false
				outcome.Error = verr.Error()
				report.Summary.Failed++
				report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
					Severity: offlinereport.SeverityError,
					File:     file,
					Message:  def.Metadata.Alias + ": " + verr.Error(),
				})
				report.Definitions = append(report.Definitions, outcome)
				continue
			}
			analysis, aerr := dag.Analyze(&def)
			if aerr != nil {
				outcome.Passed = false
				outcome.Error = aerr.Error()
				report.Summary.Failed++
				report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
					Severity: offlinereport.SeverityError,
					File:     file,
					Message:  def.Metadata.Alias + ": DAG analysis error: " + aerr.Error(),
				})
				report.Definitions = append(report.Definitions, outcome)
				continue
			}
			outcome.Passed = true
			outcome.Steps = len(analysis.Steps)
			outcome.MaxParallelism = analysis.MaxParallelism
			outcome.ExecutionOrder = formatExecutionOrder(analysis.ExecutionOrder)
			report.Summary.Passed++
			report.Definitions = append(report.Definitions, outcome)
		}
	}
	report.Summary.Definitions = report.Selected

	if report.Discovered == 0 && !reportHasError(report.Diagnostics) {
		report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
			Severity: offlinereport.SeverityError,
			Message:  "no job definitions selected",
		})
		report.Summary.Failed = 1
	}

	if checkImages && len(parsed) > 0 {
		appendImageFindings(cmd, &report, parsed)
	}

	return emitTestReport(cmd, &report)
}

func appendImageFindings(cmd *cobra.Command, report *offlinereport.Report, defs []schema.Definition) {
	targets := imageCheckTargets(defs)
	dockerImages := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.usesDocker() {
			dockerImages = append(dockerImages, target.image)
		}
	}
	results := make(map[string]imagecheck.Result, len(dockerImages))
	if len(dockerImages) > 0 {
		for _, result := range imagecheck.Check(cmd.Context(), dockerImages) {
			results[result.Image] = result
		}
	}
	for _, target := range targets {
		finding := offlinereport.Image{Image: target.image}
		switch {
		case !target.usesDocker():
			finding.Status = "advisory"
			finding.Detail = target.availabilityDetail("no local Docker daemon probe")
		default:
			r := results[target.image]
			switch {
			case r.Error != nil:
				finding.Status = "fail"
				finding.Detail = target.availabilityDetail(fmt.Sprintf("local Docker daemon error: %v", r.Error))
				report.Summary.Failed++
				report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
					Severity: offlinereport.SeverityError,
					Message:  finding.Image + ": " + finding.Detail,
				})
			case r.Available:
				finding.Status = "pass"
				finding.Detail = target.availabilityDetail("available in local Docker daemon")
			default:
				finding.Status = "miss"
				finding.Detail = target.availabilityDetail("not found in local Docker daemon")
				report.Summary.Failed++
				report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
					Severity: offlinereport.SeverityError,
					Message:  finding.Image + ": " + finding.Detail,
				})
			}
		}
		report.Images = append(report.Images, finding)
	}
}

func runScenariosJSON(cmd *cobra.Command) error {
	cmd.SilenceUsage = true
	paths := scenarioPaths
	report := offlinereport.Report{
		Command: "test",
		Scope: &offlinereport.Scope{
			Kind:  "scenario",
			Paths: append([]string(nil), paths...),
		},
	}

	var scenarios []harness.ResolvedScenario
	seenFiles := make(map[string]struct{})
	addFile := func(path string) {
		if path == "" {
			return
		}
		if _, ok := seenFiles[path]; ok {
			return
		}
		seenFiles[path] = struct{}{}
		report.Files = append(report.Files, path)
	}
	for _, path := range paths {
		found, err := harness.CollectScenarios([]string{path})
		if err != nil {
			addFile(path)
			report.Discovered++
			report.Summary.Failed++
			report.Diagnostics = append(report.Diagnostics, offlinereport.DiagnosticsFromError(path, err)...)
			continue
		}
		report.Discovered += len(found)
		for _, scenario := range found {
			addFile(scenario.SourcePath)
		}
		scenarios = append(scenarios, found...)
	}
	sort.Strings(report.Files)
	report.Selected = len(scenarios)
	report.Summary.Scenarios = len(scenarios)

	if len(scenarios) == 0 && !reportHasError(report.Diagnostics) {
		report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
			Severity: offlinereport.SeverityError,
			Message:  "no harness scenarios selected",
		})
		report.Summary.Failed = 1
	}

	for _, scenario := range scenarios {
		report.Scope.Scenarios = append(report.Scope.Scenarios, scenario.Scenario.Name)
		report.Scope.Executed++
		item := offlinereport.Scenario{
			Name:   scenario.Scenario.Name,
			Source: scenario.SourcePath,
			Path:   scenario.Scenario.Path,
			Alias:  scenario.Scenario.Alias,
		}
		result, err := harness.Execute(cmd.Context(), scenario)
		if err != nil || result == nil {
			item.Passed = false
			if err != nil {
				item.Error = err.Error()
			} else {
				item.Error = "scenario did not produce a result"
			}
			report.Summary.Failed++
			report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
				Severity: offlinereport.SeverityError,
				File:     scenario.SourcePath,
				Message:  scenario.Scenario.Name + ": " + item.Error,
			})
			report.Scenarios = append(report.Scenarios, item)
			continue
		}
		item.Passed = result.Passed()
		if result.ExecutionError != nil {
			item.Error = result.ExecutionError.Error()
		}
		if result.Run != nil {
			item.Run = scenarioRun(result)
			item.Alias = firstNonEmpty(item.Alias, result.Run.Alias)
			report.Summary.Tasks += len(result.Run.Tasks)
		}
		for _, assertion := range result.Assertions {
			item.Assertions = append(item.Assertions, offlinereport.Assertion{
				Name:     assertion.Name,
				Passed:   assertion.Passed,
				Expected: assertion.Expected,
				Actual:   assertion.Actual,
			})
		}
		if !item.Passed {
			report.Summary.Failed++
			message := scenario.Scenario.Name
			if len(result.Failures) > 0 {
				message = scenario.Scenario.Name + ": " + result.Failures[0]
			} else if item.Error != "" {
				message = scenario.Scenario.Name + ": " + item.Error
			}
			report.Diagnostics = append(report.Diagnostics, offlinereport.Diagnostic{
				Severity: offlinereport.SeverityError,
				File:     scenario.SourcePath,
				Message:  message,
			})
		} else {
			report.Summary.Passed++
		}
		report.Scenarios = append(report.Scenarios, item)
	}

	return emitTestReport(cmd, &report)
}

func scenarioRun(result *harness.Result) *offlinereport.Run {
	run := result.Run
	out := &offlinereport.Run{
		Alias:  run.Alias,
		Status: run.Status,
		Error:  run.Error,
		Tasks:  testTaskReports(run.Tasks),
	}
	if run.RunID != uuid.Nil {
		out.RunID = run.RunID.String()
	}
	if run.JobID != uuid.Nil {
		out.JobID = run.JobID.String()
	}
	if result.ExecutionError != nil && out.Error == "" {
		out.Error = result.ExecutionError.Error()
	}
	return out
}

func testTaskReports(tasks []localrun.TaskResult) []offlinereport.Task {
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

func emitTestReport(cmd *cobra.Command, report *offlinereport.Report) error {
	if err := offlinereport.Write(cmd.OutOrStdout(), report); err != nil {
		return err
	}
	if err := offlinereport.WriteHumanDiagnostics(cmd.ErrOrStderr(), report.Diagnostics); err != nil {
		return err
	}
	if !report.OK {
		return errors.New("one or more checks failed")
	}
	return nil
}

func reportHasError(diagnostics []offlinereport.Diagnostic) bool {
	for _, d := range diagnostics {
		if d.Severity != offlinereport.SeverityWarning && d.Severity != offlinereport.SeverityNote {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

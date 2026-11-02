// Package offlinereport is the versioned JSON document written by offline
// `job lint`, `test`, and `dev --once`. Server lint keeps its own document.
package offlinereport

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
)

const Version = 1

const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityNote    = "note"
)

// Report is the offline validation and execution document. Version is 1.
type Report struct {
	Version     int          `json:"version"`
	Command     string       `json:"command"`
	OK          bool         `json:"ok"`
	Files       []string     `json:"files"`
	Discovered  int          `json:"discovered"`
	Selected    int          `json:"selected"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Summary     Summary      `json:"summary"`
	Scope       *Scope       `json:"scope,omitempty"`
	Definitions []Definition `json:"definitions,omitempty"`
	Scenarios   []Scenario   `json:"scenarios,omitempty"`
	Runs        []Run        `json:"runs,omitempty"`
	Images      []Image      `json:"images,omitempty"`
}

// Diagnostic is one lint, parse, or execution finding.
type Diagnostic struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Summary counts findings and outcomes. Errors and warnings are always present.
type Summary struct {
	Errors      int `json:"errors"`
	Warnings    int `json:"warnings"`
	Passed      int `json:"passed"`
	Failed      int `json:"failed"`
	Definitions int `json:"definitions,omitempty"`
	Scenarios   int `json:"scenarios,omitempty"`
	Tasks       int `json:"tasks,omitempty"`
}

// Scope says what the command considered and what it actually ran.
type Scope struct {
	Kind      string   `json:"kind"`
	Paths     []string `json:"paths,omitempty"`
	Executed  int      `json:"executed"`
	Aliases   []string `json:"aliases,omitempty"`
	Scenarios []string `json:"scenarios,omitempty"`
}

// Definition is one schema or lint outcome.
type Definition struct {
	Alias          string   `json:"alias,omitempty"`
	File           string   `json:"file,omitempty"`
	Passed         bool     `json:"passed"`
	Error          string   `json:"error,omitempty"`
	Steps          int      `json:"steps,omitempty"`
	MaxParallelism int      `json:"maxParallelism,omitempty"`
	ExecutionOrder []string `json:"executionOrder,omitempty"`
	Contracts      string   `json:"contracts,omitempty"`
}

// Scenario is one harness case.
type Scenario struct {
	Name       string      `json:"name"`
	Source     string      `json:"source,omitempty"`
	Path       string      `json:"path,omitempty"`
	Alias      string      `json:"alias,omitempty"`
	Passed     bool        `json:"passed"`
	Error      string      `json:"error,omitempty"`
	Run        *Run        `json:"run,omitempty"`
	Assertions []Assertion `json:"assertions,omitempty"`
}

// Assertion is one expectation. Failed assertions always carry expected and actual.
type Assertion struct {
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	Expected any    `json:"expected"`
	Actual   any    `json:"actual"`
}

// Run is one local execution.
type Run struct {
	Alias  string `json:"alias,omitempty"`
	RunID  string `json:"runId,omitempty"`
	JobID  string `json:"jobId,omitempty"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
	Tasks  []Task `json:"tasks,omitempty"`
}

// Task is one task outcome from the local runner.
type Task struct {
	TaskID           string                    `json:"taskId,omitempty"`
	Name             string                    `json:"name"`
	Partition        string                    `json:"partition,omitempty"`
	Status           string                    `json:"status,omitempty"`
	Attempt          int                       `json:"attempt,omitempty"`
	ExitCode         *int                      `json:"exitCode,omitempty"`
	Output           map[string]string         `json:"output,omitempty"`
	SchemaViolations []pkgtask.SchemaViolation `json:"schemaViolations,omitempty"`
	CacheHit         bool                      `json:"cacheHit"`
	Error            string                    `json:"error,omitempty"`
	Log              string                    `json:"log,omitempty"`
	LogTruncated     bool                      `json:"logTruncated,omitempty"`
	LogScrubbed      bool                      `json:"logScrubbed,omitempty"`
}

// Image is one local image-availability finding from `caesium test --check-images`.
type Image struct {
	Image  string `json:"image"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Finalize sets version, nil-slice defaults, error and warning counts, and ok.
// Callers set passed, failed, and the other summary counters first. A warning
// or note does not fail the report.
func (r *Report) Finalize() {
	if r.Version == 0 {
		r.Version = Version
	}
	if r.Files == nil {
		r.Files = []string{}
	}
	if r.Diagnostics == nil {
		r.Diagnostics = []Diagnostic{}
	}
	errors, warnings := 0, 0
	for _, d := range r.Diagnostics {
		switch d.Severity {
		case SeverityWarning:
			warnings++
		case SeverityNote:
		default:
			errors++
		}
	}
	r.Summary.Errors = errors
	r.Summary.Warnings = warnings
	if r.Summary.Failed == 0 && errors > 0 {
		r.Summary.Failed = errors
	}
	r.OK = errors == 0 && r.Summary.Failed == 0
}

// Write encodes the report as one indented JSON document plus a trailing newline.
func Write(w io.Writer, report *Report) error {
	report.Finalize()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(report)
}

// WriteHumanDiagnostics prints error and warning findings to w, one per line.
// Notes stay in the JSON document only. The JSON document itself is never written.
func WriteHumanDiagnostics(w io.Writer, diagnostics []Diagnostic) error {
	for _, d := range diagnostics {
		if d.Severity == SeverityNote {
			continue
		}
		if _, err := fmt.Fprintln(w, d.Human()); err != nil {
			return err
		}
	}
	return nil
}

// Human is a single-line location prefix plus the message, for stderr.
func (d Diagnostic) Human() string {
	var loc []string
	switch {
	case d.File != "" && d.Line > 0:
		loc = append(loc, fmt.Sprintf("%s:%d", d.File, d.Line))
	case d.File != "":
		loc = append(loc, d.File)
	case d.Line > 0:
		loc = append(loc, fmt.Sprintf("line %d", d.Line))
	}
	if d.Path != "" {
		loc = append(loc, d.Path)
	}
	msg := d.Message
	if d.Severity != "" && d.Severity != SeverityError {
		msg = d.Severity + ": " + msg
	}
	if len(loc) == 0 {
		return msg
	}
	return strings.Join(loc, ": ") + ": " + msg
}

var (
	yamlPathLine = regexp.MustCompile(`YAML path (\S+) \(line (\d+)\): (.*)$`)
	yamlLineOnly = regexp.MustCompile(`^line (\d+):\s*(.*)$`)
)

// DiagnosticsFromError splits a manifest error into located diagnostics.
// file is attached when the caller knows the source; a leading "file: " prefix
// is removed from fallback text so the path is not repeated in the message.
func DiagnosticsFromError(file string, err error) []Diagnostic {
	if err == nil {
		return nil
	}
	text := err.Error()
	body := text
	if file != "" {
		body = strings.TrimPrefix(text, file+": ")
	}
	var found []Diagnostic
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "unknown YAML fields:" {
			continue
		}
		if m := yamlPathLine.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[2])
			found = append(found, Diagnostic{
				Severity: SeverityError,
				File:     file,
				Path:     m[1],
				Line:     n,
				Message:  m[3],
			})
			continue
		}
		if m := yamlLineOnly.FindStringSubmatch(line); m != nil {
			n, _ := strconv.Atoi(m[1])
			found = append(found, Diagnostic{
				Severity: SeverityError,
				File:     file,
				Line:     n,
				Message:  m[2],
			})
		}
	}
	if len(found) > 0 {
		return found
	}
	return []Diagnostic{{
		Severity: SeverityError,
		File:     file,
		Message:  body,
	}}
}

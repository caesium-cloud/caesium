package reproduce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	pkgjobdef "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
)

// Puller pulls the image reference selected for local reproduction.
type Puller interface {
	Pull(ctx context.Context, imageRef, platform string) error
}

// Runner executes a synthesized one-step definition through the local runtime.
type Runner interface {
	Run(ctx context.Context, def *pkgjobdef.Definition, taskTimeout time.Duration) (*RunResult, error)
}

// RunResult is the localrun result subset used by this package.
type RunResult struct {
	Status string
	Error  string
	Tasks  []TaskResult
}

// TaskResult is the task-level localrun result subset used by this package.
type TaskResult struct {
	Name         string
	Status       string
	Output       map[string]string
	LogText      string
	LogTruncated bool
	Error        string
}

// ExecuteOptions controls local execution.
type ExecuteOptions struct {
	Puller   Puller
	Runner   Runner
	Timeout  time.Duration
	Platform string
}

// ExecutionResult is the machine-readable result for run mode.
type ExecutionResult struct {
	Status       string            `json:"status"`
	ExitCode     int               `json:"exit_code"`
	Task         string            `json:"task"`
	Image        string            `json:"image"`
	Output       map[string]string `json:"output,omitempty"`
	Log          string            `json:"log,omitempty"`
	LogTruncated bool              `json:"log_truncated,omitempty"`
	Error        string            `json:"error,omitempty"`
	Envelope     *Envelope         `json:"envelope"`
	Warnings     []Warning         `json:"warnings,omitempty"`
}

// PullError is returned when the selected image cannot be pulled.
type PullError struct {
	ImageRef string
	Registry string
	Err      error
}

func (e *PullError) Error() string {
	return fmt.Sprintf(
		"pull image %s from registry %s failed: %v; authenticate with docker login %s or use --image <local-ref> when the image is available locally",
		e.ImageRef,
		e.Registry,
		e.Err,
		e.Registry,
	)
}

func (e *PullError) Unwrap() error {
	return e.Err
}

// Execute pulls the reconstructed image and runs the synthesized local task
// exactly once.
func Execute(ctx context.Context, desc *Descriptor, env *Envelope, opts ExecuteOptions) (*ExecutionResult, error) {
	if desc == nil {
		return nil, fmt.Errorf("descriptor is required")
	}
	if env == nil {
		return nil, fmt.Errorf("envelope is required")
	}
	if opts.Puller == nil {
		return nil, fmt.Errorf("image puller is required")
	}
	if opts.Runner == nil {
		return nil, fmt.Errorf("local runner is required")
	}

	platform := firstNonEmpty(strings.TrimSpace(opts.Platform), env.Platform)
	if err := opts.Puller.Pull(ctx, env.Image, platform); err != nil {
		return nil, &PullError{
			ImageRef: env.Image,
			Registry: RegistryHost(env.Image),
			Err:      err,
		}
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = desc.Timing.TaskTimeout
	}
	def := BuildDefinition(desc, env, timeout)
	runResult, runErr := opts.Runner.Run(ctx, def, timeout)
	if runResult == nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, errors.New("local runner returned no result")
	}

	task := taskResult(runResult, env.TaskName)
	if task == nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, fmt.Errorf("local runner returned no result for task %q", env.TaskName)
	}

	output := task.Output
	if strings.TrimSpace(task.LogText) != "" {
		markers, err := pkgtask.ParseMarkers(strings.NewReader(task.LogText))
		if err != nil {
			return nil, err
		}
		if markers != nil && len(markers.Output) > 0 {
			output = markers.Output
		}
	}

	result := &ExecutionResult{
		Status:       task.Status,
		Task:         env.TaskName,
		Image:        env.Image,
		Output:       output,
		Log:          task.LogText,
		LogTruncated: task.LogTruncated,
		Error:        firstNonEmpty(task.Error, runResult.Error),
		Envelope:     env,
		Warnings:     env.Warnings,
	}
	if isSuccessfulStatus(task.Status) {
		result.ExitCode = 0
		return result, nil
	}
	result.ExitCode = 1
	if result.Error == "" && runErr != nil {
		result.Error = runErr.Error()
	}
	return result, nil
}

// RegistryHost returns the registry host component used in pull guidance.
func RegistryHost(imageRef string) string {
	ref := imageRef
	if before, _, ok := strings.Cut(ref, "@"); ok {
		ref = before
	}
	first := ref
	if slash := strings.IndexByte(ref, '/'); slash >= 0 {
		first = ref[:slash]
	} else {
		return "docker.io"
	}
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first
	}
	return "docker.io"
}

func taskResult(result *RunResult, taskName string) *TaskResult {
	for i := range result.Tasks {
		if result.Tasks[i].Name == taskName {
			return &result.Tasks[i]
		}
	}
	if len(result.Tasks) == 1 {
		return &result.Tasks[0]
	}
	return nil
}

func isSuccessfulStatus(status string) bool {
	switch status {
	case "succeeded", "cached":
		return true
	default:
		return false
	}
}

// CopyLog writes the reproduced task log when present.
func CopyLog(w io.Writer, result *ExecutionResult) {
	if w == nil || result == nil || result.Log == "" {
		return
	}
	_, _ = io.WriteString(w, result.Log)
	if !strings.HasSuffix(result.Log, "\n") {
		_, _ = io.WriteString(w, "\n")
	}
}

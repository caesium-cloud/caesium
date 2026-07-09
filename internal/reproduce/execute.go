package reproduce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/outputdiff"
	"github.com/caesium-cloud/caesium/pkg/container"
	pkgjobdef "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
)

const DefaultShell = "/bin/sh"

// Puller pulls the image reference selected for local reproduction.
type Puller interface {
	Pull(ctx context.Context, imageRef, platform string) error
}

// LocalImageChecker is optionally implemented by a Puller that can report
// whether the image already exists in the local daemon, letting Execute skip
// the registry pull (and survive registry-auth failures) when it does.
type LocalImageChecker interface {
	ExistsLocally(ctx context.Context, imageRef string) bool
}

// Runner executes a synthesized one-step definition through the local runtime.
type Runner interface {
	Run(ctx context.Context, def *pkgjobdef.Definition, taskTimeout time.Duration) (*RunResult, error)
}

// ShellRunner starts an interactive shell in the reconstructed environment.
type ShellRunner interface {
	RunShell(ctx context.Context, req ShellRequest) error
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

// ShellExecuteOptions controls interactive shell execution.
type ShellExecuteOptions struct {
	Puller      Puller
	ShellRunner ShellRunner
	Platform    string
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
	Fidelity     *FidelitySummary  `json:"fidelity,omitempty"`
	OutputDiff   *outputdiff.Diff  `json:"output_diff,omitempty"`
	Warnings     []Warning         `json:"warnings,omitempty"`
}

// ShellRequest is the docker-run shape for interactive shell mode.
type ShellRequest struct {
	Image    string            `json:"image"`
	Shell    string            `json:"shell"`
	Env      map[string]string `json:"env,omitempty"`
	Mounts   []container.Mount `json:"mounts,omitempty"`
	WorkDir  string            `json:"workdir,omitempty"`
	Platform string            `json:"platform,omitempty"`
}

// ShellResult reports the interactive shell process outcome.
type ShellResult struct {
	ExitCode int              `json:"exit_code"`
	Task     string           `json:"task"`
	Image    string           `json:"image"`
	Error    string           `json:"error,omitempty"`
	Envelope *Envelope        `json:"envelope"`
	Fidelity *FidelitySummary `json:"fidelity,omitempty"`
	Warnings []Warning        `json:"warnings,omitempty"`
}

// PullError is returned when the selected image cannot be pulled.
type PullError struct {
	ImageRef string
	Registry string
	Err      error
}

func (e *PullError) Error() string {
	return fmt.Sprintf(
		"pull image %s from registry %s failed: %v; authenticate with docker login %s and re-run, or make the image available in the local Docker daemon (docker pull/docker build) — reproduce uses a locally present image without pulling",
		e.ImageRef,
		e.Registry,
		e.Err,
		e.Registry,
	)
}

func (e *PullError) Unwrap() error {
	return e.Err
}

// ShellUnavailableError is returned when the selected image has no shell at the
// requested path, which is common for distroless images.
type ShellUnavailableError struct {
	Image string
	Shell string
	Err   error
}

func (e *ShellUnavailableError) Error() string {
	return fmt.Sprintf(
		"shell %s is unavailable in image %s; distroless images often omit a shell. The --shell-image sidecar fallback is deferred (design Open Question #4); use run mode or rebuild an image with a shell for interactive debugging",
		e.Shell,
		e.Image,
	)
}

func (e *ShellUnavailableError) Unwrap() error {
	return e.Err
}

// ShellExitError records a non-zero exit from the interactive shell process.
type ShellExitError struct {
	Code int
	Err  error
}

func (e *ShellExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("shell exited with status %d", e.Code)
}

func (e *ShellExitError) Unwrap() error {
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
	if err := ensureImageAvailable(ctx, env, opts.Puller, platform); err != nil {
		return nil, err
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
		Fidelity:     env.Fidelity,
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

// ExecuteShell pulls the reconstructed image and starts an interactive shell
// inside the exact reconstructed environment.
func ExecuteShell(ctx context.Context, env *Envelope, opts ShellExecuteOptions) (*ShellResult, error) {
	if env == nil {
		return nil, fmt.Errorf("envelope is required")
	}
	if opts.Puller == nil {
		return nil, fmt.Errorf("image puller is required")
	}
	if opts.ShellRunner == nil {
		return nil, fmt.Errorf("shell runner is required")
	}

	platform := firstNonEmpty(strings.TrimSpace(opts.Platform), env.Platform)
	if err := ensureImageAvailable(ctx, env, opts.Puller, platform); err != nil {
		return nil, err
	}

	req := BuildShellRequest(env, platform)
	result := &ShellResult{
		Task:     env.TaskName,
		Image:    env.Image,
		Envelope: env,
		Fidelity: env.Fidelity,
		Warnings: env.Warnings,
	}
	if err := opts.ShellRunner.RunShell(ctx, req); err != nil {
		var shellExit *ShellExitError
		if errors.As(err, &shellExit) {
			result.ExitCode = shellExit.Code
			result.Error = shellExit.Error()
			return result, nil
		}
		return nil, err
	}
	return result, nil
}

// BuildShellRequest converts an envelope into the interactive docker-run
// request. The default shell is /bin/sh, matching the design.
func BuildShellRequest(env *Envelope, platform string) ShellRequest {
	if env == nil {
		return ShellRequest{Shell: DefaultShell}
	}
	return ShellRequest{
		Image:    env.Image,
		Shell:    DefaultShell,
		Env:      cloneStringMap(env.Env),
		Mounts:   slices.Clone(env.Mounts),
		WorkDir:  env.WorkDir,
		Platform: strings.TrimSpace(platform),
	}
}

func ensureImageAvailable(ctx context.Context, env *Envelope, puller Puller, platform string) error {
	checker, canCheckLocal := puller.(LocalImageChecker)
	if canCheckLocal && checker.ExistsLocally(ctx, env.Image) {
		env.Warnings = append(env.Warnings, Warning{
			Code:    "local_image_used",
			Message: fmt.Sprintf("image %s already present in the local daemon; skipped registry pull", env.Image),
		})
		sortWarnings(env.Warnings)
		return nil
	}
	if err := puller.Pull(ctx, env.Image, platform); err != nil {
		return &PullError{
			ImageRef: env.Image,
			Registry: RegistryHost(env.Image),
			Err:      err,
		}
	}
	return nil
}

// RegistryHost returns the registry host component used in pull guidance.
func RegistryHost(imageRef string) string {
	ref := imageRef
	if before, _, ok := strings.Cut(ref, "@"); ok {
		ref = before
	}
	slash := strings.IndexByte(ref, '/')
	if slash < 0 {
		return "docker.io"
	}
	first := ref[:slash]
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

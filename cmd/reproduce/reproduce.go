// Package reproduce implements the caesium reproduce CLI.
package reproduce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/localrun"
	"github.com/caesium-cloud/caesium/internal/outputdiff"
	ireproduce "github.com/caesium-cloud/caesium/internal/reproduce"
	"github.com/caesium-cloud/caesium/pkg/container"
	pkgjobdef "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
)

var (
	reproduceJobID    string
	reproduceTask     string
	reproduceServer   string
	reproduceAPIKey   string
	reproduceDryRun   bool
	reproduceJSON     bool
	reproduceSet      []string
	reproduceSetEnv   []string
	reproduceMounts   []string
	reproduceTimeout  time.Duration
	reproducePlatform string
	reproduceDiff     bool
	reproduceShell    bool
)

var httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}

// Cmd is the top-level reproduce command.
var Cmd = &cobra.Command{
	Use:           "reproduce <run-id> --job-id <job-id> --task <task>",
	Short:         "Re-execute a historical task locally",
	Args:          cobra.ArbitraryArgs,
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runReproduce,
}

type descriptorResponse struct {
	TaskRunID  string          `json:"task_run_id"`
	Status     string          `json:"status"`
	Result     string          `json:"result"`
	Output     json.RawMessage `json:"output"`
	ReplaySafe bool            `json:"replay_safe"`
	LogExcerpt struct {
		Path string `json:"path"`
	} `json:"log_excerpt"`
	Descriptor json.RawMessage `json:"descriptor"`
}

type dryRunEnvelope struct {
	RunID      string `json:"run_id"`
	JobID      string `json:"job_id"`
	TaskRunID  string `json:"task_run_id,omitempty"`
	Status     string `json:"recorded_status,omitempty"`
	Result     string `json:"recorded_result,omitempty"`
	LogExcerpt string `json:"log_excerpt,omitempty"`
	*ireproduce.Envelope
}

type runJSONResult struct {
	RunID          string          `json:"run_id"`
	JobID          string          `json:"job_id"`
	TaskRunID      string          `json:"task_run_id,omitempty"`
	RecordedStatus string          `json:"recorded_status,omitempty"`
	RecordedResult string          `json:"recorded_result,omitempty"`
	RecordedOutput json.RawMessage `json:"recorded_output,omitempty"`
	*ireproduce.ExecutionResult
}

func init() {
	Cmd.Flags().StringVar(&reproduceJobID, "job-id", "", "Job ID that owns the run (required)")
	Cmd.Flags().StringVar(&reproduceTask, "task", "", "Task name or ID within the run (required)")
	Cmd.Flags().StringVar(&reproduceServer, "server", "http://localhost:8080", "Caesium server base URL")
	Cmd.Flags().StringVar(&reproduceAPIKey, "api-key", "", "API key for authentication (prefer "+cliutil.APIKeyEnvVar+"; --api-key is visible in process listings)")
	Cmd.Flags().BoolVar(&reproduceDryRun, "dry-run", false, "Print the reconstructed envelope as JSON without executing")
	Cmd.Flags().BoolVar(&reproduceJSON, "json", false, "Emit machine-readable JSON")
	Cmd.Flags().StringArrayVar(&reproduceSet, "set", nil, "Override a run param as key=value (repeatable)")
	Cmd.Flags().StringArrayVar(&reproduceSetEnv, "set-env", nil, "Override or add a container env var as KEY=VALUE (repeatable)")
	Cmd.Flags().StringArrayVar(&reproduceMounts, "mount", nil, "Remap a recorded bind mount source as old=new (repeatable)")
	Cmd.Flags().DurationVar(&reproduceTimeout, "timeout", 0, "Local task timeout (default: recorded task timeout)")
	Cmd.Flags().StringVar(&reproducePlatform, "platform", "", "Platform to use when pulling the image (for example linux/amd64)")
	Cmd.Flags().BoolVar(&reproduceDiff, "diff", false, "Compare reproduced output markers against recorded output")
	Cmd.Flags().BoolVar(&reproduceShell, "shell", false, "Open an interactive shell in the reconstructed environment")
}

func runReproduce(cmd *cobra.Command, args []string) error {
	stderr := cmd.ErrOrStderr()
	if len(args) != 1 {
		return exitWithMessage(stderr, 2, "reproduce requires exactly one run id")
	}
	if err := validateModeFlags(reproduceShell, reproduceDiff, reproduceDryRun, reproduceJSON); err != nil {
		return printExitError(stderr, err)
	}

	runID := strings.TrimSpace(args[0])
	if strings.TrimSpace(reproduceJobID) == "" {
		return exitWithMessage(stderr, 2, "--job-id is required")
	}
	if strings.TrimSpace(reproduceTask) == "" {
		return exitWithMessage(stderr, 2, "--task is required")
	}

	setParams, err := parseAssignments(reproduceSet, "--set")
	if err != nil {
		return exitWithMessage(stderr, 2, err.Error())
	}
	setEnv, err := parseAssignments(reproduceSetEnv, "--set-env")
	if err != nil {
		return exitWithMessage(stderr, 2, err.Error())
	}
	mounts, err := parseMountRemaps(reproduceMounts)
	if err != nil {
		return exitWithMessage(stderr, 2, err.Error())
	}

	server := strings.TrimRight(reproduceServer, "/")
	if !reproduceDryRun || !reproduceJSON {
		_, _ = fmt.Fprintf(stderr, "fetching descriptor from %s\n", server)
	}
	resp, err := fetchDescriptor(cmd.Context(), cmd, server, reproduceJobID, runID, reproduceTask)
	if err != nil {
		return exitWithMessage(stderr, 2, err.Error())
	}

	desc, err := ireproduce.DecodeDescriptor(resp.Descriptor)
	if err != nil {
		return exitWithMessage(stderr, 2, err.Error())
	}
	env, err := ireproduce.Reconstruct(desc, ireproduce.ReconstructOptions{
		SetParams:  setParams,
		SetEnv:     setEnv,
		Mounts:     mounts,
		Timeout:    reproduceTimeout,
		Platform:   reproducePlatform,
		ReplaySafe: resp.ReplaySafe,
	})
	if err != nil {
		return exitWithMessage(stderr, 2, err.Error())
	}
	printWarnings(stderr, env.Warnings)

	var recordedOutput map[string]string
	if reproduceDiff {
		recordedOutput, err = decodeRecordedOutput(resp.Output)
		if err != nil {
			return exitWithMessage(stderr, 2, err.Error())
		}
	}

	envelopeOut := dryRunEnvelope{
		RunID:      runID,
		JobID:      reproduceJobID,
		TaskRunID:  resp.TaskRunID,
		Status:     resp.Status,
		Result:     resp.Result,
		LogExcerpt: resp.LogExcerpt.Path,
		Envelope:   env,
	}
	if reproduceDryRun {
		if err := writeJSON(cmd, envelopeOut); err != nil {
			return exitWithMessage(stderr, 2, err.Error())
		}
		return nil
	}

	if reproduceShell {
		result, err := ireproduce.ExecuteShell(cmd.Context(), env, ireproduce.ShellExecuteOptions{
			Puller:      dockerPuller{},
			ShellRunner: dockerShellRunner{},
			Platform:    reproducePlatform,
		})
		if err != nil {
			return exitWithMessage(stderr, 2, err.Error())
		}
		printFidelitySummary(stderr, result.Fidelity)
		if result.ExitCode != 0 {
			return exitWithCode(result.ExitCode)
		}
		return nil
	}

	result, err := ireproduce.Execute(cmd.Context(), desc, env, ireproduce.ExecuteOptions{
		Puller:   dockerPuller{},
		Runner:   localRunner{},
		Timeout:  reproduceTimeout,
		Platform: reproducePlatform,
	})
	if err != nil {
		return exitWithMessage(stderr, 2, err.Error())
	}

	var diff *outputdiff.Diff
	if reproduceDiff && result.ExitCode == 0 {
		computed := outputdiff.Compare(recordedOutput, result.Output)
		result.OutputDiff = &computed
		diff = &computed
		if !computed.Empty() {
			result.ExitCode = 3
		}
	}

	if reproduceJSON {
		if err := writeJSON(cmd, runJSONResult{
			RunID:           runID,
			JobID:           reproduceJobID,
			TaskRunID:       resp.TaskRunID,
			RecordedStatus:  resp.Status,
			RecordedResult:  resp.Result,
			RecordedOutput:  resp.Output,
			ExecutionResult: result,
		}); err != nil {
			return exitWithMessage(stderr, 2, err.Error())
		}
		if result.ExitCode != 0 {
			return exitWithCode(result.ExitCode)
		}
		return nil
	}

	ireproduce.CopyLog(cmd.OutOrStdout(), result)
	if diff != nil {
		_, _ = io.WriteString(cmd.OutOrStdout(), diff.Render())
	}
	if result.ExitCode == 0 {
		_, _ = fmt.Fprintf(stderr, "%s exited 0\n", env.TaskName)
		printFidelitySummary(stderr, result.Fidelity)
		return nil
	}
	if result.ExitCode == 3 {
		_, _ = fmt.Fprintf(stderr, "%s exited 0; reproduced output differs from recorded output\n", env.TaskName)
		printFidelitySummary(stderr, result.Fidelity)
		return exitWithCode(3)
	}
	_, _ = fmt.Fprintf(stderr, "%s failed", env.TaskName)
	if result.Error != "" {
		_, _ = fmt.Fprintf(stderr, ": %s", result.Error)
	}
	_, _ = fmt.Fprintln(stderr)
	printFidelitySummary(stderr, result.Fidelity)
	return exitWithCode(1)
}

func fetchDescriptor(ctx context.Context, cmd *cobra.Command, server, jobID, runID, task string) (*descriptorResponse, error) {
	reqURL := fmt.Sprintf(
		"%s/v1/jobs/%s/runs/%s/tasks/%s/descriptor",
		server,
		url.PathEscape(jobID),
		url.PathEscape(runID),
		url.PathEscape(task),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey := cliutil.ResolveAPIKey(cmd, reproduceAPIKey, cliutil.APIKeyEnvVar); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch descriptor: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusBadRequest {
		if resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "descriptor unavailable") {
			return nil, fmt.Errorf("descriptor unavailable for run %s task %s", runID, task)
		}
		return nil, fmt.Errorf("fetch descriptor failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if readErr != nil {
		return nil, fmt.Errorf("read descriptor response: %w", readErr)
	}

	var out descriptorResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode descriptor response: %w", err)
	}
	if len(out.Descriptor) == 0 {
		return nil, fmt.Errorf("descriptor unavailable for run %s task %s", runID, task)
	}
	return &out, nil
}

type dockerPuller struct{}

func (dockerPuller) Pull(ctx context.Context, imageRef, platform string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()

	reader, err := cli.ImagePull(ctx, imageRef, image.PullOptions{Platform: platform})
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	_, err = io.Copy(io.Discard, reader)
	return err
}

// ExistsLocally reports whether the image is already present in the local
// daemon, letting Execute skip the registry pull (and survive private-registry
// auth failures when the operator pulled or built the image themselves).
func (dockerPuller) ExistsLocally(ctx context.Context, imageRef string) bool {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return false
	}
	defer func() { _ = cli.Close() }()

	_, err = cli.ImageInspect(ctx, imageRef)
	return err == nil
}

type localRunner struct{}

func (localRunner) Run(ctx context.Context, def *pkgjobdef.Definition, taskTimeout time.Duration) (*ireproduce.RunResult, error) {
	result, err := localrun.New(localrun.Config{TaskTimeout: taskTimeout}).RunWithResult(ctx, def)
	if result == nil {
		return nil, err
	}
	out := &ireproduce.RunResult{
		Status: result.Status,
		Error:  result.Error,
		Tasks:  make([]ireproduce.TaskResult, 0, len(result.Tasks)),
	}
	for _, task := range result.Tasks {
		out.Tasks = append(out.Tasks, ireproduce.TaskResult{
			Name:         task.Name,
			Status:       task.Status,
			Output:       task.Output,
			LogText:      task.LogText,
			LogTruncated: task.LogTruncated,
			Error:        task.Error,
		})
	}
	return out, err
}

type dockerShellRunner struct{}

func (dockerShellRunner) RunShell(ctx context.Context, req ireproduce.ShellRequest) error {
	command := osexec.CommandContext(ctx, "docker", dockerRunShellArgs(req)...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		code := commandExitCode(err)
		if code == 126 || code == 127 {
			return &ireproduce.ShellUnavailableError{Image: req.Image, Shell: req.Shell, Err: err}
		}
		if code >= 0 {
			return &ireproduce.ShellExitError{Code: code, Err: err}
		}
		return err
	}
	return nil
}

func dockerRunShellArgs(req ireproduce.ShellRequest) []string {
	shell := strings.TrimSpace(req.Shell)
	if shell == "" {
		shell = ireproduce.DefaultShell
	}
	args := []string{"run", "--rm", "-it", "--entrypoint", shell}
	if strings.TrimSpace(req.Platform) != "" {
		args = append(args, "--platform", req.Platform)
	}
	if strings.TrimSpace(req.WorkDir) != "" {
		args = append(args, "--workdir", req.WorkDir)
	}
	for _, key := range sortedMapKeys(req.Env) {
		args = append(args, "-e", key+"="+req.Env[key])
	}
	for _, mount := range req.Mounts {
		if arg := dockerMountArg(mount); arg != "" {
			args = append(args, "--mount", arg)
		}
	}
	args = append(args, req.Image)
	return args
}

func dockerMountArg(mount container.Mount) string {
	switch mount.Type {
	case container.MountTypeBind:
		if strings.TrimSpace(mount.Source) == "" || strings.TrimSpace(mount.Target) == "" {
			return ""
		}
		return readonlySuffix(fmt.Sprintf("type=bind,source=%s,target=%s", mount.Source, mount.Target), mount.ReadOnly)
	case container.MountTypeVolume:
		if strings.TrimSpace(mount.Source) == "" || strings.TrimSpace(mount.Target) == "" {
			return ""
		}
		return readonlySuffix(fmt.Sprintf("type=volume,source=%s,target=%s", mount.Source, mount.Target), mount.ReadOnly)
	case container.MountTypeTmpfs:
		if strings.TrimSpace(mount.Target) == "" {
			return ""
		}
		return fmt.Sprintf("type=tmpfs,target=%s", mount.Target)
	default:
		return ""
	}
}

func readonlySuffix(arg string, readonly bool) string {
	if readonly {
		return arg + ",readonly"
	}
	return arg
}

func commandExitCode(err error) int {
	var exitErr *osexec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func parseAssignments(values []string, flag string) ([]ireproduce.Assignment, error) {
	out := make([]ireproduce.Assignment, 0, len(values))
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("%s must be key=value: %s", flag, value)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s key cannot be empty", flag)
		}
		out = append(out, ireproduce.Assignment{Key: key, Value: val})
	}
	return out, nil
}

func parseMountRemaps(values []string) ([]ireproduce.MountRemap, error) {
	out := make([]ireproduce.MountRemap, 0, len(values))
	for _, value := range values {
		from, to, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("--mount must be old=new: %s", value)
		}
		from = strings.TrimSpace(from)
		to = strings.TrimSpace(to)
		if from == "" || to == "" {
			return nil, fmt.Errorf("--mount old and new paths are required")
		}
		out = append(out, ireproduce.MountRemap{From: from, To: to})
	}
	return out, nil
}

func writeJSON(cmd *cobra.Command, value any) error {
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func decodeRecordedOutput(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode recorded output: %w", err)
	}
	return out, nil
}

func printWarnings(w io.Writer, warnings []ireproduce.Warning) {
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(w, "warning: %s\n", warning.Message)
	}
}

func printFidelitySummary(w io.Writer, summary *ireproduce.FidelitySummary) {
	if w == nil || summary == nil || len(summary.Dimensions) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "fidelity summary:")
	for _, dimension := range summary.Dimensions {
		if len(dimension.Details) == 0 {
			_, _ = fmt.Fprintf(w, "  %s: %s\n", dimension.Dimension, dimension.Status)
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s: %s - %s\n", dimension.Dimension, dimension.Status, strings.Join(dimension.Details, "; "))
	}
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validateModeFlags(shellMode, diffMode, dryRunMode, jsonMode bool) error {
	switch {
	case shellMode && diffMode:
		return usageError("--shell cannot be combined with --diff")
	case shellMode && dryRunMode:
		return usageError("--shell cannot be combined with --dry-run")
	case shellMode && jsonMode:
		return usageError("--shell cannot be combined with --json")
	case diffMode && dryRunMode:
		return usageError("--diff cannot be combined with --dry-run")
	default:
		return nil
	}
}

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string {
	if e.msg != "" {
		return e.msg
	}
	return fmt.Sprintf("exit status %d", e.code)
}

func (e *exitError) ExitCode() int {
	return e.code
}

func usageError(message string) error {
	return &exitError{code: 2, msg: message}
}

// ExitCode extracts the process exit code requested by reproduce.
func ExitCode(err error) (int, bool) {
	var exitErr *exitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

func printExitError(w io.Writer, err error) error {
	var exitErr *exitError
	if errors.As(err, &exitErr) && exitErr.msg != "" {
		_, _ = fmt.Fprintln(w, exitErr.msg)
	}
	return err
}

func exitWithMessage(w io.Writer, code int, message string) error {
	_, _ = fmt.Fprintln(w, message)
	return &exitError{code: code, msg: message}
}

func exitWithCode(code int) error {
	if code == 0 {
		return nil
	}
	return &exitError{code: code}
}

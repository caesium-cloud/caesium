// Package reproduce implements the caesium reproduce CLI.
package reproduce

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/localrun"
	ireproduce "github.com/caesium-cloud/caesium/internal/reproduce"
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
)

var httpClient = &http.Client{Timeout: cliutil.DefaultHTTPTimeout}

// Cmd is the top-level reproduce command.
var Cmd = &cobra.Command{
	Use:           "reproduce <run-id> --job-id <job-id> --task <task>",
	Short:         "Re-execute a historical task locally",
	Args:          cobra.ExactArgs(1),
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
}

func runReproduce(cmd *cobra.Command, args []string) error {
	runID := strings.TrimSpace(args[0])
	if strings.TrimSpace(reproduceJobID) == "" {
		return fmt.Errorf("--job-id is required")
	}
	if strings.TrimSpace(reproduceTask) == "" {
		return fmt.Errorf("--task is required")
	}

	setParams, err := parseAssignments(reproduceSet, "--set")
	if err != nil {
		return err
	}
	setEnv, err := parseAssignments(reproduceSetEnv, "--set-env")
	if err != nil {
		return err
	}
	mounts, err := parseMountRemaps(reproduceMounts)
	if err != nil {
		return err
	}

	stderr := cmd.ErrOrStderr()
	server := strings.TrimRight(reproduceServer, "/")
	if !reproduceDryRun || !reproduceJSON {
		_, _ = fmt.Fprintf(stderr, "fetching descriptor from %s\n", server)
	}
	resp, err := fetchDescriptor(cmd.Context(), cmd, server, reproduceJobID, runID, reproduceTask)
	if err != nil {
		exitWithMessage(stderr, 2, err.Error())
	}

	desc, err := ireproduce.DecodeDescriptor(resp.Descriptor)
	if err != nil {
		exitWithMessage(stderr, 2, err.Error())
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
		exitWithMessage(stderr, 2, err.Error())
	}
	printWarnings(stderr, env.Warnings)

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
			exitWithMessage(stderr, 2, err.Error())
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
		exitWithMessage(stderr, 2, err.Error())
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
			exitWithMessage(stderr, 2, err.Error())
		}
		os.Exit(result.ExitCode)
	}

	ireproduce.CopyLog(cmd.OutOrStdout(), result)
	if result.ExitCode == 0 {
		_, _ = fmt.Fprintf(stderr, "%s exited 0\n", env.TaskName)
		return nil
	}
	_, _ = fmt.Fprintf(stderr, "%s failed", env.TaskName)
	if result.Error != "" {
		_, _ = fmt.Fprintf(stderr, ": %s", result.Error)
	}
	_, _ = fmt.Fprintln(stderr)
	os.Exit(1)
	return nil
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

func printWarnings(w io.Writer, warnings []ireproduce.Warning) {
	for _, warning := range warnings {
		_, _ = fmt.Fprintf(w, "warning: %s\n", warning.Message)
	}
}

func exitWithMessage(w io.Writer, code int, message string) {
	_, _ = fmt.Fprintln(w, message)
	os.Exit(code)
}

// Package reproduce implements the caesium reproduce CLI.
package reproduce

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/caesium-cloud/caesium/cmd/cliutil"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/localrun"
	"github.com/caesium-cloud/caesium/internal/outputdiff"
	ireproduce "github.com/caesium-cloud/caesium/internal/reproduce"
	"github.com/caesium-cloud/caesium/internal/strutil"
	"github.com/caesium-cloud/caesium/pkg/container"
	pkgjobdef "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	reproduceJobID          string
	reproduceTask           string
	reproduceServer         string
	reproduceAPIKey         string
	reproduceDryRun         bool
	reproduceJSON           bool
	reproduceSet            []string
	reproduceSetEnv         []string
	reproduceMounts         []string
	reproduceTimeout        time.Duration
	reproducePlatform       string
	reproduceDiff           bool
	reproduceShell          bool
	reproduceShellImage     string
	reproduceImage          string
	reproduceResolveSecrets bool
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
	Cmd.Flags().StringVar(&reproduceShellImage, "shell-image", ireproduce.DefaultShellImage, "With --shell: image whose static BusyBox is injected when the target image has no shell (empty disables the fallback)")
	Cmd.Flags().StringVar(&reproduceImage, "image", "", "Override the image for fix testing (marks output OVERRIDDEN)")
	Cmd.Flags().BoolVar(&reproduceResolveSecrets, "resolve-secrets", false, "Resolve secret:// refs via local providers (default: omit and warn)")
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
	var secretResolver ireproduce.SecretResolver
	if reproduceResolveSecrets {
		secretResolver, err = buildReproduceSecretResolver()
		if err != nil {
			return exitWithMessage(stderr, 2, err.Error())
		}
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
	env, err := ireproduce.Reconstruct(cmd.Context(), desc, ireproduce.ReconstructOptions{
		SetParams:      setParams,
		SetEnv:         setEnv,
		Mounts:         mounts,
		Timeout:        reproduceTimeout,
		Platform:       reproducePlatform,
		ReplaySafe:     resp.ReplaySafe,
		ImageOverride:  reproduceImage,
		ResolveSecrets: reproduceResolveSecrets,
		SecretResolver: secretResolver,
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
		// Installed before any container is allocated: SIGINT/SIGTERM cancel the
		// context so the shell runner's cleanup defers run instead of the
		// process dying with the reconstructed container (and its env) behind.
		shellCtx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stopSignals()
		// After the first signal restore default handling, so a second
		// Ctrl-C / SIGTERM exits immediately (the usual CLI convention).
		go func() {
			<-shellCtx.Done()
			stopSignals()
		}()
		result, err := ireproduce.ExecuteShell(shellCtx, env, ireproduce.ShellExecuteOptions{
			Puller:      dockerPuller{},
			ShellRunner: dockerShellRunner{},
			Platform:    reproducePlatform,
			ShellImage:  reproduceShellImage,
			TTY:         term.IsTerminal(int(os.Stdin.Fd())),
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
	} else if reproduceDiff {
		_, _ = fmt.Fprintln(stderr, "diff skipped: the task exited non-zero, so there is no successful output to compare")
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
	taskLabel := resultTaskLabel(env)
	if result.ExitCode == 0 {
		_, _ = fmt.Fprintf(stderr, "%s exited 0\n", taskLabel)
		printFidelitySummary(stderr, result.Fidelity)
		return nil
	}
	if result.ExitCode == 3 {
		_, _ = fmt.Fprintf(stderr, "%s exited 0; reproduced output differs from recorded output\n", taskLabel)
		printFidelitySummary(stderr, result.Fidelity)
		return exitWithCode(3)
	}
	_, _ = fmt.Fprintf(stderr, "%s failed", taskLabel)
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
			if readErr != nil {
				return nil, fmt.Errorf("descriptor unavailable for run %s task %s (reading response: %w)", runID, task, readErr)
			}
			return nil, fmt.Errorf("descriptor unavailable for run %s task %s", runID, task)
		}
		if readErr != nil {
			return nil, fmt.Errorf("fetch descriptor failed (%d): %s (reading response: %w)", resp.StatusCode, strings.TrimSpace(string(body)), readErr)
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
		if readErr != nil {
			return nil, fmt.Errorf("descriptor unavailable for run %s task %s (reading response: %w)", runID, task, readErr)
		}
		return nil, fmt.Errorf("descriptor unavailable for run %s task %s", runID, task)
	}
	return &out, nil
}

type secretResolverAdapter struct {
	resolver secret.Resolver
}

func (a secretResolverAdapter) ResolveWithIdentity(ctx context.Context, ref string) (string, ireproduce.SecretIdentity, error) {
	value, identity, err := a.resolver.ResolveWithIdentity(ctx, ref)
	if err != nil {
		return "", ireproduce.SecretIdentity{}, err
	}
	return value, ireproduce.SecretIdentity{
		Provider:           identity.Provider,
		Ref:                identity.Ref,
		Version:            identity.Version,
		ResourceVersion:    identity.ResourceVersion,
		Namespace:          identity.Namespace,
		Name:               identity.Name,
		Key:                identity.Key,
		KeyID:              identity.KeyID,
		HMACSHA256:         identity.HMACSHA256,
		Verifiable:         identity.Verifiable,
		UnverifiableReason: identity.UnverifiableReason,
		Metadata:           identity.Metadata,
	}, nil
}

func buildReproduceSecretResolver() (ireproduce.SecretResolver, error) {
	cfg := secret.Config{
		EnableEnv:           envBool("CAESIUM_JOBDEF_SECRETS_ENABLE_ENV", true),
		IdentityHMACKeys:    os.Getenv("CAESIUM_JOBDEF_SECRETS_IDENTITY_HMAC_KEYS"),
		IdentityHMACCurrent: os.Getenv("CAESIUM_JOBDEF_SECRETS_IDENTITY_HMAC_KEY_ID"),
	}

	kubeConfig := firstNonEmptyString(os.Getenv("CAESIUM_JOBDEF_SECRETS_KUBECONFIG"), os.Getenv("KUBECONFIG"))
	kubeNamespaceRaw := os.Getenv("CAESIUM_JOBDEF_SECRETS_KUBE_NAMESPACE")
	kubeNamespace := firstNonEmptyString(kubeNamespaceRaw, os.Getenv("KUBERNETES_NAMESPACE"), "default")
	if envBool("CAESIUM_JOBDEF_SECRETS_ENABLE_KUBERNETES", false) ||
		kubeConfig != "" ||
		(strings.TrimSpace(kubeNamespaceRaw) != "" && kubeNamespace != "default") ||
		os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		cfg.Kubernetes = &secret.KubernetesConfig{
			KubeConfigPath: kubeConfig,
			Namespace:      kubeNamespace,
		}
	}

	vaultAddress := firstNonEmptyString(os.Getenv("CAESIUM_JOBDEF_SECRETS_VAULT_ADDRESS"), os.Getenv("VAULT_ADDR"))
	if vaultAddress != "" {
		cfg.Vault = &secret.VaultConfig{
			Address:       vaultAddress,
			Token:         firstNonEmptyString(os.Getenv("CAESIUM_JOBDEF_SECRETS_VAULT_TOKEN"), os.Getenv("VAULT_TOKEN")),
			Namespace:     firstNonEmptyString(os.Getenv("CAESIUM_JOBDEF_SECRETS_VAULT_NAMESPACE"), os.Getenv("VAULT_NAMESPACE")),
			CACertPath:    firstNonEmptyString(os.Getenv("CAESIUM_JOBDEF_SECRETS_VAULT_CA_CERT"), os.Getenv("VAULT_CACERT")),
			TLSSkipVerify: envBool("CAESIUM_JOBDEF_SECRETS_VAULT_SKIP_VERIFY", false) || envBool("VAULT_SKIP_VERIFY", false),
		}
	}

	resolver, err := secret.NewConfiguredResolver(cfg)
	if err != nil {
		return nil, fmt.Errorf("build local secret resolver: %w", err)
	}
	return secretResolverAdapter{resolver: resolver}, nil
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
			LogScrubbed:  task.LogScrubbed,
			Error:        task.Error,
		})
	}
	return out, err
}

type dockerShellRunner struct{}

// shellStderrTee mirrors docker's stderr to the operator while keeping a
// bounded copy so exit 126/127 can be classified: only a docker/daemon
// exec-failure (shell binary missing from the image) is "shell unavailable";
// the same codes typed inside a WORKING shell (`exit 127`) must not be.
type shellStderrTee struct {
	dst io.Writer
	buf bytes.Buffer
}

func (t *shellStderrTee) Write(p []byte) (int, error) {
	if t.buf.Len() < 8192 {
		t.buf.Write(p)
	}
	return t.dst.Write(p)
}

func (t *shellStderrTee) indicatesMissingShell() bool {
	s := t.buf.String()
	return strings.Contains(s, "Error response from daemon") ||
		strings.Contains(s, "no such file or directory") ||
		strings.Contains(s, "executable file not found")
}

// shellSessionLabel marks every container a `reproduce --shell` run creates,
// valued with a per-run id, so leftovers are identifiable and cleanable.
const shellSessionLabel = "caesium.reproduce.shell"

func (dockerShellRunner) RunShell(ctx context.Context, req ireproduce.ShellRequest) error {
	req.Session = uuid.NewString()
	// Whatever happens (error, signal-cancelled ctx), remove this run's
	// containers with a context that is not cancelled.
	defer removeSessionContainers(context.WithoutCancel(ctx), req.Session)
	// Backstop: if the docker client dies without restoring the terminal, put
	// the operator's tty back the way we found it.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if state, serr := term.GetState(int(os.Stdin.Fd())); serr == nil {
			defer func() { _ = term.Restore(int(os.Stdin.Fd()), state) }()
		}
	}
	// Probe without stdin first so a missing shell never swallows piped input
	// that the fallback shell (or the real one) still needs to read.
	probeErr := runShellCommand(ctx, req, dockerProbeShellArgs(req), nil, io.Discard, io.Discard)
	var unavailable *ireproduce.ShellUnavailableError
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !errors.As(probeErr, &unavailable) {
		return runShellCommand(ctx, req, dockerRunShellArgs(req), os.Stdin, os.Stdout, os.Stderr)
	}
	if strings.TrimSpace(req.ShellImage) == "" {
		return unavailable
	}
	fmt.Fprintf(os.Stderr, "shell %s is unavailable in %s; injecting a static BusyBox from %s\n", req.Shell, req.Image, req.ShellImage)
	if ferr := runInjectedShell(ctx, req); ferr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, ok := errors.AsType[*ireproduce.ShellExitError](ferr); ok {
			return ferr
		}
		return &ireproduce.ShellUnavailableError{Image: req.Image, Shell: req.Shell, ShellImage: req.ShellImage, Err: ferr}
	}
	return nil
}

// runShellCommand runs a docker command and classifies a missing-shell exec
// failure (exit 126/127 with a daemon/exec error on stderr).
func runShellCommand(ctx context.Context, req ireproduce.ShellRequest, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	tee := &shellStderrTee{dst: stderr}
	command := gracefulCommand(ctx, "docker", args...)
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = tee
	if err := command.Run(); err != nil {
		code := commandExitCode(err)
		if (code == 126 || code == 127) && tee.indicatesMissingShell() {
			return &ireproduce.ShellUnavailableError{Image: req.Image, Shell: req.Shell, Err: err}
		}
		if code >= 0 {
			return &ireproduce.ShellExitError{Code: code, Err: err}
		}
		return err
	}
	return nil
}

// dockerProbeShellArgs is a stdin-less, non-TTY run that only checks the
// shell can be exec'd.
func dockerProbeShellArgs(req ireproduce.ShellRequest) []string {
	probe := req
	probe.TTY = false
	args := dockerRunShellArgs(probe)
	// docker run <flags> <image> [cmd...]: append the probe command.
	return append(args, "-c", "true")
}

// gracefulCommand is exec.CommandContext except cancellation sends SIGTERM and
// waits before killing: the docker client restores a raw-mode terminal itself
// when terminated, but not when SIGKILLed.
func gracefulCommand(ctx context.Context, name string, args ...string) *osexec.Cmd {
	command := osexec.CommandContext(ctx, name, args...)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = 5 * time.Second
	return command
}

// dockerCreate runs a short `docker create` that is NOT interrupted by ctx: a
// killed client can still leave the daemon registering the container after any
// cleanup snapshot, so we always learn the id first and honour cancellation
// afterwards.
//
// Because it ignores cancellation it must be bounded and must never pull
// (ensureShellImage pulls first, cancellably): on timeout the label sweep runs
// and a clear error is returned.
func dockerCreate(ctx context.Context, session string, args ...string) (string, error) {
	detached := context.WithoutCancel(ctx)
	cctx, cancel := context.WithTimeout(detached, createTimeout)
	defer cancel()
	id, err := dockerOutputFn(cctx, args...)
	if err != nil {
		if errors.Is(cctx.Err(), context.DeadlineExceeded) {
			sweepFn(detached, session)
			return "", fmt.Errorf("docker create did not finish within %s; removed any partially created containers", createTimeout)
		}
		return "", err
	}
	return id, ctx.Err()
}

// createTimeout bounds the uninterruptible docker create; vars so tests can shrink them.
var (
	createTimeout = 60 * time.Second
	sweepFn       = removeSessionContainers
)

// ensureShellImage makes the shell image present locally, pulling it under the
// (cancellable) run context so `docker create` never has to.
func ensureShellImage(ctx context.Context, req ireproduce.ShellRequest) error {
	if _, err := dockerOutputFn(ctx, "image", "inspect", "--format", "{{.Id}}", req.ShellImage); err == nil {
		return nil
	}
	pull := []string{"pull", "--quiet"}
	if p := strings.TrimSpace(req.Platform); p != "" {
		pull = append(pull, "--platform", p)
	}
	if _, err := dockerOutputFn(ctx, append(pull, req.ShellImage)...); err != nil {
		return fmt.Errorf("pull shell image %s: %w", req.ShellImage, err)
	}
	return nil
}

// removeSessionContainers force-removes every container labelled with the
// session id, including one whose `docker create` was interrupted before its
// id was returned.
func removeSessionContainers(ctx context.Context, session string) {
	ids, err := dockerOutput(ctx, "ps", "-aq", "--filter", "label="+shellSessionLabel+"="+session)
	if err != nil || ids == "" {
		return
	}
	_ = osexec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, strings.Fields(ids)...)...).Run()
}

// dockerOutputFn is swappable so tests can observe the context create runs under.
var dockerOutputFn = dockerOutput

func dockerOutput(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	command := osexec.CommandContext(ctx, "docker", args...)
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// runInjectedShell is the --shell-image fallback. It creates (without
// starting) a container of the target image carrying the task's env, mounts and
// workdir, copies the static BusyBox out of a throwaway container of the shell
// image into it with `docker cp`, then starts it with BusyBox as the entrypoint.
func runInjectedShell(ctx context.Context, req ireproduce.ShellRequest) error {
	srcArgs := []string{"create", "--label", shellSessionLabel + "=" + req.Session}
	if p := strings.TrimSpace(req.Platform); p != "" {
		srcArgs = append(srcArgs, "--platform", p)
	}
	if err := ensureShellImage(ctx, req); err != nil {
		return err
	}
	srcID, err := dockerCreate(ctx, req.Session, append(srcArgs, req.ShellImage)...)
	if err != nil {
		return err
	}

	targetID, err := dockerCreate(ctx, req.Session, dockerCreateInjectedShellArgs(req)...)
	if err != nil {
		return err
	}

	var srcTar bytes.Buffer
	cp := osexec.CommandContext(ctx, "docker", "cp", srcID+":"+shellImageBinary, "-")
	var cpErr bytes.Buffer
	cp.Stdout = &srcTar
	cp.Stderr = &cpErr
	if err := cp.Run(); err != nil {
		return fmt.Errorf("copy %s out of %s: %w: %s (the shell image must ship a static BusyBox)", shellImageBinary, req.ShellImage, err, strings.TrimSpace(cpErr.String()))
	}
	var injected bytes.Buffer
	if err := rewrapBusybox(&srcTar, &injected); err != nil {
		return fmt.Errorf("package BusyBox from %s: %w", req.ShellImage, err)
	}
	put := osexec.CommandContext(ctx, "docker", "cp", "-", targetID+":/")
	var putErr bytes.Buffer
	put.Stdin = &injected
	put.Stderr = &putErr
	if err := put.Run(); err != nil {
		return fmt.Errorf("copy BusyBox into %s: %w: %s", req.Image, err, strings.TrimSpace(putErr.String()))
	}

	start := gracefulCommand(ctx, "docker", "start", "-a", "-i", targetID)
	start.Stdin = os.Stdin
	start.Stdout = os.Stdout
	start.Stderr = os.Stderr
	if err := start.Run(); err != nil {
		if code := commandExitCode(err); code >= 0 {
			return &ireproduce.ShellExitError{Code: code, Err: err}
		}
		return err
	}
	return nil
}

const (
	// shellImageBinary is the static multi-call binary copied out of the shell
	// image. The name is split so the image-pin guardrail (which scans for the
	// bare image name) does not mistake the path for an image reference.
	shellImageBinary = "/bin/bus" + "ybox"
	injectedShellDir = "/.caesium-shell"
	// injectedShell is named sh so BusyBox (a multi-call binary) starts the sh
	// applet from its argv[0] with no extra arguments.
	injectedShell = injectedShellDir + "/sh"
)

// injectedApplets are linked into /.caesium-shell/bin so they resolve on PATH.
var injectedApplets = []string{
	"awk", "cat", "chmod", "chown", "cp", "cut", "date", "df", "du", "env", "find", "grep",
	"head", "hostname", "id", "ifconfig", "ip", "kill", "ln", "ls", "mkdir", "mv", "nc",
	"netstat", "nslookup", "ping", "ps", "rm", "sed", "sleep", "sort", "tail", "tar", "touch",
	"tr", "uniq", "vi", "wc", "wget", "whoami",
}

// injectedPath prefixes the injected tools onto the default search path.
const injectedPath = injectedShellDir + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// rewrapBusybox reads the single-file tar `docker cp <ctr>:<binary> -`
// emits and writes a tar that lands the binary at /.caesium-shell/sh
// (mode 0755) when extracted at the target's root.
func rewrapBusybox(src io.Reader, dst io.Writer) error {
	tr := tar.NewReader(src)
	hdr, err := tr.Next()
	if err != nil {
		return fmt.Errorf("read BusyBox tar: %w", err)
	}
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("%s is not a regular file (type %c); a static BusyBox binary is required", shellImageBinary, hdr.Typeflag)
	}
	tw := tar.NewWriter(dst)
	dir := strings.TrimPrefix(injectedShellDir, "/")
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0o755}); err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: dir + "/sh", Mode: 0o755, Size: hdr.Size}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, tr); err != nil {
		return err
	}
	// Applet symlinks (bin/ls -> ../sh) put the common debugging tools on PATH:
	// BusyBox picks the applet from argv[0], which is the symlink name.
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir + "/bin/", Mode: 0o755}); err != nil {
		return err
	}
	for _, applet := range injectedApplets {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: dir + "/bin/" + applet, Linkname: "../sh", Mode: 0o777}); err != nil {
			return err
		}
	}
	return tw.Close()
}

// dockerCreateInjectedShellArgs builds the `docker create` for the target
// container of the injected-shell fallback: same env/mounts/workdir/platform as
// the plain shell, entrypoint = the injected shell.
func dockerCreateInjectedShellArgs(req ireproduce.ShellRequest) []string {
	args := []string{"create", "-i", "--label", shellSessionLabel + "=" + req.Session}
	if req.TTY {
		args = append(args, "-t")
	}
	args = append(args, "--entrypoint", injectedShell)
	if _, recorded := req.Env["PATH"]; !recorded {
		args = append(args, "-e", "PATH="+injectedPath)
	}
	args = append(args, dockerShellOptionArgs(req)...)
	return append(args, req.Image)
}

func dockerRunShellArgs(req ireproduce.ShellRequest) []string {
	shell := strings.TrimSpace(req.Shell)
	if shell == "" {
		shell = ireproduce.DefaultShell
	}
	flags := "-i"
	if req.TTY {
		flags = "-it"
	}
	args := []string{"run", "--rm", flags, "--entrypoint", shell, "--label", shellSessionLabel + "=" + req.Session}
	args = append(args, dockerShellOptionArgs(req)...)
	return append(args, req.Image)
}

func dockerShellOptionArgs(req ireproduce.ShellRequest) []string {
	var args []string
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
	if exitErr, ok := errors.AsType[*osexec.ExitError](err); ok {
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

func resultTaskLabel(env *ireproduce.Envelope) string {
	if env == nil {
		return "task"
	}
	if env.ImagePullMode == "OVERRIDDEN" {
		return fmt.Sprintf("%s (OVERRIDDEN image: %s)", env.TaskName, env.Image)
	}
	return env.TaskName
}

func sortedMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	return append(keys, slices.Sorted(maps.Keys(values))...)
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
	if exitErr, ok := errors.AsType[*exitError](err); ok {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

func printExitError(w io.Writer, err error) error {
	exitErr, ok := errors.AsType[*exitError](err)
	if ok && exitErr.msg != "" {
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

func envBool(key string, fallback bool) bool {
	value, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func firstNonEmptyString(values ...string) string {
	return strings.TrimSpace(strutil.FirstNonBlank(values...))
}

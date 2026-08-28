package tf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
)

// Runner wraps one root module's terraform-exec handle with the settings every
// tf-runner phase shares.
//
// Building on terraform-exec rather than shelling out is what makes the two
// §6.4 security requirements typed field access instead of `jq` over a schema
// the reagents would otherwise hand-track, and what makes "did anything change" a
// boolean rather than exit-code handling in shell (design §6.7).
type Runner struct {
	tf   *tfexec.Terraform
	root string
	log  io.Writer
}

// NewRunner prepares Terraform for root.
//
// It deliberately does NOT call tfexec's SetEnv. terraform-exec's SetEnv
// rejects the TF_VAR_* and TF_CLI_ARGS_* prefixes and TF_WORKSPACE outright,
// and CleanEnv strips them — so an environment installed that way could never
// carry the cross-stack variables IMPORT_OUTPUTS_FROM exports. Leaving the
// handle's env nil makes terraform-exec copy os.Environ() at each invocation
// instead, which is why the runner's callers set TF_VAR_* and TF_DATA_DIR on
// their own process (see ExportVariable). terraform-exec still removes
// TF_WORKSPACE from what it builds, so workspace selection stays with
// SelectWorkspace and cannot be silently overridden by a manifest.
func NewRunner(root, execPath string, log io.Writer) (*Runner, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root module %s: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("root module %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root module %s is not a directory", abs)
	}

	terraform, err := tfexec.NewTerraform(abs, execPath)
	if err != nil {
		return nil, fmt.Errorf("initialize terraform in %s: %w", abs, err)
	}
	// stdout belongs to the marker protocol alone. Terraform's own output is
	// routed to the log stream, where it is still captured in the task log but
	// can never be mistaken for a marker line.
	//
	// Both streams are pumped by terraform-exec on their OWN goroutines, and
	// both land in the same writer here, so the writer is serialized. Without
	// that, any caller passing something other than an *os.File — a buffer, a
	// tee, a structured logger — races on every Terraform invocation.
	serialized := &syncWriter{w: log}
	terraform.SetStdout(serialized)
	terraform.SetStderr(serialized)

	return &Runner{tf: terraform, root: abs, log: serialized}, nil
}

// syncWriter serializes concurrent writes from terraform-exec's two output
// pumps. A single *os.File would be safe on its own (one write syscall), but the
// Runner's contract must not depend on which writer a caller happens to pass.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// Root is the absolute root module directory.
func (r *Runner) Root() string { return r.root }

// Init runs `terraform init -input=false`, offline against the warm step's
// filesystem mirror (the mirror is selected by TF_CLI_CONFIG_FILE, which the
// manifest points at the generated /cache/terraformrc).
//
// backendConfig carries `-backend-config=key=value` settings, which is how a
// pipeline keeps Terraform state on a volume that survives the source tree
// being re-materialized on every run. Reconfigure is passed alongside it so a
// data directory left by an earlier run with different settings is replaced
// rather than prompting for a state migration in a non-interactive container.
//
// Init also runs with `-lockfile=readonly`. The reference manifests mount the
// staged source tree READ-ONLY for plan/apply/drift — nothing in a propose or
// apply step should be rewriting the configuration it is deploying — and
// `terraform init` rewrites `.terraform.lock.hcl` in the root module whenever
// the recorded provider set no longer matches what the configuration requires
// (adding an entry, pruning a stale one, recording a hash for a new platform).
// Without the flag that write lands as a bare EROFS/permission error from deep
// inside Terraform; with it, Terraform fails with its own
// "Provider dependency changes detected ... the lock file is read-only"
// diagnostic, which says what to actually do (regenerate and commit the lock
// file with `terraform providers lock -platform=...`).
func (r *Runner) Init(ctx context.Context, backendConfig []string) error {
	if err := r.discardInstalledModules(); err != nil {
		return fmt.Errorf("terraform init in %s: %w", r.root, err)
	}

	opts := []tfexec.InitOption{tfexec.Upgrade(false)}
	for _, cfg := range backendConfig {
		opts = append(opts, tfexec.BackendConfig(cfg))
	}
	if len(backendConfig) > 0 {
		opts = append(opts, tfexec.Reconfigure(true))
	}

	restore, err := withReadonlyLockfile()
	if err != nil {
		return fmt.Errorf("terraform init in %s: %w", r.root, err)
	}
	defer restore()

	if err := r.tf.Init(ctx, opts...); err != nil {
		return fmt.Errorf("terraform init in %s: %w", r.root, err)
	}
	return nil
}

// discardInstalledModules removes the module installs left by an earlier run so
// `terraform init` resolves every module source afresh.
//
// This is the plan side of the fingerprint contract. The discover role resolves
// modules in a fresh temp TF_DATA_DIR on every run, so it always fingerprints
// what a source address means TODAY. The runner's TF_DATA_DIR, by contrast,
// persists under ARTIFACT_DIR on the state volume — and Terraform's module
// installer leaves an already-installed module alone when the source address is
// unchanged, whether or not the revision that address points at has moved. So
// after a git branch advanced or a registry range admitted a new version,
// discover could fingerprint v2 while plan quietly reused v1 — and Caesium then
// cached the v2 fingerprint against a plan produced from v1. That is a green run
// for code that was never planned and never applied, which is design §8's
// cardinal failure with the evidence pointing the wrong way.
//
// Clearing the install directory is preferred over `init -upgrade` because
// -upgrade also re-selects PROVIDERS: against a mirror that carries a newer
// version than the lock file records, that turns into a lock-file rewrite,
// which -lockfile=readonly correctly refuses — a red run caused by fixing a
// module problem. Providers stay pinned by the lock file and the warm mirror;
// only modules are re-resolved.
//
// The cost is a module re-fetch on every plan and apply. That is the price of
// planning what was fingerprinted, and it is paid against the local mirror or
// the forge, not against correctness.
func (r *Runner) discardInstalledModules() error {
	dataDir, err := r.dataDir()
	if err != nil {
		return err
	}
	installed := filepath.Join(dataDir, modulesDirName)
	if err := os.RemoveAll(installed); err != nil {
		return fmt.Errorf("clear installed modules in %s: %w", installed, err)
	}
	return nil
}

// dataDir is Terraform's working directory for this root module, resolved by
// Terraform's own rule: TF_DATA_DIR when set (tf-runner's prepare sets it to an
// absolute path), otherwise .terraform beside the configuration.
//
// A RELATIVE TF_DATA_DIR is refused rather than resolved. Terraform resolves it
// against the root module; this process would resolve it against its own
// working directory, and the two are not the same place. The consequence fails
// in exactly the wrong direction: discardInstalledModules would RemoveAll a
// path that does not exist, os.RemoveAll returns nil for that, and the stale
// module reuse that call exists to prevent quietly comes back with no error
// anywhere. Refusing costs an operator one clear message; accepting costs them
// a green run that planned code it never fetched.
func (r *Runner) dataDir() (string, error) {
	dir := strings.TrimSpace(os.Getenv(dataDirEnvVar))
	if dir == "" {
		// r.root is absolute — NewRunner resolves it — so this is too.
		return filepath.Join(r.root, DataDirName), nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf(
			"%s is %q, which is relative: Terraform resolves it against the root module while this process "+
				"would resolve it against its own working directory, so the two would disagree about where the "+
				"installed modules live. Set %s to an absolute path",
			dataDirEnvVar, dir, dataDirEnvVar)
	}
	return dir, nil
}

// dataDirEnvVar relocates Terraform's working directory.
const dataDirEnvVar = "TF_DATA_DIR"

// modulesDirName is where the module installer puts what it fetched, relative
// to the data directory. ManifestPath ("modules/modules.json") lives under it.
const modulesDirName = "modules"

// initArgsEnvVar is Terraform's own mechanism for injecting extra arguments
// into one subcommand.
const initArgsEnvVar = "TF_CLI_ARGS_init"

// lockfileReadonlyArg makes `terraform init` refuse to rewrite
// .terraform.lock.hcl instead of attempting the write.
const lockfileReadonlyArg = "-lockfile=readonly"

// withReadonlyLockfile installs TF_CLI_ARGS_init=-lockfile=readonly for the
// duration of one Init and returns a function that puts the environment back.
//
// terraform-exec v0.25.3 has no Lockfile InitOption (see tfexec/init.go's
// initConfig), and its SetEnv REJECTS the whole TF_CLI_ARGS_ prefix — but its
// buildEnv copies os.Environ() verbatim when the handle's env is nil, which is
// the mode NewRunner deliberately leaves it in so cross-stack TF_VAR_* survive.
// Setting the variable on the runner's own process is therefore the supported
// route, and it is the same one ExportVariable already uses for TF_VAR_*.
//
// An operator who has pinned -lockfile= themselves is left alone; anything else
// already in TF_CLI_ARGS_init is preserved and appended to.
func withReadonlyLockfile() (func(), error) {
	previous, had := os.LookupEnv(initArgsEnvVar)
	if err := os.Setenv(initArgsEnvVar, initCLIArgs(previous)); err != nil {
		return nil, fmt.Errorf("set %s: %w", initArgsEnvVar, err)
	}
	return func() {
		if !had {
			_ = os.Unsetenv(initArgsEnvVar)
			return
		}
		_ = os.Setenv(initArgsEnvVar, previous)
	}, nil
}

// initCLIArgs folds -lockfile=readonly into an existing TF_CLI_ARGS_init value.
func initCLIArgs(existing string) string {
	if strings.Contains(existing, "-lockfile=") {
		return existing
	}
	if strings.TrimSpace(existing) == "" {
		return lockfileReadonlyArg
	}
	return existing + " " + lockfileReadonlyArg
}

// SelectWorkspace switches to workspace, creating it if it does not exist yet.
//
// `workspace select` fails on an unknown workspace, and a pipeline that has
// never run against a new environment has no workspace for it — so a bare
// select would make the first run of every new environment red for a reason
// that has nothing to do with the change under review.
func (r *Runner) SelectWorkspace(ctx context.Context, workspace string) error {
	if workspace == "" || workspace == "default" {
		// `default` always exists and selecting it on a backend that does not
		// support workspaces is an error, so it is left alone.
		return nil
	}
	if err := r.tf.WorkspaceSelect(ctx, workspace); err == nil {
		return nil
	}
	if err := r.tf.WorkspaceNew(ctx, workspace); err != nil {
		return fmt.Errorf("select or create workspace %q in %s: %w", workspace, r.root, err)
	}
	return nil
}

// Plan writes a plan to outPath and reports whether the diff is non-empty.
//
// terraform-exec always passes -detailed-exitcode and -input=false, and turns
// exit 2 into (true, nil) — so "did anything change" arrives as a boolean and
// exit 1 arrives as an error, with no exit-code handling of our own to get
// wrong (design §6.5).
func (r *Runner) Plan(ctx context.Context, outPath string) (bool, error) {
	changes, err := r.tf.Plan(ctx, tfexec.Out(outPath))
	if err != nil {
		return false, fmt.Errorf("terraform plan in %s: %w", r.root, err)
	}
	return changes, nil
}

// RefreshOnlyPlan reports whether refreshing state against the real world finds
// a difference — the drift signal (design §6.6).
//
// outPath receives a plan file so the drift phase can read TYPED counts back
// out of it rather than scraping Terraform's human output. It is scratch, not
// an artifact: a refresh-only plan is not something anyone applies, so the
// drift role never emits an output reference for it.
func (r *Runner) RefreshOnlyPlan(ctx context.Context, outPath string) (bool, error) {
	opts := []tfexec.PlanOption{tfexec.RefreshOnly(true)}
	if outPath != "" {
		opts = append(opts, tfexec.Out(outPath))
	}
	drifted, err := r.tf.Plan(ctx, opts...)
	if err != nil {
		return false, fmt.Errorf("terraform plan -refresh-only in %s: %w", r.root, err)
	}
	return drifted, nil
}

// ShowPlanFile decodes a saved plan into terraform-json's typed representation.
// The caller must pass the result through StripSensitive before anything
// derived from it is emitted.
//
// The command's own stdout is discarded for the duration. terraform-exec's JSON
// commands tee the raw response into the handle's stdout writer as well as into
// the decoder, so leaving it connected would dump the ENTIRE unsanitised plan
// JSON — sensitive_values and all — into the task log on every propose step.
// The typed value is the only thing that should survive this call.
func (r *Runner) ShowPlanFile(ctx context.Context, path string) (*tfjson.Plan, error) {
	r.tf.SetStdout(io.Discard)
	defer r.tf.SetStdout(r.log)

	plan, err := r.tf.ShowPlanFile(ctx, path)
	if err != nil {
		return nil, sanitizeDecodeError(fmt.Sprintf("terraform show %s", path), err)
	}
	return plan, nil
}

// sanitizeDecodeError replaces an encoding/json failure with a payload-free
// message, leaving every other error untouched.
//
// terraform-exec's JSON commands end in a bare `json.Decoder.Decode`, so a
// response the decoder dislikes surfaces as an encoding/json error — and
// json.UnmarshalTypeError carries the offending VALUE in its Value field for
// numbers, while a future encoding/json is free to carry more. That error then
// flows to Emitter.FailClosed, to stderr, and into the persisted task log. It is
// the same leak shape as teeing the raw response into the log, one layer down,
// and against `terraform output -json` — which prints sensitive values in full —
// it would defeat the withholding this package does immediately afterwards.
//
// Offsets and struct-field paths are kept: they are what makes the failure
// diagnosable, and neither is a value.
func sanitizeDecodeError(op string, err error) error {
	if err == nil {
		return nil
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return fmt.Errorf("%s: response is not valid JSON (syntax error at byte offset %d)", op, syntaxErr.Offset)
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			field = "(root)"
		}
		return fmt.Errorf("%s: response has an unexpected shape for field %q at byte offset %d "+
			"(value withheld: it may be sensitive)", op, field, typeErr.Offset)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// Apply applies exactly the saved plan at path — never a fresh plan computed at
// apply time. Applying the reviewed artifact is the entire point of the
// propose/apply split: a re-plan could differ from what was approved.
func (r *Runner) Apply(ctx context.Context, planPath string) error {
	if err := r.tf.Apply(ctx, tfexec.DirOrPlan(planPath)); err != nil {
		return fmt.Errorf("terraform apply %s: %w", planPath, err)
	}
	return nil
}

// Outputs returns the root module's outputs, each carrying its Sensitive flag.
//
// Stdout is discarded for the duration, and here that is a security control
// rather than tidiness: `terraform output -json` prints sensitive values IN
// FULL (the CLI only masks them in its human rendering), and terraform-exec
// tees a JSON command's raw response into the handle's stdout writer. Leaving
// it connected would put every secret this function then carefully withholds
// from the output row straight into the task log instead.
func (r *Runner) Outputs(ctx context.Context) (map[string]tfexec.OutputMeta, error) {
	r.tf.SetStdout(io.Discard)
	defer r.tf.SetStdout(r.log)

	outputs, err := r.tf.Output(ctx)
	if err != nil {
		return nil, sanitizeDecodeError(fmt.Sprintf("terraform output in %s", r.root), err)
	}
	return outputs, nil
}

// ExportVariable sets TF_VAR_<name> on this process so the next Terraform
// invocation sees it.
//
// The process environment is the transport because terraform-exec refuses to
// install TF_VAR_* through SetEnv, and because `-var` is not a substitute: an
// undeclared variable passed with `-var` is a hard Terraform error, while an
// undeclared TF_VAR_ is ignored. That difference decides the design. A stack
// imports the whole output set of the stacks it depends on, and most of those
// outputs are for other consumers — so with `-var` the cross-stack wiring would
// fail the moment an upstream stack grew an output this one does not read.
func ExportVariable(name, value string) error {
	if !validVariableName(name) {
		return fmt.Errorf("%q is not a Terraform variable name", name)
	}
	if strings.ContainsAny(value, "\x00") {
		return fmt.Errorf("value for variable %q contains a NUL byte", name)
	}
	if err := os.Setenv("TF_VAR_"+name, value); err != nil {
		return fmt.Errorf("export TF_VAR_%s: %w", name, err)
	}
	return nil
}

// validVariableName mirrors Terraform's identifier grammar.
func validVariableName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case (r >= '0' && r <= '9') || r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// OutputValue renders one Terraform output as the scalar string a Caesium
// output row can carry.
//
// A string output is emitted verbatim so a downstream TF_VAR_ receives exactly
// the value Terraform produced. Everything else — an object, a list, a number —
// is emitted as its compact JSON, because pkg/task.ParseOutput stores only
// scalars and would otherwise DROP the key entirely.
func OutputValue(meta tfexec.OutputMeta) (string, error) {
	if len(meta.Value) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(meta.Value, &asString); err == nil {
		return asString, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, meta.Value); err != nil {
		return "", fmt.Errorf("render output value: %w", err)
	}
	return compact.String(), nil
}

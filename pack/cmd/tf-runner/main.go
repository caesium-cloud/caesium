// Command tf-runner is the propose/apply role of the Terraform binding
// (design §6.4, §6.6). One binary, three subcommands selected by the manifest's
// `command:`:
//
//	tf-plan   propose — init offline, plan, emit a reviewable proposal
//	tf-apply  apply exactly the proposed artifact, then publish the outputs
//	tf-drift  refresh-only plan; drift makes the run red
//
// Environment (shared):
//
//	STACK_ROOT           the root module to operate on. When absent, it is taken
//	                     from CAESIUM_PARTITION_JSON's `root` attribute joined
//	                     onto SCAN_ROOT — the fan-out form, where one step
//	                     definition serves every stack.
//	SCAN_ROOT            base for the partition's relative root (fan-out form)
//	TF_WORKSPACE         workspace to select (default "default")
//	TF_CLI_CONFIG_FILE   the warm step's generated terraformrc; read by Terraform
//	TF_DATA_DIR          Terraform's working directory (default <ARTIFACT_DIR>/tfdata)
//	ARTIFACT_DIR         where the plan artifact is written (default <root>/.caesium)
//	BACKEND_CONFIG       comma-separated `-backend-config` key=value settings
//	TF_CLI_PATH          terraform binary (default: `terraform` on PATH)
//
// tf-plan:
//
//	IMPORT_OUTPUTS_FROM  comma-separated upstream apply step names. Every
//	                     CAESIUM_OUTPUT_<STEP>_<KEY> of those steps is exported as
//	                     TF_VAR_<key>. This is the cross-stack wiring, and it is
//	                     deliberately NOT terraform_remote_state: reading an
//	                     upstream stack's state would mean granting every
//	                     application stack credentials on the network stack's
//	                     state (design §6.5).
//	APPLY_STEP           when set, emit ##caesium::branch <APPLY_STEP> only if the
//	                     plan has changes — the leaf-stack branch form (§6.4).
//
// tf-apply:
//
//	PLAN_STEP            the plan step whose proposal to apply. Its
//	                     CAESIUM_OUTPUT_<PLAN>_PROPOSAL_* values locate the
//	                     summary and the artifact.
//
// Every phase fails closed: an error exits non-zero having emitted no marker at
// all, because an absent result must never be read as "nothing changed"
// (design §5.2, §8).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
	"github.com/caesium-cloud/caesium/pack/internal/tf"
)

// planFileName is the saved plan every stack's propose phase writes and its
// apply phase consumes. It is stable per stack so the artifact reference — and
// therefore the apply step's cache key — does not churn on a path.
const planFileName = "tf.plan"

// subcommands is the phase table. It is data rather than a switch so the usage
// message and the dispatch can never disagree about what exists.
var subcommands = map[string]func(context.Context, config, *protocol.Emitter, io.Writer) error{
	"tf-plan":  runPlan,
	"tf-apply": runApply,
	"tf-drift": runDrift,
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "tf-runner: a subcommand is required (one of %s)\n", strings.Join(subcommandNames(), ", "))
		os.Exit(1)
	}
	name := os.Args[1]
	phase, ok := subcommands[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "tf-runner: unknown subcommand %q (want one of %s)\n", name, strings.Join(subcommandNames(), ", "))
		os.Exit(1)
	}

	protocol.Run(name, func(e *protocol.Emitter) error {
		cfg, err := loadConfig(os.Getenv)
		if err != nil {
			return err
		}
		return phase(context.Background(), cfg, e, os.Stderr)
	})
}

func subcommandNames() []string {
	return protocol.SortedKeys(subcommands)
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	Root              string
	Workspace         string
	DataDir           string
	ArtifactDir       string
	ExecPath          string
	BackendConfig     []string
	ImportOutputsFrom []string
	ApplyStep         string
	PlanStep          string
}

func loadConfig(getenv func(string) string) (config, error) {
	root, err := resolveRoot(getenv)
	if err != nil {
		return config{}, err
	}

	cfg := config{
		Root:              root,
		Workspace:         strings.TrimSpace(getenv("TF_WORKSPACE")),
		DataDir:           strings.TrimSpace(getenv("TF_DATA_DIR")),
		ArtifactDir:       strings.TrimSpace(getenv("ARTIFACT_DIR")),
		ExecPath:          strings.TrimSpace(getenv("TF_CLI_PATH")),
		BackendConfig:     splitList(getenv("BACKEND_CONFIG")),
		ImportOutputsFrom: splitList(getenv("IMPORT_OUTPUTS_FROM")),
		ApplyStep:         strings.TrimSpace(getenv("APPLY_STEP")),
		PlanStep:          strings.TrimSpace(getenv("PLAN_STEP")),
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "default"
	}
	if cfg.ArtifactDir == "" {
		cfg.ArtifactDir = filepath.Join(cfg.Root, ".caesium")
	}
	if cfg.DataDir == "" {
		// Terraform's data directory is relocated out of the stack's top level
		// on purpose. `terraform init` writes .terraform/ wherever this points,
		// and a .terraform/ sitting beside the configuration is a directory the
		// discover role would then have to know to ignore. Keeping it under the
		// artifact directory also means plan and apply share one initialized
		// working directory within a run without either of them writing into
		// the configuration itself.
		cfg.DataDir = filepath.Join(cfg.ArtifactDir, "tfdata")
	}
	for _, setting := range cfg.BackendConfig {
		if !strings.Contains(setting, "=") {
			return config{}, fmt.Errorf("BACKEND_CONFIG entry %q is not key=value", setting)
		}
	}
	if cfg.ExecPath == "" {
		path, err := exec.LookPath("terraform")
		if err != nil {
			return config{}, fmt.Errorf("terraform binary not found on PATH: %w", err)
		}
		cfg.ExecPath = path
	}
	return cfg, nil
}

// resolveRoot finds the root module this instance operates on.
//
// The hand-written form names it outright. The fan-out form cannot: one step
// definition serves every stack, so the directory arrives per instance in
// CAESIUM_PARTITION_JSON, in the free-form `root` attribute the discover role
// puts there. It is joined onto SCAN_ROOT rather than used as-is because
// discover emits it relative to the scan root — a partition that carried an
// absolute path would depend on where the volume happened to be mounted.
func resolveRoot(getenv func(string) string) (string, error) {
	if root := strings.TrimSpace(getenv("STACK_ROOT")); root != "" {
		return root, nil
	}

	raw := strings.TrimSpace(getenv("CAESIUM_PARTITION_JSON"))
	if raw == "" {
		return "", fmt.Errorf("STACK_ROOT is required (or CAESIUM_PARTITION_JSON with a `root` attribute when fanned out)")
	}
	key, rel, err := partitionRoot(raw)
	if err != nil {
		return "", err
	}
	if rel == "" {
		return "", fmt.Errorf("partition %q carries no `root` attribute; the discover role must name each unit's directory", key)
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("partition %q has the root %q; roots must be relative to SCAN_ROOT and stay inside it", key, rel)
	}
	scanRoot := strings.TrimSpace(getenv("SCAN_ROOT"))
	if scanRoot == "" {
		return "", fmt.Errorf("SCAN_ROOT is required when the root module comes from CAESIUM_PARTITION_JSON")
	}
	return filepath.Join(scanRoot, filepath.FromSlash(rel)), nil
}

// partitionRoot reads the partition key and its `root` attribute out of
// CAESIUM_PARTITION_JSON.
//
// The object is Caesium's canonical partition encoding: `key`, an optional
// `fingerprint`, an optional `dependsOn` array, and the discover role's
// free-form scalar attributes flattened alongside them. Only two fields are
// read here, so the decode is deliberately narrow rather than a mirror of the
// server's type — the pack must not grow a second, drifting copy of a Caesium
// struct (the contract between them is the marker protocol, not a Go API).
func partitionRoot(raw string) (key, root string, err error) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return "", "", fmt.Errorf("CAESIUM_PARTITION_JSON is not a JSON object: %w", err)
	}
	if s, ok := obj["key"].(string); ok {
		key = s
	}
	if key == "" {
		return "", "", fmt.Errorf("CAESIUM_PARTITION_JSON carries no partition key")
	}
	if s, ok := obj["root"].(string); ok {
		root = strings.TrimSpace(s)
	}
	return key, root, nil
}

func splitList(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\t'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// prepare installs the process environment Terraform inherits and returns a
// ready Runner. It is shared by all three phases so none of them can drift into
// a different data directory or a different mirror.
func prepare(cfg config, log io.Writer) (*tf.Runner, error) {
	if err := os.MkdirAll(cfg.ArtifactDir, 0o755); err != nil {
		return nil, fmt.Errorf("create artifact directory %s: %w", cfg.ArtifactDir, err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create terraform data directory %s: %w", cfg.DataDir, err)
	}
	absData, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve terraform data directory %s: %w", cfg.DataDir, err)
	}
	if err := os.Setenv("TF_DATA_DIR", absData); err != nil {
		return nil, fmt.Errorf("set TF_DATA_DIR: %w", err)
	}
	return tf.NewRunner(cfg.Root, cfg.ExecPath, log)
}

// ---------------------------------------------------------------------------
// tf-plan (propose)
// ---------------------------------------------------------------------------

func runPlan(ctx context.Context, cfg config, e *protocol.Emitter, log io.Writer) error {
	runner, err := prepare(cfg, log)
	if err != nil {
		return err
	}
	imported, err := exportImportedOutputs(cfg.ImportOutputsFrom, os.Environ())
	if err != nil {
		return err
	}
	if len(imported) > 0 {
		_, _ = fmt.Fprintf(log, "tf-plan: imported %d upstream outputs as TF_VAR_* from %v\n", len(imported), cfg.ImportOutputsFrom)
	}

	if err := runner.Init(ctx, cfg.BackendConfig); err != nil {
		return err
	}
	if err := runner.SelectWorkspace(ctx, cfg.Workspace); err != nil {
		return err
	}

	planPath := filepath.Join(cfg.ArtifactDir, planFileName)
	changes, err := runner.Plan(ctx, planPath)
	if err != nil {
		return err
	}

	if !changes {
		// A zero-count summary, no artifact and no branch marker. The summary is
		// still emitted: the apply step reads it to decide there is nothing to
		// do, and the Console renders "no changes" rather than an empty panel.
		summary, err := tf.Summary{Resources: []tf.ResourceChange{}}.Encode()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(log, "tf-plan: %s has no changes\n", runner.Root())
		return e.Output(map[string]string{
			"proposal_kind":    tf.ProposalKind,
			"proposal_summary": summary,
		})
	}

	plan, err := runner.ShowPlanFile(ctx, planPath)
	if err != nil {
		return err
	}
	// Strip BEFORE summarizing. Both halves of §6.4's plan-side requirement live
	// on this line: the plan JSON that reaches anything we emit carries no
	// sensitive_values, and the summary is computed from the already-stripped
	// object rather than from the original.
	summary, err := tf.SummarizePlan(tf.StripSensitive(plan)).Encode()
	if err != nil {
		return err
	}

	if err := e.Output(map[string]string{
		"proposal_kind":     tf.ProposalKind,
		"proposal_summary":  summary,
		"proposal_artifact": "plan",
	}); err != nil {
		return err
	}
	// The artifact reference carries the digest the apply step verifies before
	// it applies anything, and the digest is what makes a downstream cache hit
	// value-verified rather than path-based.
	if err := e.OutputRef("plan", planPath); err != nil {
		return err
	}

	if cfg.ApplyStep != "" {
		// The branch form, for a leaf stack: the apply step is skipped by the
		// DAG when there is nothing to apply, so the run visibly shows it as
		// skipped rather than as a container that succeeded doing nothing.
		if err := e.Branch(cfg.ApplyStep); err != nil {
			return err
		}
	}
	return nil
}

// exportImportedOutputs turns every CAESIUM_OUTPUT_<STEP>_<KEY> of the named
// upstream steps into TF_VAR_<key>, and returns the variable names it set.
//
// A companion _DIGEST variable is skipped: it belongs to an output reference,
// whose base key already carries the path, and a digest is not a value any
// Terraform variable wants.
func exportImportedOutputs(steps []string, environ []string) ([]string, error) {
	if len(steps) == 0 {
		return nil, nil
	}
	prefixes := make([]string, 0, len(steps))
	for _, step := range steps {
		prefixes = append(prefixes, "CAESIUM_OUTPUT_"+normalizeStepName(step)+"_")
	}

	var exported []string
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for _, prefix := range prefixes {
			rest, found := strings.CutPrefix(key, prefix)
			if !found || rest == "" {
				continue
			}
			if strings.HasSuffix(rest, "_DIGEST") {
				break
			}
			name := strings.ToLower(rest)
			if err := tf.ExportVariable(name, value); err != nil {
				return nil, err
			}
			exported = append(exported, name)
			break
		}
	}
	return exported, nil
}

// normalizeStepName mirrors pkg/task.NormalizeStepName, which is how Caesium
// spells a step name inside an environment variable. The pack cannot import
// Caesium (the contract between them is the marker protocol, not a Go API), so
// the rule is restated here; test/infra_deploy_test.go drives the real server,
// which is what would catch a divergence.
func normalizeStepName(name string) string {
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, ".", "_")
	return strings.ToUpper(name)
}

// ---------------------------------------------------------------------------
// tf-apply
// ---------------------------------------------------------------------------

func runApply(ctx context.Context, cfg config, e *protocol.Emitter, log io.Writer) error {
	if cfg.PlanStep == "" {
		return fmt.Errorf("PLAN_STEP is required: tf-apply applies the artifact a named plan step proposed")
	}
	proposal, err := readProposal(cfg.PlanStep, os.Getenv)
	if err != nil {
		return err
	}

	runner, err := prepare(cfg, log)
	if err != nil {
		return err
	}
	// init runs in both branches. `terraform output` needs an initialized
	// working directory, and the empty-plan branch must still publish outputs.
	if err := runner.Init(ctx, cfg.BackendConfig); err != nil {
		return err
	}
	if err := runner.SelectWorkspace(ctx, cfg.Workspace); err != nil {
		return err
	}

	if proposal.Summary.HasChanges() {
		if err := verifyArtifact(proposal); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(log, "tf-apply: applying %s (%d add, %d change, %d destroy, %d replace)\n",
			proposal.ArtifactPath, proposal.Summary.Add, proposal.Summary.Change,
			proposal.Summary.Destroy, proposal.Summary.Replace)
		if err := runner.Apply(ctx, proposal.ArtifactPath); err != nil {
			return err
		}
	} else {
		// The container no-op form: no per-unit edge exists to suppress when
		// the apply step is a fan-out group, so the decision lives in the
		// image. Terraform is not invoked at all.
		_, _ = fmt.Fprintf(log, "tf-apply: %s proposed no changes; not invoking terraform apply\n", cfg.PlanStep)
	}

	// Outputs are published in BOTH branches, always. A stack whose own plan was
	// empty is still the producer every downstream stack reads its inputs from;
	// an apply that emitted nothing because it had nothing to do would look
	// identical to a stack that vanished, and the cascade would re-plan (or
	// fail) every consumer.
	outputs, err := runner.Outputs(ctx)
	if err != nil {
		return err
	}
	values, withheld, err := tf.PublishableOutputs(outputs)
	if err != nil {
		return err
	}
	if len(withheld) > 0 {
		_, _ = fmt.Fprintf(log, "tf-apply: withheld %d sensitive output(s): %v\n", len(withheld), withheld)
	}
	if len(values) == 0 {
		// A stack with no non-sensitive outputs has nothing to publish, and the
		// emitter rejects an empty output map. Nothing downstream can be reading
		// this stack's values, so there is nothing to keep alive.
		_, _ = fmt.Fprintf(log, "tf-apply: %s exports no publishable outputs\n", runner.Root())
		return nil
	}
	return e.Output(values)
}

// proposal is what a plan step told this apply step.
type proposal struct {
	Kind         string
	Summary      tf.Summary
	ArtifactKey  string
	ArtifactPath string
	Digest       string
}

// readProposal reconstructs the proposal from the plan step's outputs as
// Caesium exposes them: CAESIUM_OUTPUT_<PLAN>_PROPOSAL_SUMMARY and friends,
// plus CAESIUM_OUTPUT_<PLAN>_<ARTIFACT>/_DIGEST for the reference.
func readProposal(planStep string, getenv func(string) string) (proposal, error) {
	prefix := "CAESIUM_OUTPUT_" + normalizeStepName(planStep) + "_"

	encoded := strings.TrimSpace(getenv(prefix + "PROPOSAL_SUMMARY"))
	if encoded == "" {
		return proposal{}, fmt.Errorf(
			"%sPROPOSAL_SUMMARY is empty: step %q produced no proposal, so there is nothing to apply",
			prefix, planStep)
	}
	summary, err := tf.DecodeSummary(encoded)
	if err != nil {
		return proposal{}, fmt.Errorf("proposal from step %q: %w", planStep, err)
	}

	p := proposal{
		Kind:        strings.TrimSpace(getenv(prefix + "PROPOSAL_KIND")),
		Summary:     summary,
		ArtifactKey: strings.TrimSpace(getenv(prefix + "PROPOSAL_ARTIFACT")),
	}
	if p.Kind != "" && p.Kind != tf.ProposalKind {
		return proposal{}, fmt.Errorf("step %q proposed %q, which tf-apply cannot apply (want %q)",
			planStep, p.Kind, tf.ProposalKind)
	}
	if !summary.HasChanges() {
		return p, nil
	}
	if p.ArtifactKey == "" {
		return proposal{}, fmt.Errorf(
			"step %q proposed changes but named no artifact; refusing to apply a plan that was never produced", planStep)
	}
	artifactPrefix := prefix + normalizeStepName(p.ArtifactKey)
	p.ArtifactPath = strings.TrimSpace(getenv(artifactPrefix))
	p.Digest = strings.TrimSpace(getenv(artifactPrefix + "_DIGEST"))
	if p.ArtifactPath == "" || p.Digest == "" {
		return proposal{}, fmt.Errorf(
			"step %q named the artifact %q but %s/%s_DIGEST are not both set; the plan file cannot be verified",
			planStep, p.ArtifactKey, artifactPrefix, artifactPrefix)
	}
	return p, nil
}

// verifyArtifact re-hashes the plan file and compares it to the digest the plan
// step reported.
//
// The plan file lives on a shared volume that other steps can write to, and it
// is the thing that decides what happens to real infrastructure. Applying it
// without checking that it is still the reviewed bytes would make the whole
// propose/apply split decorative.
func verifyArtifact(p proposal) error {
	f, err := os.Open(p.ArtifactPath) //nolint:gosec // the path came from the plan step's own output reference.
	if err != nil {
		return fmt.Errorf("open plan artifact %s: %w", p.ArtifactPath, err)
	}
	defer func() { _ = f.Close() }()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Errorf("read plan artifact %s: %w", p.ArtifactPath, err)
	}
	got := protocol.DigestPrefix + hex.EncodeToString(sum.Sum(nil))
	if got != p.Digest {
		return fmt.Errorf(
			"plan artifact %s does not match the proposal: proposed %s, found %s. "+
				"The reviewed plan is not the plan on disk; refusing to apply",
			p.ArtifactPath, p.Digest, got)
	}
	return nil
}

// ---------------------------------------------------------------------------
// tf-drift
// ---------------------------------------------------------------------------

func runDrift(ctx context.Context, cfg config, e *protocol.Emitter, log io.Writer) error {
	runner, err := prepare(cfg, log)
	if err != nil {
		return err
	}
	if _, err := exportImportedOutputs(cfg.ImportOutputsFrom, os.Environ()); err != nil {
		return err
	}
	if err := runner.Init(ctx, cfg.BackendConfig); err != nil {
		return err
	}
	if err := runner.SelectWorkspace(ctx, cfg.Workspace); err != nil {
		return err
	}

	// The refresh-only plan goes to scratch, never to the artifact directory:
	// nothing applies a drift plan, so publishing it as an artifact would offer
	// a reviewable object that must not be reviewed.
	scratch, err := os.MkdirTemp("", "tf-drift-")
	if err != nil {
		return fmt.Errorf("create scratch directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	planPath := filepath.Join(scratch, "refresh.plan")

	drifted, err := runner.RefreshOnlyPlan(ctx, planPath)
	if err != nil {
		return err
	}
	if !drifted {
		_, _ = fmt.Fprintf(log, "tf-drift: %s matches its state\n", runner.Root())
		return e.Output(map[string]string{"drift": "false"})
	}

	plan, err := runner.ShowPlanFile(ctx, planPath)
	if err != nil {
		return err
	}
	summary, err := tf.SummarizeDrift(tf.StripSensitive(plan)).Encode()
	if err != nil {
		return err
	}
	if err := e.Output(map[string]string{"drift": "true", "drift_summary": summary}); err != nil {
		return err
	}
	// Emit, THEN fail. The output is what the Console and a notification render;
	// the non-zero exit is what makes the task and the run red so the shipped
	// notification, callback and metadata.remediation paths fire at all. A drift
	// job that stayed green would be a drift job nobody ever noticed.
	if err := e.Flush(); err != nil {
		return err
	}
	return errDrift
}

// errDrift is the sentinel the drift phase fails with after emitting. It is
// returned rather than calling os.Exit so the emitter's own flush ordering
// stays in one place.
var errDrift = errors.New("drift detected: the real world no longer matches state")

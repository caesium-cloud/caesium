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
//	                     TF_VAR_<original>. Original names come from the JSON
//	                     name index (CAESIUM_OUTPUT_NAME_INDEX_<STEP>) when
//	                     present, otherwise from lowercasing the folded suffix.
//	                     A producer that declares the index required fails if an
//	                     old server did not provide it. This is the cross-stack
//	                     wiring, and it is
//	                     deliberately NOT terraform_remote_state: reading an
//	                     upstream stack's state would mean granting every
//	                     application stack credentials on the network stack's
//	                     state (design §6.5). EVERY named step must actually be
//	                     present — a name that imports nothing fails the phase
//	                     rather than planning against variable defaults.
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
	"sort"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/reagents/internal/protocol"
	"github.com/caesium-cloud/caesium/reagents/internal/tf"
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
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("partition %q has the root %q; roots must be relative to SCAN_ROOT and stay inside it", key, rel)
	}
	scanRoot := strings.TrimSpace(getenv("SCAN_ROOT"))
	if scanRoot == "" {
		return "", fmt.Errorf("SCAN_ROOT is required when the root module comes from CAESIUM_PARTITION_JSON")
	}
	root, err := containedPath(scanRoot, filepath.FromSlash(rel))
	if err != nil {
		return "", fmt.Errorf("partition %q has the root %q: %w", key, rel, err)
	}
	return root, nil
}

// containedPath joins rel onto base and proves the result is still inside base.
//
// A leading-".." test is not that proof. `filepath.Join` CLEANS, so
// "stack/../../state" begins with a directory name and still lands outside the
// scan root — and an in-tree symlink escapes without any ".." at all. Either
// one makes tf-plan/tf-apply operate on a different mounted tree than the one
// discover enumerated, which is the "deployed the wrong thing" failure of
// design §8 reached through a malformed partition payload.
//
// So containment is proved positively, with filepath.Rel over cleaned absolute
// paths: the relative step from base to the target may not begin with a ".."
// segment. Symlinks are resolved on BOTH paths or on neither — resolving only
// one of them would report a legitimate root as an escape whenever the mount
// point itself is a symlink and the stack directory does not exist yet.
//
// The value returned is the lexically joined path, not the symlink-resolved
// one: the runner must operate on the tree as it was mounted.
func containedPath(base, rel string) (string, error) {
	joined := filepath.Join(base, rel)

	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("resolve SCAN_ROOT %s: %w", base, err)
	}
	absTarget, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", joined, err)
	}
	resolvedBase, baseErr := filepath.EvalSymlinks(absBase)
	resolvedTarget, targetErr := filepath.EvalSymlinks(absTarget)
	if baseErr == nil && targetErr == nil {
		absBase, absTarget = resolvedBase, resolvedTarget
	}

	step, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return "", fmt.Errorf("%s is not reachable from SCAN_ROOT %s: %w", joined, base, err)
	}
	if step == ".." || strings.HasPrefix(step, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf(
			"it resolves to %s, which is outside SCAN_ROOT %s; roots must stay inside the scanned tree",
			absTarget, absBase)
	}
	return joined, nil
}

// partitionRoot reads the partition key and its `root` attribute out of
// CAESIUM_PARTITION_JSON.
//
// The object is Caesium's canonical partition encoding: `key`, an optional
// `fingerprint`, an optional `dependsOn` array, and the discover role's
// free-form scalar attributes flattened alongside them. Only two fields are
// read here, so the decode is deliberately narrow rather than a mirror of the
// server's type — the reagents must not grow a second, drifting copy of a Caesium
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
	if err := pinGitIdentity(); err != nil {
		return nil, err
	}
	return tf.NewRunner(cfg.Root, cfg.ExecPath, log)
}

// pinGitIdentity installs an inert git identity if the environment has none.
//
// Every phase re-resolves its modules (tf.Runner.discardInstalledModules), so a
// stack with a git module source shells out to git on every init. git
// synthesizes an identity for the clone's reflog by resolving the machine's own
// hostname when none is configured, and in a pod with no DNS entry for its name
// that lookup stalls until the resolver gives up — measured at ~5 s per fetch on
// a network-isolated container — or fails outright, on a module install that has
// nothing to do with identity. Pinning an inert one removes the lookup; nothing
// here ever creates a commit. Same reasoning, and the same values in spirit, as
// tf-discover's terraformEnv and git-source's gitEnv.
func pinGitIdentity() error {
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "caesium tf-runner"},
		{"GIT_AUTHOR_EMAIL", "tf-runner@caesium.invalid"},
		{"GIT_COMMITTER_NAME", "caesium tf-runner"},
		{"GIT_COMMITTER_EMAIL", "tf-runner@caesium.invalid"},
	} {
		if _, set := os.LookupEnv(kv[0]); set {
			continue
		}
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			return fmt.Errorf("set %s: %w", kv[0], err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// tf-plan (propose)
// ---------------------------------------------------------------------------

func runPlan(ctx context.Context, cfg config, e *protocol.Emitter, log io.Writer) error {
	runner, err := prepare(cfg, log)
	if err != nil {
		return err
	}
	imported, err := exportImportedOutputs(cfg.ImportOutputsFrom, os.Environ(), log)
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
		summary, err := tf.Summary{Resources: []tf.ResourceChange{}}.WithChanges(false).Encode()
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
	// WithChanges carries Terraform's own -detailed-exitcode answer, so the
	// apply step's decision to invoke Terraform can no longer disagree with the
	// plan step's decision to produce an artifact.
	summary, err := tf.SummarizePlan(tf.StripSensitive(plan)).WithChanges(changes).Encode()
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
// upstream steps into TF_VAR_<original>, and returns the variable names it set.
//
// Original names come from the JSON name index Caesium publishes as
// CAESIUM_OUTPUT_NAME_INDEX_<STEP>. Without an index the suffix is lowercased,
// which is lossless for the historical snake_case contract. A tf-apply that
// publishes a mixed-case or dashed name also emits an index-required sentinel;
// if that sentinel arrives without the dedicated index, the server is too old
// for the producer and import fails rather than guessing a lowercase name.
//
// A companion _DIGEST variable is skipped: it belongs to an output reference,
// whose base key already carries the path, and a digest is not a value any
// Terraform variable wants.
func exportImportedOutputs(steps []string, environ []string, log io.Writer) ([]string, error) {
	if len(steps) == 0 {
		return nil, nil
	}

	named := make(map[string]string, len(steps))
	for _, step := range steps {
		prefix := stepEnvPrefix(step)
		if existing, dup := named[prefix]; dup {
			return nil, fmt.Errorf("IMPORT_OUTPUTS_FROM names %q and %q, which normalize to the same environment prefix %s",
				existing, step, prefix)
		}
		named[prefix] = step
	}
	// Two NAMED steps whose prefixes nest ("apply-network" and
	// "apply-network-2") cannot be told apart by the shorter one's own
	// iteration, so refuse rather than guess.
	for prefix, step := range named {
		for other, otherStep := range named {
			if prefix != other && strings.HasPrefix(other, prefix) {
				return nil, fmt.Errorf(
					"IMPORT_OUTPUTS_FROM names both %q and %q: %q's environment prefix contains %q's, "+
						"so their outputs cannot be told apart", step, otherStep, otherStep, step)
			}
		}
	}

	present := make(map[string]struct{}, len(environ))
	envValues := make(map[string]string, len(environ))
	for _, kv := range environ {
		if key, value, ok := strings.Cut(kv, "="); ok {
			present[key] = struct{}{}
			envValues[key] = value
		}
	}
	if err := requireProducers(named, present); err != nil {
		return nil, err
	}
	// Every OTHER step whose outputs are in this environment, identified
	// exactly. See discoverStepPrefixes: this is what makes the match
	// step-exact instead of merely prefix-based.
	siblings := discoverStepPrefixes(present, named)
	indexes, err := outputNamesIndexes(named, envValues, siblings)
	if err != nil {
		return nil, err
	}

	type source struct{ step, envKey string }
	exported := make(map[string]source)
	for _, kv := range environ {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		for prefix, step := range named {
			rest, found := strings.CutPrefix(key, prefix)
			if !found || rest == "" {
				continue
			}
			// The key belongs to a LONGER step name that merely starts with
			// this one. Without this, `IMPORT_OUTPUTS_FROM=apply-network` also
			// imports `apply-network-extra`'s outputs — silently, because
			// `extra_foo` is a perfectly good Terraform identifier and an
			// undeclared TF_VAR_ is ignored.
			if owner, taken := longestOwner(siblings, key); taken && owner != prefix {
				break
			}
			// A companion _DIGEST belongs to an output reference whose base key
			// carries the path; it is not a value any Terraform variable wants.
			// The base key has to actually exist, or a legitimate output named
			// `foo_digest` would be swallowed.
			if isSyntheticOutputDigest(prefix, rest, envValues, indexes[prefix]) {
				break
			}
			// Protocol keys are never Terraform variables. Every tf-apply emits
			// the published-count sentinel, and a producer with non-fold-stable
			// names emits the index-required sentinel. Without this skip a
			// diamond import collides with itself on keys the operator never
			// wrote. The generated index lives on
			// CAESIUM_OUTPUT_NAME_INDEX_<STEP>, outside this suffix space;
			// caesium_output_names remains an ordinary application suffix.
			if isProtocolOutputSuffix(rest) {
				break
			}
			name := importedOutputName(rest, indexes[prefix])
			if prior, dup := exported[name]; dup {
				// Last-write-wins here would resolve a real ambiguity by
				// os.Environ() ordering — an implementation detail of whichever
				// engine formatted the environment — and log nothing.
				return nil, fmt.Errorf(
					"IMPORT_OUTPUTS_FROM: %s and %s both map to TF_VAR_%s; "+
						"rename one of the outputs or import only one of the steps",
					prior.envKey, key, name)
			}
			if err := tf.ExportVariable(name, value); err != nil {
				return nil, fmt.Errorf("importing %s from step %q: %w", key, step, err)
			}
			exported[name] = source{step: step, envKey: key}
			break
		}
	}

	names := protocol.SortedKeys(exported)
	// Logged individually because the failure mode this guards against is
	// silent: an undeclared TF_VAR_ is ignored by Terraform, so a variable that
	// arrived under the wrong name produces no error anywhere — only a stack
	// planned against a default. The log is the one place an operator can see
	// what actually crossed the boundary.
	//
	// Original names are recovered from the JSON index when one was published;
	// otherwise the suffix is lowercased. The log is the one place an operator
	// can see which TF_VAR_ actually crossed, including that vpcId did not
	// become vpcid.
	for _, name := range names {
		_, _ = fmt.Fprintf(log, "tf-runner: TF_VAR_%s <- %s (step %q)\n", name, exported[name].envKey, exported[name].step)
	}
	return names, nil
}

// requireProducers refuses to import from a step whose outputs are not in this
// environment at all.
//
// Importing nothing used to be a success. A misspelled step name, a step that
// is not actually a predecessor, or a producer that was unexpectedly skipped
// therefore exported zero variables — and because an undeclared TF_VAR_ is
// silently ignored by Terraform (the very property that makes env the right
// transport, see tf.ExportVariable), the consuming stack simply planned against
// its variables' defaults. That is a green deployment built on cross-stack
// inputs nobody supplied: design §8's cardinal failure, reached by a spelling
// mistake.
//
// Presence is decided by the sentinel, not by "has any key with this prefix":
// every tf-apply publishes caesium_outputs_published unconditionally (see
// runApply), including when every one of its outputs was withheld as sensitive.
// A prefix scan would instead read a producer with nothing publishable as
// absent, and a producer whose normalized name merely EXTENDS this one as
// present.
func requireProducers(named map[string]string, present map[string]struct{}) error {
	sentinel := normalizeStepName(publishedCountKey)
	var missing []string
	for prefix, step := range named {
		if _, ok := present[prefix+sentinel]; !ok {
			missing = append(missing, step)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf(
		"IMPORT_OUTPUTS_FROM names %s, whose outputs are not in this step's environment "+
			"(no %s%s was published). Check the step name is spelled exactly as in the manifest, that it is a "+
			"predecessor of this step, and that it is a tf-apply step; importing nothing would leave this stack "+
			"planning against its variables' defaults",
		quotedList(missing), stepEnvPrefix(missing[0]), sentinel)
}

// quotedList renders names for an error message: `"a"`, `"a" and "b"`,
// `"a", "b" and "c"`.
func quotedList(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
	}
}

// stepEnvPrefix is the environment prefix Caesium gives one step's outputs.
func stepEnvPrefix(step string) string {
	return "CAESIUM_OUTPUT_" + normalizeStepName(step) + "_"
}

// discoverStepPrefixes identifies, exactly, every step whose outputs are in this
// environment, by looking for the published-count sentinel.
//
// Caesium provides no list of predecessor step names, and a single
// CAESIUM_OUTPUT_<STEP>_<KEY> variable cannot be split back into step and key —
// both halves are uppercased with "_" separators, so the boundary is not
// recoverable from one name. The sentinel recovers it: EVERY tf-apply publishes
// caesium_outputs_published, so a key ending in that suffix names its step
// exactly, and everything before the suffix is that step's prefix.
//
// That is what turns "starts with the named step's prefix" into "belongs to the
// named step". RESIDUAL: a producer that is not a tf-apply publishes no
// sentinel, so a hypothetical non-apply sibling whose normalized name extends a
// named step's would still be matched by prefix. IMPORT_OUTPUTS_FROM names
// upstream APPLY steps by contract, so every real sibling is discoverable; the
// per-variable log above is the backstop for anything else.
func discoverStepPrefixes(present map[string]struct{}, named map[string]string) map[string]struct{} {
	suffix := "_" + normalizeStepName(publishedCountKey)
	prefixes := make(map[string]struct{}, len(present))
	for key := range present {
		if !strings.HasPrefix(key, "CAESIUM_OUTPUT_") {
			continue
		}
		stepPrefix, ok := strings.CutSuffix(key, suffix)
		if !ok {
			continue
		}
		prefixes[stepPrefix+"_"] = struct{}{}
	}
	for prefix := range named {
		prefixes[prefix] = struct{}{}
	}
	return prefixes
}

// longestOwner returns the longest known step prefix that key starts with.
func longestOwner(prefixes map[string]struct{}, key string) (string, bool) {
	best := ""
	for prefix := range prefixes {
		if strings.HasPrefix(key, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	return best, best != ""
}

// normalizeStepName mirrors pkg/task.NormalizeStepName, which is how Caesium
// spells a step name inside an environment variable. The reagents cannot import
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
		if err := applyProposal(ctx, cfg, runner, proposal, log); err != nil {
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
	// The row is published unconditionally, even when every output was withheld.
	//
	// The dangerous case is not a stack that never had outputs; it is a stack
	// that STOPS having publishable ones — someone marks the last non-sensitive
	// output `sensitive = true`. Emitting nothing then makes a vanished VALUE
	// indistinguishable from a vanished STACK: the consumer's TF_VAR_ simply
	// disappears, and because an undeclared TF_VAR_ is silently ignored (the
	// very property that makes env the right transport, see tf.ExportVariable),
	// the consumer plans against the variable's default and the run is green.
	// The count is the sentinel that keeps the row non-empty and makes the
	// change visible to a consumer's cache key.
	if _, taken := values[publishedCountKey]; taken {
		return fmt.Errorf("output %q is reserved by tf-apply; rename it", publishedCountKey)
	}
	needsNameIndex := tf.OutputNamesNeedIndex(values)
	values[publishedCountKey] = strconv.Itoa(len(values))
	// A mixed/dashed name requires server support for the dedicated index.
	// Persist the requirement after the count so neither protocol key changes
	// caesium_outputs_published. An old server still forwards this sentinel,
	// allowing a new consumer to fail explicitly instead of lowercasing names.
	if needsNameIndex {
		values[tf.OutputNamesIndexRequiredKey] = "1"
	}
	return e.Output(values)
}

// publishedCountKey is the reserved output key carrying how many of the stack's
// outputs were published. It exists so the output row is never empty (see
// runApply) and so a stack that stops publishing a value moves its consumers'
// cache keys instead of silently starving them.
const publishedCountKey = "caesium_outputs_published"

// isProtocolOutputSuffix reports whether rest (the folded env suffix after
// CAESIUM_OUTPUT_<STEP>_) is a tf-apply protocol key, never a Terraform
// variable.
func isProtocolOutputSuffix(rest string) bool {
	switch rest {
	case normalizeStepName(publishedCountKey), normalizeStepName(tf.OutputNamesIndexRequiredKey):
		return true
	default:
		return false
	}
}

// outputNamesIndexes reads and verifies each named step's dedicated JSON name
// index. Once present, an index is authoritative: it must describe every
// transported output suffix exactly once, carry no extras, and map each suffix
// back to an original name that folds to that suffix. A partial index never
// falls through to lowercase recovery.
//
// tf-apply persists OutputNamesIndexRequiredKey when mixed-case or dashed names
// make an index mandatory. An old server forwards that sentinel but cannot
// synthesize CAESIUM_OUTPUT_NAME_INDEX_<STEP>; detecting that combination here
// turns an incompatible rolling upgrade into an explicit failure instead of a
// green plan against a lowercased variable name.
func outputNamesIndexes(
	named map[string]string,
	envValues map[string]string,
	siblings map[string]struct{},
) (map[string]map[string]string, error) {
	out := make(map[string]map[string]string, len(named))
	prefixes := make([]string, 0, len(named))
	for prefix := range named {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		step := named[prefix]
		publishedCountEnv := prefix + normalizeStepName(publishedCountKey)
		publishedCountValue := envValues[publishedCountEnv]
		publishedCount, err := strconv.Atoi(publishedCountValue)
		if err != nil || publishedCount < 0 {
			return nil, fmt.Errorf(
				"IMPORT_OUTPUTS_FROM: step %q published invalid protocol sentinel %s=%q (want a non-negative integer)",
				step, publishedCountEnv, publishedCountValue)
		}

		requiredKey := prefix + normalizeStepName(tf.OutputNamesIndexRequiredKey)
		requiredValue, required := envValues[requiredKey]
		if required && requiredValue != "1" {
			return nil, fmt.Errorf(
				"IMPORT_OUTPUTS_FROM: step %q published invalid protocol sentinel %s=%q (want 1)",
				step, requiredKey, requiredValue)
		}

		indexKey := tf.OutputNamesIndexEnv(step)
		raw, hasIndex := envValues[indexKey]
		if required && !hasIndex {
			return nil, fmt.Errorf(
				"IMPORT_OUTPUTS_FROM: step %q requires an output-name index, but %s is absent; "+
					"the Caesium server is too old or incompatible with this tf-apply output row",
				step, indexKey)
		}
		if !hasIndex {
			continue
		}

		index, err := decodeOutputNamesIndex(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"IMPORT_OUTPUTS_FROM: step %q published invalid %s: %w",
				step, indexKey, err)
		}
		suffixes := ownedOutputSuffixes(prefix, envValues, siblings, index)
		if err := validateOutputNamesIndex(index, suffixes, publishedCount); err != nil {
			return nil, fmt.Errorf(
				"IMPORT_OUTPUTS_FROM: step %q published invalid %s: %w",
				step, indexKey, err)
		}
		out[prefix] = index
	}
	return out, nil
}

func decodeOutputNamesIndex(raw string) (map[string]string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("expected a JSON object of folded suffixes to original output names: %w", err)
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '{' {
		return nil, fmt.Errorf("expected a JSON object of folded suffixes to original output names")
	}

	index := make(map[string]string)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("reading folded suffix: %w", err)
		}
		suffix, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("folded suffix is not a string")
		}
		if _, duplicate := index[suffix]; duplicate {
			return nil, fmt.Errorf("folded suffix %q appears more than once", suffix)
		}
		var original string
		if err := decoder.Decode(&original); err != nil {
			return nil, fmt.Errorf("original name for suffix %q is not a string: %w", suffix, err)
		}
		index[suffix] = original
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("closing output-name index: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected JSON value after the output-name index")
		}
		return nil, fmt.Errorf("reading after output-name index: %w", err)
	}
	return index, nil
}

func ownedOutputSuffixes(
	prefix string,
	envValues map[string]string,
	siblings map[string]struct{},
	index map[string]string,
) map[string]struct{} {
	suffixes := make(map[string]struct{})
	for key := range envValues {
		rest, found := strings.CutPrefix(key, prefix)
		if !found || rest == "" {
			continue
		}
		if owner, taken := longestOwner(siblings, key); taken && owner != prefix {
			continue
		}
		if isSyntheticOutputDigest(prefix, rest, envValues, index) {
			continue
		}
		suffixes[rest] = struct{}{}
	}
	return suffixes
}

// isSyntheticOutputDigest distinguishes the companion env var Caesium adds for
// an OutputRef from a legitimate original output whose name ends in _digest.
// With an authoritative index, a real *_digest output names itself while a
// synthetic companion does not; without one, retain the historical base-key
// heuristic.
func isSyntheticOutputDigest(prefix, rest string, envValues map[string]string, index map[string]string) bool {
	baseSuffix, isDigest := strings.CutSuffix(rest, "_DIGEST")
	if !isDigest {
		return false
	}
	if _, baseExists := envValues[prefix+baseSuffix]; !baseExists {
		return false
	}
	if index == nil {
		return true
	}
	_, baseIsOutput := index[baseSuffix]
	_, digestIsOutput := index[rest]
	return baseIsOutput && !digestIsOutput
}

func validateOutputNamesIndex(index map[string]string, suffixes map[string]struct{}, publishedCount int) error {
	originals := make(map[string]string, len(index))
	applicationMappings := 0
	indexedSuffixes := make([]string, 0, len(index))
	for suffix := range index {
		indexedSuffixes = append(indexedSuffixes, suffix)
	}
	sort.Strings(indexedSuffixes)
	for _, suffix := range indexedSuffixes {
		original := index[suffix]
		if prior, duplicate := originals[original]; duplicate {
			return fmt.Errorf("original output name %q is targeted by both %s and %s", original, prior, suffix)
		}
		originals[original] = suffix
		if folded := normalizeStepName(original); folded != suffix {
			return fmt.Errorf("suffix %s maps to %q, which folds to %s", suffix, original, folded)
		}
		if _, exists := suffixes[suffix]; !exists {
			return fmt.Errorf("suffix %s has no transported output", suffix)
		}
		if !isProtocolOutputSuffix(suffix) {
			applicationMappings++
		}
	}

	protocolNames := map[string]string{
		normalizeStepName(publishedCountKey):              publishedCountKey,
		normalizeStepName(tf.OutputNamesIndexRequiredKey): tf.OutputNamesIndexRequiredKey,
	}
	for suffix, original := range protocolNames {
		if got, exists := index[suffix]; exists && got != original {
			return fmt.Errorf("protocol suffix %s must map to %q, not %q", suffix, original, got)
		}
	}
	transportedSuffixes := make([]string, 0, len(suffixes))
	for suffix := range suffixes {
		transportedSuffixes = append(transportedSuffixes, suffix)
	}
	sort.Strings(transportedSuffixes)
	for _, suffix := range transportedSuffixes {
		if _, exists := index[suffix]; !exists {
			return fmt.Errorf("transported output suffix %s is missing from the index", suffix)
		}
	}
	// The count is emitted by tf-apply before either protocol sentinel is
	// appended, so it is the authoritative number of application outputs. In
	// particular, it closes the only structural ambiguity in the environment:
	// BASE_DIGEST can be either a synthetic OutputRef companion or a real output.
	// If a partial index omits the latter, the digest heuristic would otherwise
	// hide that omission and let the index look complete.
	if applicationMappings != publishedCount {
		return fmt.Errorf(
			"caesium_outputs_published reports %d application output(s), but the index names %d",
			publishedCount, applicationMappings)
	}
	return nil
}

// importedOutputName restores the original Terraform output name from the
// folded env suffix. A present index was already verified as complete, so
// there is deliberately no per-entry lowercase fallback.
func importedOutputName(rest string, index map[string]string) string {
	if index != nil {
		return index[rest]
	}
	return strings.ToLower(rest)
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
	// The kind is the discriminator, so an ABSENT one is not "probably ours".
	// Every tf-plan emits it in both the empty and the non-empty branch, and the
	// Console will not render a proposal without it — so a summary that looks
	// plausible with no kind attached is a proposal this role never produced.
	// The example manifests catch it with `schemaValidation: fail`, but the role
	// claims every phase fails closed and must hold with validation off or warn.
	switch p.Kind {
	case tf.ProposalKind:
	case "":
		return proposal{}, fmt.Errorf(
			"step %q named no proposal kind (%sPROPOSAL_KIND is empty); tf-apply applies only %q proposals",
			planStep, prefix, tf.ProposalKind)
	default:
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
	// The digest is checked for SHAPE here, not just for content later. It names
	// the apply receipt (applyReceiptPath), so a value carrying a path separator
	// would place that file outside ARTIFACT_DIR — a write chosen by whatever
	// produced the environment.
	if !protocol.ValidDigest(p.Digest) {
		return proposal{}, fmt.Errorf(
			"step %q reported the artifact digest %q, which is not a %s<64 hex> digest",
			planStep, p.Digest, protocol.DigestPrefix)
	}
	return p, nil
}

// applyProposal applies the reviewed plan exactly once, and makes that fact
// durable before anything else can fail.
//
// The ordering here is the whole point. `terraform apply` mutates real
// infrastructure and advances state; everything after it — reading outputs,
// rendering them, flushing markers, the process surviving at all — can still
// fail. Without a durable record, such a failure marks the task failed with the
// world already changed, and Caesium's retry hands the SAME cached plan back:
// Terraform then rejects it as stale against the advanced state, and the DAG is
// wedged until someone invalidates the cache by hand. The receipt turns that
// retry into the harmless path it should have been — republish the outputs and
// move on.
//
// The receipt lives in ARTIFACT_DIR, which both shipped manifests and the
// integration lane put on the state volume, i.e. on the same durable store as
// the state the apply advanced. An ARTIFACT_DIR that does not survive the retry
// simply means no receipt is found and the pre-existing behaviour applies.
func applyProposal(ctx context.Context, cfg config, runner *tf.Runner, p proposal, log io.Writer) error {
	receipt := applyReceiptPath(cfg.ArtifactDir, p.Digest)
	switch _, err := os.Stat(receipt); {
	case err == nil:
		_, _ = fmt.Fprintf(log,
			"tf-apply: this plan was already applied (receipt %s); publishing its outputs without re-applying\n",
			receipt)
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read apply receipt %s: %w", receipt, err)
	}

	staged, cleanup, err := stagePlan(p)
	defer cleanup()
	if err != nil {
		return err
	}
	if err := checkPlannedOutputNames(ctx, runner, staged, log); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(log, "tf-apply: applying %s (%d add, %d change, %d destroy, %d replace)\n",
		p.ArtifactPath, p.Summary.Add, p.Summary.Change, p.Summary.Destroy, p.Summary.Replace)
	if err := runner.Apply(ctx, staged); err != nil {
		return err
	}
	return writeApplyReceipt(receipt, p)
}

// checkPlannedOutputNames rejects an output name a consumer could not import
// exactly, BEFORE terraform apply mutates anything.
//
// The naming rule was previously enforced only after the apply, against
// `terraform output`. That is late in the one way that matters: the stack has
// already been deployed, so the operator meets a red run over infrastructure
// that changed, and every retry then fails identically (correctly — the receipt
// makes it skip the apply) until the output is renamed. The saved plan already
// names the outputs it will publish, and this reads them from the VERIFIED
// private copy, so the same failure arrives with nothing deployed.
//
// It is an early pass, not a replacement. tf.PlannedPublishableOutputNames only
// returns names the plan states plainly are not sensitive, and an output the
// plan reports as unchanged may not appear at all — so PublishableOutputs stays
// the authoritative check. The empty-plan branch of runApply never reaches this
// function and is covered by that backstop alone.
func checkPlannedOutputNames(ctx context.Context, runner *tf.Runner, planPath string, log io.Writer) error {
	// ShowPlanFile discards Terraform's stdout for the duration, so the raw plan
	// JSON — sensitive_values and all — never reaches the task log. Only output
	// NAMES are read out of the result here, and never a value.
	plan, err := runner.ShowPlanFile(ctx, planPath)
	if err != nil {
		return err
	}
	names := tf.PlannedPublishableOutputNames(plan)
	if err := tf.CheckOutputNames(names); err != nil {
		return fmt.Errorf(
			"the proposed plan publishes an output this stack's consumers could not read, so it was not applied: %w", err)
	}
	_, _ = fmt.Fprintf(log, "tf-apply: %d planned output name(s) can be read by a consumer\n", len(names))
	return nil
}

// applyReceiptPath is where a completed apply of one proposal is recorded. The
// plan digest is in the name so a LATER proposal — a different plan, a
// different set of changes — never matches an earlier receipt.
func applyReceiptPath(artifactDir, digest string) string {
	return filepath.Join(artifactDir, "applied."+strings.TrimPrefix(digest, protocol.DigestPrefix))
}

// writeApplyReceipt records that the plan was applied, and fails loudly if it
// cannot.
//
// Failing here is not pedantry: the apply has already happened, so a missing
// receipt is exactly the unrecoverable state this function exists to prevent.
// Saying so — naming the receipt, and saying the apply itself succeeded — is
// what lets an operator create the file (or invalidate the cache) rather than
// discovering the wedge on the next run.
//
// The write is atomic: content into a sibling temp file, fsync, rename into
// place. A receipt is read by EXISTENCE, and it is written at the one moment
// the process is most likely to be killed — immediately after an apply that
// just took minutes and changed real infrastructure. A plain os.WriteFile
// creates the final name first and fills it afterwards, so a crash in that
// window leaves an empty file at exactly the path a retry stats, and the retry
// skips an apply that never completed. rename(2) within a directory is atomic,
// so the receipt either does not exist or is whole. The fsync is what makes
// that true across a machine crash rather than only a process one, and it is
// cheap against a file this size.
func writeApplyReceipt(path string, p proposal) error {
	body := fmt.Sprintf("plan %s applied\ndigest %s\nartifact %s\n", p.ArtifactKey, p.Digest, p.ArtifactPath)
	if err := writeFileAtomic(path, []byte(body)); err != nil {
		return fmt.Errorf(
			"terraform apply SUCCEEDED but its receipt could not be written to %s: %w. "+
				"A retry of this step would try to re-apply the same saved plan against state it has already "+
				"advanced, which Terraform rejects as stale",
			path, err)
	}
	pruneApplyReceipts(filepath.Dir(path), filepath.Base(path))
	return nil
}

// writeFileAtomic writes body to path so that path is never observed partially
// written.
//
// The temp file is created in the SAME directory (rename across filesystems is
// not atomic and would fail outright on a bind mount) and named so that
// pruneApplyReceipts sweeps up anything a crash left behind, while
// applyReceiptPath can never match it.
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "applied.*"+receiptPartialSuffix)
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	// Removing the temp file is a no-op once the rename has consumed it.
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	// Durability before visibility: an fsync after the rename could still leave
	// a visible name pointing at unwritten content.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp.Name(), err)
	}
	// A retry runs in a different container, so the receipt has to be readable
	// by more than its creator; it carries no secret.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("set permissions on %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("rename %s into place: %w", tmp.Name(), err)
	}
	return nil
}

// receiptPartialSuffix marks a receipt that is still being written. It is a
// SUFFIX rather than a prefix so a leftover still begins with "applied." and is
// swept by pruneApplyReceipts, while never equalling an applyReceiptPath.
const receiptPartialSuffix = ".partial"

// pruneApplyReceipts drops receipts from earlier proposals, so a long-lived
// state volume accumulates one file rather than one per apply. Best effort: a
// receipt that cannot be removed is clutter, never a correctness problem.
func pruneApplyReceipts(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == keep || !strings.HasPrefix(name, "applied.") {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// stagePlan copies the proposed plan into an apply-private file, hashing the
// bytes it copies, and verifies THAT copy against the digest the plan step
// reported. The caller applies the copy.
//
// Hashing a path and then handing the same path to Terraform leaves a window:
// the plan file lives on a shared volume, and a concurrent writer (or a
// symlink swap) between the check and Terraform's own open would have Terraform
// apply bytes nobody verified — which would make the whole propose/apply split
// decorative. Copying from ONE opened descriptor closes it: the bytes that are
// hashed, the bytes that are written, and the bytes Terraform reads are the
// same bytes, and the copy lives in a container-private temp directory that no
// other step can reach.
//
// cleanup is always safe to call, including on the error paths.
func stagePlan(p proposal) (path string, cleanup func(), err error) {
	cleanup = func() {}

	src, err := os.Open(p.ArtifactPath) //nolint:gosec // the path came from the plan step's own output reference.
	if err != nil {
		return "", cleanup, fmt.Errorf("open plan artifact %s: %w", p.ArtifactPath, err)
	}
	defer func() { _ = src.Close() }()
	// Statted through the descriptor, not the path: whatever is at the path now
	// is irrelevant, this is the file that will actually be read.
	info, err := src.Stat()
	if err != nil {
		return "", cleanup, fmt.Errorf("stat plan artifact %s: %w", p.ArtifactPath, err)
	}
	if !info.Mode().IsRegular() {
		return "", cleanup, fmt.Errorf("plan artifact %s is not a regular file; refusing to apply it", p.ArtifactPath)
	}

	dir, err := os.MkdirTemp("", "tf-apply-")
	if err != nil {
		return "", cleanup, fmt.Errorf("create apply scratch directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(dir) }

	staged := filepath.Join(dir, planFileName)
	dst, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", cleanup, fmt.Errorf("create apply-private plan copy: %w", err)
	}
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(dst, sum), src); err != nil {
		_ = dst.Close()
		return "", cleanup, fmt.Errorf("copy plan artifact %s: %w", p.ArtifactPath, err)
	}
	if err := dst.Close(); err != nil {
		return "", cleanup, fmt.Errorf("write apply-private plan copy: %w", err)
	}

	got := protocol.DigestPrefix + hex.EncodeToString(sum.Sum(nil))
	if got != p.Digest {
		return "", cleanup, fmt.Errorf(
			"plan artifact %s does not match the proposal: proposed %s, found %s. "+
				"The reviewed plan is not the plan on disk; refusing to apply",
			p.ArtifactPath, p.Digest, got)
	}
	return staged, cleanup, nil
}

// ---------------------------------------------------------------------------
// tf-drift
// ---------------------------------------------------------------------------

func runDrift(ctx context.Context, cfg config, e *protocol.Emitter, log io.Writer) error {
	runner, err := prepare(cfg, log)
	if err != nil {
		return err
	}
	// A drift job contains no apply steps, so it must leave IMPORT_OUTPUTS_FROM
	// unset and pin its cross-stack variables in its own step env (TF_VAR_*,
	// which passes straight through) — otherwise a stack with a required
	// cross-stack variable fails with "No value for required variable". Naming a
	// producer that is not there is now a hard failure rather than a silent
	// zero-import (see requireProducers), which is what makes the pin explicit
	// instead of accidental. This call stays so a drift job that DOES follow an
	// apply behaves like tf-plan.
	if _, err := exportImportedOutputs(cfg.ImportOutputsFrom, os.Environ(), log); err != nil {
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

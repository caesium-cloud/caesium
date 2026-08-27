// Command tf-discover is the discover role of the unit-pipeline pattern
// (design §5.1, §6.2): it enumerates the Terraform stacks under a scan root,
// fingerprints each one, and declares their order.
//
// Discover owns the fingerprint. Caesium never inspects a stack's contents —
// whatever this image says a stack's inputs digest to *is* that stack's cache
// key contribution. The corollary is that every failure here must be loud: an
// absent fingerprint is never "unchanged", so any error exits non-zero having
// emitted nothing at all.
//
// Environment:
//
//	SCAN_ROOT     a stack directory (single-root) or a directory of stacks (multi-root)
//	TF_WORKSPACE  workspace name folded into the fingerprint (default "default")
//	TF_CLI_PATH   terraform binary to use (default: `terraform` on PATH)
//
// Single-root mode emits ##caesium::output {fingerprint, input_<name>…} for the
// hand-written per-stack job form. Multi-root mode emits ##caesium::partitions
// [{key, fingerprint, dependsOn, root}] for the fan-out form.
//
// Module resolution is Terraform's own: `terraform get` installs modules
// WITHOUT installing providers, which is why discover depends only on the
// checkout and never on the provider warm step.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"

	"github.com/caesium-cloud/caesium/pack/internal/fingerprint"
	"github.com/caesium-cloud/caesium/pack/internal/protocol"
	"github.com/caesium-cloud/caesium/pack/internal/tf"
)

const roleName = "tf-discover"

func main() {
	protocol.Run(roleName, func(e *protocol.Emitter) error {
		cfg, err := loadConfig(os.Getenv)
		if err != nil {
			return err
		}
		return discover(context.Background(), cfg, e)
	})
}

type config struct {
	ScanRoot  string
	Workspace string
	ExecPath  string
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		ScanRoot:  strings.TrimSpace(getenv("SCAN_ROOT")),
		Workspace: strings.TrimSpace(getenv("TF_WORKSPACE")),
		ExecPath:  strings.TrimSpace(getenv("TF_CLI_PATH")),
	}
	if cfg.ScanRoot == "" {
		return config{}, fmt.Errorf("SCAN_ROOT is required")
	}
	info, err := os.Stat(cfg.ScanRoot)
	if err != nil {
		return config{}, fmt.Errorf("SCAN_ROOT %s: %w", cfg.ScanRoot, err)
	}
	if !info.IsDir() {
		return config{}, fmt.Errorf("SCAN_ROOT %s is not a directory", cfg.ScanRoot)
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "default"
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

func discover(ctx context.Context, cfg config, e *protocol.Emitter) error {
	single, err := isRootModule(cfg.ScanRoot)
	if err != nil {
		return err
	}
	if single {
		return discoverSingle(ctx, cfg, e)
	}
	return discoverMulti(ctx, cfg, e)
}

// discoverSingle fingerprints SCAN_ROOT itself. Alongside the fingerprint it
// emits one digest per input, so `caesium why` can name which input moved
// rather than only reporting that the key changed.
func discoverSingle(ctx context.Context, cfg config, e *protocol.Emitter) error {
	inputs, fp, err := fingerprintStack(ctx, cfg, cfg.ScanRoot)
	if err != nil {
		return err
	}
	values := map[string]string{"fingerprint": fp}
	for _, in := range inputs {
		values["input_"+in.Name] = in.Digest
	}
	values["input_workspace"] = scalarDigest(cfg.Workspace)
	return e.Output(values)
}

// discoverMulti fingerprints every stack under SCAN_ROOT and emits them as
// structured partitions, carrying the inter-stack order from stacks.yaml.
func discoverMulti(ctx context.Context, cfg config, e *protocol.Emitter) error {
	stacks, err := enumerateStacks(cfg.ScanRoot)
	if err != nil {
		return err
	}
	parts := make([]protocol.Partition, 0, len(stacks))
	for _, st := range stacks {
		_, fp, err := fingerprintStack(ctx, cfg, filepath.Join(cfg.ScanRoot, filepath.FromSlash(st.Root)))
		if err != nil {
			return fmt.Errorf("stack %s: %w", st.Name, err)
		}
		parts = append(parts, protocol.Partition{
			Key:         st.Name,
			Fingerprint: fp,
			DependsOn:   st.DependsOn,
			// root is relative to SCAN_ROOT so the partition never carries a
			// path that depends on where the volume happened to be mounted.
			Attributes: map[string]string{"root": st.Root},
		})
	}
	return e.Partitions(parts)
}

// fingerprintStack resolves one root module's module graph and digests it.
func fingerprintStack(ctx context.Context, cfg config, dir string) ([]fingerprint.Input, string, error) {
	terraform, err := tfexec.NewTerraform(dir, cfg.ExecPath)
	if err != nil {
		return nil, "", fmt.Errorf("initialize terraform in %s: %w", dir, err)
	}
	// Terraform's own stdout goes to STDERR. stdout belongs to the marker
	// protocol alone: a `Downloading …` line landing between the markers would
	// be harmless, but a line that happens to contain a marker prefix would not.
	terraform.SetStdout(os.Stderr)
	terraform.SetStderr(os.Stderr)

	// Relocate Terraform's data directory out of the source tree. Discover
	// mounts the source read-only (design §5.5) and, in multi-root mode, walks
	// many stacks in one container — a per-stack scratch directory keeps
	// `terraform get` from needing to write into either.
	dataDir, err := os.MkdirTemp("", "tf-discover-data-")
	if err != nil {
		return nil, "", fmt.Errorf("create terraform data directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dataDir) }()
	if err := terraform.SetEnv(terraformEnv(dataDir)); err != nil {
		return nil, "", fmt.Errorf("configure terraform environment: %w", err)
	}

	// `terraform get` installs modules only — no providers, which is why
	// discover depends on the checkout alone. It also fails on a module source
	// that cannot reduce to a constant, which is what closes the "green run
	// that deployed nothing" path for dynamic sources upstream, rather than by
	// a heuristic of ours (design §6.2).
	if err := terraform.Get(ctx); err != nil {
		return nil, "", fmt.Errorf("terraform get in %s: %w", dir, err)
	}

	manifest, err := tf.ReadManifest(dataDir)
	if err != nil {
		return nil, "", err
	}

	// Resolve before digesting: an installed module (registry, git, http) lives
	// in the data directory, so its manifest Dir is a per-run scratch path that
	// must be read from but never digested.
	modules, err := manifest.Resolve(dir, dataDir)
	if err != nil {
		return nil, "", err
	}

	inputs := make([]fingerprint.Input, 0, len(modules))
	for _, module := range modules {
		digest, err := fingerprint.DigestDir(module.Dir)
		if err != nil {
			return nil, "", fmt.Errorf("module %q (%s): %w", module.Key, module.Identity, err)
		}
		inputs = append(inputs, fingerprint.Input{
			Name:     tf.ModuleName(module.Key),
			Identity: module.Identity,
			Digest:   digest,
		})
	}
	fp, err := fingerprint.Combine(inputs, "workspace="+cfg.Workspace)
	if err != nil {
		return nil, "", fmt.Errorf("fingerprint %s: %w", dir, err)
	}
	return inputs, fp, nil
}

// ---------------------------------------------------------------------------
// Stack enumeration
// ---------------------------------------------------------------------------

// stack is one unit in multi-root mode.
type stack struct {
	Name      string
	Root      string
	DependsOn []string
}

// enumerateStacks lists the root modules directly under scanRoot and applies
// the ordering declared in stacks.yaml.
//
// When stacks.yaml is present it must name exactly the stacks on disk. A stack
// that exists but is unlisted would be silently dropped from the plan, and a
// listed stack that does not exist would expand into an instance with nothing
// to apply — both are the cardinal failure in a different costume, so both are
// hard errors.
func enumerateStacks(scanRoot string) ([]stack, error) {
	entries, err := os.ReadDir(scanRoot)
	if err != nil {
		return nil, fmt.Errorf("read SCAN_ROOT %s: %w", scanRoot, err)
	}
	onDisk := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		isRoot, err := isRootModule(filepath.Join(scanRoot, entry.Name()))
		if err != nil {
			return nil, err
		}
		if isRoot {
			onDisk = append(onDisk, entry.Name())
		}
	}
	sort.Strings(onDisk)
	if len(onDisk) == 0 {
		return nil, fmt.Errorf("SCAN_ROOT %s is neither a root module nor a directory containing any", scanRoot)
	}

	declared, err := readStacksFile(filepath.Join(scanRoot, stacksFileName))
	if err != nil {
		return nil, err
	}
	if declared == nil {
		stacks := make([]stack, 0, len(onDisk))
		for _, name := range onDisk {
			stacks = append(stacks, stack{Name: name, Root: name})
		}
		return stacks, nil
	}

	declaredRoots := make(map[string]struct{}, len(declared))
	for _, st := range declared {
		declaredRoots[st.Root] = struct{}{}
	}
	for _, name := range onDisk {
		if _, ok := declaredRoots[name]; !ok {
			return nil, fmt.Errorf("%s does not list the stack directory %q; every stack under the scan root must be declared", stacksFileName, name)
		}
	}
	onDiskSet := make(map[string]struct{}, len(onDisk))
	for _, name := range onDisk {
		onDiskSet[name] = struct{}{}
	}
	for _, st := range declared {
		if _, ok := onDiskSet[st.Root]; !ok {
			return nil, fmt.Errorf("%s lists stack %q with root %q, which is not a root module under the scan root", stacksFileName, st.Name, st.Root)
		}
	}
	return declared, nil
}

// isRootModule reports whether dir holds Terraform configuration of its own.
func isRootModule(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".tf") || strings.HasSuffix(name, ".tf.json") {
			return true, nil
		}
	}
	return false, nil
}

// terraformEnv is the environment `terraform get` runs with: this process's
// own, plus TF_DATA_DIR, minus the variables terraform-exec owns itself
// (TF_WORKSPACE, TF_VAR_*, TF_CLI_ARGS_*, the TF_LOG family). Those are
// stripped rather than rejected because a job manifest may legitimately set
// TF_VAR_* on every step in the group, and discover has no use for them.
func terraformEnv(dataDir string) map[string]string {
	env := make(map[string]string, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	env["TF_DATA_DIR"] = dataDir

	// A git module source makes `terraform get` shell out to git, and git
	// synthesizes a reflog identity by resolving the machine's own hostname
	// when none is configured. In a pod with no DNS entry for its name that
	// lookup stalls until the resolver gives up (measured at ~5 s per fetch on
	// a network-isolated container) or fails outright, on a module install that
	// has nothing to do with identity. Pinning an inert one removes the lookup;
	// discover never creates a commit. Same reasoning as git-source's gitEnv.
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME":     "caesium tf-discover",
		"GIT_AUTHOR_EMAIL":    "tf-discover@caesium.invalid",
		"GIT_COMMITTER_NAME":  "caesium tf-discover",
		"GIT_COMMITTER_EMAIL": "tf-discover@caesium.invalid",
	} {
		if _, set := env[key]; !set {
			env[key] = value
		}
	}

	return tfexec.CleanEnv(env)
}

func scalarDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return protocol.Digest(sum[:])
}

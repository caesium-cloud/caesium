// Command tf-warm is the warm role of the Terraform binding: it populates a
// shared provider filesystem_mirror on the cache volume, idempotently, and
// self-checks a marker inside that volume (design §3.4, §6.3).
//
// Two decisions in the design are load-bearing here and easy to undo by
// accident:
//
//   - **The warm step is never given a Caesium `cache` block.** A cache hit
//     means no container ran; if the cache PVC had been recreated the volume
//     would be empty and every downstream `init` would fail. Always running and
//     self-checking a marker *inside the volume* is self-healing, and costs one
//     container start per run.
//   - **A filesystem mirror, not TF_PLUGIN_CACHE_DIR.** HashiCorp documents the
//     plugin cache directory as not concurrency safe, which rules it out for a
//     cache shared by many parallel `init` calls. A filesystem mirror is
//     read-only at consumption time.
//
// Content addressing plus an atomic rename is what makes two concurrent warms
// of the same key benign, which is why no lock is required.
//
// Environment:
//
//	SRC              source tree to scan for .terraform.lock.hcl (default /src)
//	CACHE_DIR        the cache volume mount (default /cache)
//	CACHE_MOUNT_PATH the cache path CONSUMERS see, if it differs from CACHE_DIR
//	TARGET_PLATFORM  os_arch to mirror for, space/comma separated
//	                 (default: this container's own platform)
//	TF_CLI_PATH      terraform binary to use (default: `terraform` on PATH)
//
// Emits no markers at all: the warm step's result is the volume's contents, and
// a role that emitted an output would make its consumers' cache keys depend on
// a step whose whole purpose is to be invisible to them.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
	"github.com/caesium-cloud/caesium/pack/internal/tf"
)

const roleName = "tf-warm"

func main() {
	protocol.Run(roleName, func(*protocol.Emitter) error {
		cfg, err := loadConfig(os.Getenv)
		if err != nil {
			return err
		}
		return warm(context.Background(), cfg, os.Stderr)
	})
}

type config struct {
	Src       string
	CacheDir  string
	MountPath string
	Platforms []string
	ExecPath  string
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		Src:       strings.TrimSpace(getenv("SRC")),
		CacheDir:  strings.TrimSpace(getenv("CACHE_DIR")),
		MountPath: strings.TrimSpace(getenv("CACHE_MOUNT_PATH")),
		Platforms: splitPlatforms(getenv("TARGET_PLATFORM")),
		ExecPath:  strings.TrimSpace(getenv("TF_CLI_PATH")),
	}
	if cfg.Src == "" {
		cfg.Src = "/src"
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = "/cache"
	}
	if cfg.MountPath == "" {
		// The generated terraformrc names absolute paths that CONSUMING
		// containers resolve, so it must be written in their view of the
		// volume. They coincide unless a manifest mounts the volume elsewhere.
		cfg.MountPath = cfg.CacheDir
	}
	if !filepath.IsAbs(cfg.CacheDir) || !filepath.IsAbs(cfg.MountPath) {
		return config{}, fmt.Errorf("CACHE_DIR and CACHE_MOUNT_PATH must be absolute paths")
	}
	if len(cfg.Platforms) == 0 {
		cfg.Platforms = []string{runtime.GOOS + "_" + runtime.GOARCH}
	}
	for _, platform := range cfg.Platforms {
		if !validPlatform(platform) {
			return config{}, fmt.Errorf("TARGET_PLATFORM %q is not an os_arch pair", platform)
		}
	}
	info, err := os.Stat(cfg.Src)
	if err != nil {
		return config{}, fmt.Errorf("SRC %s: %w", cfg.Src, err)
	}
	if !info.IsDir() {
		return config{}, fmt.Errorf("SRC %s is not a directory", cfg.Src)
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

func splitPlatforms(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// validPlatform accepts the os_arch form `terraform providers mirror -platform`
// takes. It is checked rather than passed through because the value reaches a
// command line.
func validPlatform(platform string) bool {
	goos, arch, ok := strings.Cut(platform, "_")
	if !ok || goos == "" || arch == "" {
		return false
	}
	for _, part := range []string{goos, arch} {
		for _, r := range part {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}

// warm is the whole role: derive the key, exit on the marker, otherwise mirror,
// rename, write the CLI config, and drop the marker.
func warm(ctx context.Context, cfg config, logOut io.Writer) error {
	lockPaths, err := tf.FindLockFiles(cfg.Src)
	if err != nil {
		return err
	}
	if len(lockPaths) == 0 {
		// Fail closed. An empty mirror would make every consuming `init` fail
		// with an unrelated diagnosis; worse, a marker dropped for an empty
		// provider set would make later runs exit fast against it.
		return fmt.Errorf("no %s under %s; a Terraform source tree must commit its lock files "+
			"so the provider set is pinned (design §6.3)", tf.LockFileName, cfg.Src)
	}

	sets := make([][]tf.LockedProvider, 0, len(lockPaths))
	for _, path := range lockPaths {
		providers, err := tf.ReadLockFile(path)
		if err != nil {
			return err
		}
		sets = append(sets, providers)
	}
	providers := tf.MergeLocked(sets...)
	key := tf.MirrorKey(providers, cfg.Platforms)

	markerDir := filepath.Join(cfg.CacheDir, ".warm")
	marker := filepath.Join(markerDir, key)
	mirrorDir := filepath.Join(cfg.CacheDir, "providers", key)

	if _, err := os.Stat(marker); err == nil {
		_, _ = fmt.Fprintf(logOut, "%s: mirror %s already warm (%d lock files, %d providers); nothing to do\n",
			roleName, key, len(lockPaths), len(providers))
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check warm marker %s: %w", marker, err)
	}

	_, _ = fmt.Fprintf(logOut, "%s: mirroring %d providers for %v into %s (key %s)\n",
		roleName, len(providers), cfg.Platforms, mirrorDir, key)

	staging := filepath.Join(cfg.CacheDir, "providers.tmp."+key)
	// A staging directory left by a killed warm is scratch by construction: it
	// is never read by anything (only the renamed, keyed directory is), so
	// clearing it is safe and keeps a half-populated mirror from being promoted.
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("clear staging directory %s: %w", staging, err)
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("create staging directory %s: %w", staging, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := mirrorProviders(ctx, cfg, providers, staging, logOut); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
		return fmt.Errorf("create mirror root %s: %w", filepath.Dir(mirrorDir), err)
	}
	if err := os.Rename(staging, mirrorDir); err != nil {
		// A concurrent warm of the same key populated the same content first.
		// The mirror is content-addressed, so its directory is interchangeable
		// with ours: adopt it rather than failing.
		if _, statErr := os.Stat(mirrorDir); statErr != nil {
			return fmt.Errorf("promote mirror %s: %w", mirrorDir, err)
		}
		_, _ = fmt.Fprintf(logOut, "%s: mirror %s was populated concurrently; adopting it\n", roleName, key)
	}

	consumerMirror := filepath.Join(cfg.MountPath, "providers", key)
	if err := writeFileAtomic(filepath.Join(cfg.CacheDir, "terraformrc"), []byte(tf.TerraformRC(consumerMirror)), 0o644); err != nil {
		return err
	}

	// The marker is written LAST. Every consumer-visible artifact — the mirror
	// directory and the CLI configuration — must exist before anything is
	// allowed to exit fast on its behalf.
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return fmt.Errorf("create marker directory %s: %w", markerDir, err)
	}
	if err := writeFileAtomic(marker, []byte(key+"\n"), 0o644); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(logOut, "%s: mirror %s ready at %s\n", roleName, key, consumerMirror)
	return nil
}

// mirrorProviders runs `terraform providers mirror` into target.
//
// One synthetic root module per distinct version of a provider: a single
// required_providers block can pin one version per provider, but two stacks may
// legitimately pin two, and the mirror has to hold both or the stack on the
// older pin cannot init offline. In the overwhelmingly common case that is one
// pass.
func mirrorProviders(ctx context.Context, cfg config, providers []tf.LockedProvider, target string, logOut io.Writer) error {
	for round, group := range versionRounds(providers) {
		dir, err := os.MkdirTemp("", "tf-warm-root-")
		if err != nil {
			return fmt.Errorf("create synthetic root module: %w", err)
		}
		if err := mirrorRound(ctx, cfg, group, dir, target); err != nil {
			_ = os.RemoveAll(dir)
			return fmt.Errorf("mirror round %d: %w", round+1, err)
		}
		_ = os.RemoveAll(dir)
		_, _ = fmt.Fprintf(logOut, "%s: mirrored %d providers (round %d)\n", roleName, len(group), round+1)
	}
	return nil
}

func mirrorRound(ctx context.Context, cfg config, group []tf.LockedProvider, dir, target string) error {
	hcl, err := tf.RequiredProvidersHCL(group)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "providers.tf"), []byte(hcl), 0o600); err != nil {
		return fmt.Errorf("write synthetic root module: %w", err)
	}
	// The lock file is copied in so `providers mirror` selects exactly the
	// locked versions and verifies exactly the recorded checksums. Without it
	// the mirror would be populated from the constraint, which is the whole
	// reproducibility property the lock file exists to provide.
	if err := os.WriteFile(filepath.Join(dir, tf.LockFileName), []byte(renderLockFile(group)), 0o600); err != nil {
		return fmt.Errorf("write synthetic lock file: %w", err)
	}

	terraform, err := tfexec.NewTerraform(dir, cfg.ExecPath)
	if err != nil {
		return fmt.Errorf("initialize terraform: %w", err)
	}
	// stdout belongs to the marker protocol alone; Terraform's own chatter goes
	// to stderr where it is still visible in the task log.
	terraform.SetStdout(os.Stderr)
	terraform.SetStderr(os.Stderr)
	if err := terraform.SetEnv(tfexec.CleanEnv(envWith("TF_DATA_DIR", filepath.Join(dir, ".tfdata")))); err != nil {
		return fmt.Errorf("configure terraform environment: %w", err)
	}

	opts := make([]tfexec.ProvidersMirrorOption, 0, len(cfg.Platforms))
	for _, platform := range cfg.Platforms {
		opts = append(opts, tfexec.Platform(platform))
	}
	if err := terraform.ProvidersMirror(ctx, target, opts...); err != nil {
		return fmt.Errorf("terraform providers mirror: %w", err)
	}
	return nil
}

// versionRounds splits the provider union into groups that each hold at most
// one version per provider source.
func versionRounds(providers []tf.LockedProvider) [][]tf.LockedProvider {
	bySource := map[string][]tf.LockedProvider{}
	sources := make([]string, 0, len(providers))
	for _, p := range providers {
		if _, seen := bySource[p.Source]; !seen {
			sources = append(sources, p.Source)
		}
		bySource[p.Source] = append(bySource[p.Source], p)
	}
	sort.Strings(sources)

	depth := 0
	for _, list := range bySource {
		if len(list) > depth {
			depth = len(list)
		}
	}

	rounds := make([][]tf.LockedProvider, 0, depth)
	for i := range depth {
		round := make([]tf.LockedProvider, 0, len(sources))
		for _, source := range sources {
			if i < len(bySource[source]) {
				round = append(round, bySource[source][i])
			}
		}
		rounds = append(rounds, round)
	}
	return rounds
}

// renderLockFile writes the subset of a lock file `providers mirror` reads: the
// source, the exact version, and the recorded hashes.
func renderLockFile(providers []tf.LockedProvider) string {
	var b strings.Builder
	b.WriteString("# Generated by caesium tf-warm from the source tree's committed lock files.\n")
	for _, p := range providers {
		fmt.Fprintf(&b, "\nprovider %q {\n  version = %q\n  hashes = [\n", p.Source, p.Version)
		for _, h := range p.Hashes {
			fmt.Fprintf(&b, "    %q,\n", h)
		}
		b.WriteString("  ]\n}\n")
	}
	return b.String()
}

// writeFileAtomic writes through a temporary file in the same directory and
// renames, so a consumer never reads a half-written CLI configuration or a
// marker that arrived before the mirror it vouches for.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Chmod(name, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", name, path, err)
	}
	return nil
}

// envWith is this process's environment plus one override. Terraform's own
// TF_DATA_DIR must point at scratch: the synthetic root module is temporary and
// nothing may be written into the (read-only) source tree.
func envWith(key, value string) map[string]string {
	env := make(map[string]string, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		env[k] = v
	}
	env[key] = value
	return env
}

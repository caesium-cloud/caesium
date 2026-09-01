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
//	CACHE_KEY        terraformrc slot on the cache volume. Unset keeps the
//	                 historical /cache/terraformrc; set (and matched on every
//	                 consuming tf-runner step) writes /cache/<key>/terraformrc
//	                 so two provider sets can share one volume without
//	                 clobbering the CLI config.
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
	"time"

	"github.com/hashicorp/terraform-exec/tfexec"

	"github.com/caesium-cloud/caesium/reagents/internal/protocol"
	"github.com/caesium-cloud/caesium/reagents/internal/tf"
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
	CacheKey  string
	Platforms []string
	ExecPath  string
}

func loadConfig(getenv func(string) string) (config, error) {
	key, err := tf.SanitizeCacheKey(getenv("CACHE_KEY"))
	if err != nil {
		return config{}, err
	}
	cfg := config{
		Src:       strings.TrimSpace(getenv("SRC")),
		CacheDir:  strings.TrimSpace(getenv("CACHE_DIR")),
		MountPath: strings.TrimSpace(getenv("CACHE_MOUNT_PATH")),
		CacheKey:  key,
		Platforms: splitPlatforms(getenv("TARGET_PLATFORM")),
		ExecPath:  strings.TrimSpace(getenv("TF_CLI_PATH")),
	}
	if cfg.Src == "" {
		cfg.Src = "/src"
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = tf.DefaultCacheDir
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
	if err := ensureCacheLayout(cfg); err != nil {
		return err
	}

	ready, err := warmArtifactsReady(marker, mirrorDir, key, providers, cfg.Platforms)
	if err != nil {
		return err
	}
	if ready {
		_, _ = fmt.Fprintf(logOut, "%s: mirror %s already warm (%d lock files, %d providers)\n",
			roleName, key, len(lockPaths), len(providers))
		// The mirror is content-addressed per provider set, but the CLI
		// configuration that POINTS at one is a filename — `{cache}/terraformrc`
		// by default, or `{cache}/{CACHE_KEY}/terraformrc` when a slot is
		// named. Two jobs sharing a cache volume with the SAME slot and
		// different provider sets (the deploy + drift topology design §6.6
		// prescribes, whose sparse checkouts need not match) each rewrite it.
		// Returning here without re-asserting the file would leave a warm that
		// found its own marker pointing consumers at another provider set's
		// mirror, so every init fails offline with a diagnosis that points
		// nowhere near the cause. Re-asserting is idempotent. Distinct CACHE_KEY
		// values give those jobs distinct files, which is how one volume serves
		// more than one provider set.
		return ensureTerraformRC(cfg, key, logOut)
	}

	_, _ = fmt.Fprintf(logOut, "%s: mirroring %d providers for %v into %s (key %s)\n",
		roleName, len(providers), cfg.Platforms, mirrorDir, key)

	// The staging directory is per-PROCESS, not per-key.
	//
	// Design §3.5 and §6.3 both justify having no named lock by saying that
	// concurrent warms of one key are benign, because the write is
	// content-addressed and promoted by an atomic rename. A staging path derived
	// from the key alone breaks precisely that: two warms of the same key share
	// one directory, and the second one clearing it mid-flight leaves the first
	// to finish `providers mirror` successfully, promote a directory missing a
	// package, and drop a marker vouching for it. Every later run then exits
	// fast on that marker and every consuming `init` fails offline, until a
	// human deletes it. The marker is the thing that lies, which is the failure
	// the self-healing design exists to eliminate. MkdirTemp gives each process
	// its own directory, so two warms can only race at the rename below, where
	// the adoption path already handles it.
	staging, err := os.MkdirTemp(cfg.CacheDir, "providers.tmp."+key+".")
	if err != nil {
		return fmt.Errorf("create staging directory in %s: %w", cfg.CacheDir, err)
	}
	// MkdirTemp creates 0700, and this directory is RENAMED into place as the
	// mirror root every consuming container resolves through terraformrc. A
	// 0700 mirror is readable only by the warm step's uid, so the moment a pod
	// security context overrides runAsUser, an fsGroup is relied on, or a
	// non-reagent image consumes the cache volume, every `init` fails offline with
	// a "provider not available" diagnosis that points nowhere near the cause.
	if err := os.Chmod(staging, 0o755); err != nil {
		return fmt.Errorf("make staging directory %s world-readable: %w", staging, err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	// Swept by AGE, never unconditionally: another process's staging directory
	// may be a mirror in flight.
	sweepStaleStaging(cfg.CacheDir, staging, logOut)

	if err := mirrorProviders(ctx, cfg, providers, staging, logOut); err != nil {
		return err
	}

	if err := promoteMirror(staging, mirrorDir, cfg.CacheDir, key, providers, cfg.Platforms, logOut); err != nil {
		return err
	}

	if err := ensureTerraformRC(cfg, key, logOut); err != nil {
		return err
	}

	// The marker is written LAST. Every consumer-visible artifact — the mirror
	// directory and the CLI configuration — must exist before anything is
	// allowed to exit fast on its behalf.
	if err := writeFileAtomic(marker, []byte(key+"\n"), 0o644); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(logOut, "%s: mirror %s ready at %s\n", roleName, key, consumerMirrorPath(cfg, key))
	return nil
}

// ensureCacheLayout creates the tf-warm-owned directory entries and proves
// that none is a symlink. CACHE_KEY is meant to select an independent child of
// the cache root; following `/cache/a -> /cache/b` (or out of the volume) would
// silently turn two keys into one slot and let one warm clobber the other.
func ensureCacheLayout(cfg config) error {
	for _, item := range []struct {
		path, purpose string
	}{
		{filepath.Join(cfg.CacheDir, "providers"), "provider mirror root"},
		{filepath.Join(cfg.CacheDir, ".warm"), "warm marker root"},
	} {
		if err := ensureCacheDirectory(item.path, item.purpose); err != nil {
			return err
		}
	}
	if cfg.CacheKey != "" {
		if err := ensureCacheDirectory(filepath.Join(cfg.CacheDir, cfg.CacheKey), "CACHE_KEY slot"); err != nil {
			return err
		}
	}
	return nil
}

// ensureCacheDirectory is race-safe for concurrent warms creating the same
// layout. Every consumer may run under a different uid/fsGroup, so the
// directories are re-asserted as traversable rather than trusting an old mode.
func ensureCacheDirectory(path, purpose string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if mkdirErr := os.Mkdir(path, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
			return fmt.Errorf("create %s %s: %w", purpose, path, mkdirErr)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect %s %s: %w", purpose, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s %s is a symbolic link; refusing a cache alias or escape", purpose, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %s is not a directory", purpose, path)
	}
	if info.Mode().Perm() != 0o755 {
		if err := os.Chmod(path, 0o755); err != nil {
			return fmt.Errorf("make %s %s traversable by cache consumers: %w", purpose, path, err)
		}
	}
	return nil
}

// warmArtifactsReady validates the marker promise before taking the fast path.
// A marker whose mirror was removed must trigger a re-mirror; returning green
// and merely repairing terraformrc would point every consumer at a path that
// does not exist. Symlinks and non-regular markers fail closed instead of being
// followed out of tf-warm's owned layout.
func warmArtifactsReady(
	marker, mirrorDir, key string,
	providers []tf.LockedProvider,
	platforms []string,
) (bool, error) {
	info, err := os.Lstat(marker)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect warm marker %s: %w", marker, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("warm marker %s is not a regular file", marker)
	}
	data, err := os.ReadFile(marker) //nolint:gosec // marker is a verified regular file under CACHE_DIR.
	if err != nil {
		return false, fmt.Errorf("read warm marker %s: %w", marker, err)
	}
	if string(data) != key+"\n" {
		return false, nil
	}
	return mirrorDirectoryReady(mirrorDir, providers, platforms)
}

// mirrorDirectoryReady proves every provider/platform artifact selected by the
// committed lock files still exists. Merely finding one root entry is not
// enough: deleting one provider subtree from a multi-provider mirror used to
// leave the marker fast path green while the corresponding init failed offline.
func mirrorDirectoryReady(path string, providers []tf.LockedProvider, platforms []string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect provider mirror %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("provider mirror %s is a symbolic link; refusing a cache alias or escape", path)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("provider mirror %s is not a directory", path)
	}
	wanted := make(map[string]struct{})
	for _, provider := range providers {
		source := provider.Source
		if strings.Count(source, "/") == 1 {
			source = "registry.terraform.io/" + source
		}
		providerDir := filepath.FromSlash(source)
		wanted[filepath.Join(providerDir, "index.json")] = struct{}{}
		wanted[filepath.Join(providerDir, provider.Version+".json")] = struct{}{}
		for _, platform := range platforms {
			name := fmt.Sprintf("terraform-provider-%s_%s_%s.zip", provider.Type(), provider.Version, platform)
			wanted[filepath.Join(providerDir, name)] = struct{}{}
		}
	}
	paths := make([]string, 0, len(wanted))
	for rel := range wanted {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		present, err := regularMirrorFile(path, rel)
		if err != nil {
			return false, err
		}
		if !present {
			return false, nil
		}
	}
	return len(paths) > 0, nil
}

// regularMirrorFile checks every directory component with Lstat before the
// file, so a tampered provider subtree cannot redirect readiness outside the
// promoted mirror through a symlink.
func regularMirrorFile(root, rel string) (bool, error) {
	parts := strings.Split(filepath.Clean(rel), string(filepath.Separator))
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("inspect provider mirror directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("provider mirror directory %s is not a real directory", current)
		}
	}
	path := filepath.Join(root, rel)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect provider mirror artifact %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("provider mirror artifact %s is not a regular file", path)
	}
	return true, nil
}

// promoteMirror installs a complete staging mirror, adopting a complete winner
// of a same-key race. If an existing content-addressed directory is incomplete,
// it is first renamed to a sweepable quarantine and then replaced. That keeps
// the repair crash-safe: a killed repair leaves either the old directory, a
// complete new directory, or a quarantine the next always-run warm can ignore
// while promoting its own complete staging tree.
func promoteMirror(
	staging, mirrorDir, cacheDir, key string,
	providers []tf.LockedProvider,
	platforms []string,
	logOut io.Writer,
) error {
	if err := os.Rename(staging, mirrorDir); err == nil {
		complete, checkErr := mirrorDirectoryReady(mirrorDir, providers, platforms)
		if checkErr != nil {
			return checkErr
		}
		if !complete {
			return fmt.Errorf("promoted mirror %s is incomplete", mirrorDir)
		}
		return nil
	} else {
		complete, checkErr := mirrorDirectoryReady(mirrorDir, providers, platforms)
		if checkErr != nil {
			return checkErr
		}
		if complete {
			_, _ = fmt.Fprintf(logOut, "%s: mirror %s was populated concurrently; adopting it\n", roleName, key)
			return nil
		}

		// Reserve a unique name without leaving an empty directory for Rename to
		// reject. The providers.tmp prefix makes a crash leftover eligible for the
		// existing age-based sweep.
		quarantine, reserveErr := os.MkdirTemp(cacheDir, "providers.tmp."+key+".incomplete.")
		if reserveErr != nil {
			return fmt.Errorf("reserve quarantine for incomplete mirror %s: %w", mirrorDir, reserveErr)
		}
		if removeErr := os.Remove(quarantine); removeErr != nil {
			return fmt.Errorf("prepare quarantine %s: %w", quarantine, removeErr)
		}

		if quarantineErr := os.Rename(mirrorDir, quarantine); quarantineErr != nil {
			// Another repair may have moved the incomplete directory. Compete at
			// the atomic promotion seam instead; exactly one complete staging tree
			// wins and every loser can adopt it.
			if promoteErr := os.Rename(staging, mirrorDir); promoteErr == nil {
				complete, checkErr = mirrorDirectoryReady(mirrorDir, providers, platforms)
				if checkErr != nil {
					return checkErr
				}
				if !complete {
					return fmt.Errorf("concurrently replaced mirror %s is incomplete", mirrorDir)
				}
				_, _ = fmt.Fprintf(logOut, "%s: replaced incomplete mirror %s concurrently\n", roleName, key)
				return nil
			}
			complete, checkErr = mirrorDirectoryReady(mirrorDir, providers, platforms)
			if checkErr != nil {
				return checkErr
			}
			if complete {
				_, _ = fmt.Fprintf(logOut, "%s: incomplete mirror %s was repaired concurrently; adopting it\n", roleName, key)
				return nil
			}
			return fmt.Errorf("quarantine incomplete mirror %s: %w", mirrorDir, quarantineErr)
		}

		if promoteErr := os.Rename(staging, mirrorDir); promoteErr != nil {
			complete, checkErr = mirrorDirectoryReady(mirrorDir, providers, platforms)
			if checkErr == nil && complete {
				_ = os.RemoveAll(quarantine)
				_, _ = fmt.Fprintf(logOut, "%s: incomplete mirror %s was repaired concurrently; adopting it\n", roleName, key)
				return nil
			}
			// Best-effort rollback only when nobody else installed a winner.
			if _, statErr := os.Lstat(mirrorDir); errors.Is(statErr, fs.ErrNotExist) {
				if restoreErr := os.Rename(quarantine, mirrorDir); restoreErr != nil {
					return fmt.Errorf("promote repaired mirror %s: %v (also could not restore quarantine %s: %w)",
						mirrorDir, promoteErr, quarantine, restoreErr)
				}
			}
			return fmt.Errorf("promote repaired mirror %s: %w", mirrorDir, promoteErr)
		}

		_ = os.RemoveAll(quarantine)
		complete, checkErr = mirrorDirectoryReady(mirrorDir, providers, platforms)
		if checkErr != nil {
			return checkErr
		}
		if !complete {
			return fmt.Errorf("repaired mirror %s is incomplete", mirrorDir)
		}
		_, _ = fmt.Fprintf(logOut, "%s: replaced incomplete mirror %s\n", roleName, key)
		return nil
	}
}

// consumerMirrorPath is the mirror directory as CONSUMING containers resolve it.
func consumerMirrorPath(cfg config, key string) string {
	return filepath.Join(cfg.MountPath, "providers", key)
}

// ensureTerraformRC writes the CLI configuration unless it already names exactly
// this mirror.
//
// The file lives at CLIConfigFile(CACHE_DIR, CACHE_KEY): the unkeyed historical
// slot, or a per-key path so two provider sets can share one volume. This
// function guarantees only that a warm which RUNS leaves ITS slot pointing at
// its own mirror, including on the marker fast path. Jobs that share a slot
// still last-writer-wins on that file; distinct CACHE_KEY values are what
// keeps them from clobbering each other.
func ensureTerraformRC(cfg config, key string, logOut io.Writer) error {
	path := tf.CLIConfigFile(cfg.CacheDir, cfg.CacheKey)
	want := tf.TerraformRC(consumerMirrorPath(cfg, key))

	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Written below.
	case err != nil:
		return fmt.Errorf("inspect %s: %w", path, err)
	case info.Mode()&os.ModeSymlink != 0:
		// The parent slot was already proven to be a real directory, so an atomic
		// rename replaces this link itself and cannot follow it out of the cache.
		_, _ = fmt.Fprintf(logOut, "%s: %s was a symbolic link; replacing it with a regular file\n", roleName, path)
	case !info.Mode().IsRegular():
		return fmt.Errorf("terraform CLI config %s is not a regular file", path)
	default:
		current, readErr := os.ReadFile(path) //nolint:gosec // Lstat proved this path is a regular file.
		if readErr != nil {
			return fmt.Errorf("read %s: %w", path, readErr)
		}
		if string(current) == want {
			if info.Mode().Perm() != 0o644 {
				if err := os.Chmod(path, 0o644); err != nil {
					return fmt.Errorf("make %s readable by cache consumers: %w", path, err)
				}
			}
			return nil
		}
		_, _ = fmt.Fprintf(logOut, "%s: %s named a different mirror; re-pointing it at %s\n",
			roleName, path, consumerMirrorPath(cfg, key))
	}
	return writeFileAtomic(path, []byte(want), 0o644)
}

// stagingMaxAge is how long an abandoned staging directory is left alone. It is
// generously longer than any real `providers mirror` run, because deleting a
// SIBLING'S in-flight mirror is the failure this sweep exists to avoid and a
// leaked scratch directory is only wasted bytes.
const stagingMaxAge = 6 * time.Hour

// sweepStaleStaging removes staging directories abandoned by killed warms: only
// those older than stagingMaxAge, and never our own.
func sweepStaleStaging(cacheDir, own string, logOut io.Writer) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-stagingMaxAge)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "providers.tmp.") {
			continue
		}
		path := filepath.Join(cacheDir, entry.Name())
		if path == own {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(path); err == nil {
			_, _ = fmt.Fprintf(logOut, "%s: swept abandoned staging directory %s\n", roleName, path)
		}
	}
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

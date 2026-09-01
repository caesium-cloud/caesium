package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/reagents/internal/tf"
)

const fixtureLock = `# This file is maintained automatically by "terraform init".

provider "registry.terraform.io/hashicorp/null" {
  version     = "3.3.1"
  constraints = "~> 3.2"
  hashes = [
    "h1:4pjRixNj9/nijyC0jrCr8tYOpZ8afFwZ2M86y81PMa0=",
    "zh:08c59776542ea16e5a8545752787b17ff412922182b4cfabe16139197be8ac44",
  ]
}

provider "registry.terraform.io/hashicorp/random" {
  version     = "3.9.0"
  constraints = "~> 3.6"
  hashes = [
    "h1:UlBuNVuCGJ39tTv2c5gz2NRZnQbXfbIWbTzWcth5o74=",
  ]
}
`

// fakeTerraform writes a stand-in `terraform` binary that answers the version
// probe terraform-exec makes and records every other invocation, creating the
// mirror directory the real command would populate.
//
// A fake rather than the real CLI: `providers mirror` is the one operation in
// this role that genuinely needs the network, and the properties under test
// here — the marker gate, the atomic promotion, the generated CLI config — are
// exactly the ones that must hold whether or not a download happened.
func fakeTerraform(t *testing.T, exitCode int) (execPath, recordPath string) {
	t.Helper()
	dir := t.TempDir()
	execPath = filepath.Join(dir, "terraform")
	recordPath = filepath.Join(dir, "invocations")

	script := `#!/bin/sh
if [ "$1" = "version" ]; then
  echo '{"terraform_version":"1.15.9","platform":"linux_amd64","provider_selections":{},"terraform_outdated":false}'
  exit 0
fi
echo "$*" >> "` + recordPath + `"
platforms=""
for a in "$@"; do
  case "$a" in
    -platform=*) platforms="$platforms ${a#-platform=}" ;;
    *) target="$a" ;;
  esac
done
awk '
$1 == "provider" { gsub(/"/, "", $2); source = $2 }
$1 == "version" { gsub(/"/, "", $3); print source, $3 }
' .terraform.lock.hcl | while read -r source version; do
  provider_type="${source##*/}"
  dir="$target/$source"
  mkdir -p "$dir"
  echo '{}' > "$dir/index.json"
  echo '{}' > "$dir/$version.json"
  for platform in $platforms; do
    echo mirrored > "$dir/terraform-provider-${provider_type}_${version}_${platform}.zip"
  done
  # A real mirror writes one package at a time. Pausing between providers is
  # what makes a concurrent warm able to observe — or destroy — a partial tree.
  [ -n "${CAESIUM_FAKE_MIRROR_DELAY:-}" ] && sleep "$CAESIUM_FAKE_MIRROR_DELAY"
done
cp providers.tf "$target/providers.tf.seen" 2>/dev/null || true
exit ` + strconv.Itoa(exitCode) + `
`
	if err := os.WriteFile(execPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return execPath, recordPath
}

func invocations(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-local path.
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// newWarmFixture lays out a source tree with two stacks that share one provider
// set, plus an empty cache volume.
func newWarmFixture(t *testing.T) (src, cache string) {
	t.Helper()
	root := t.TempDir()
	src = filepath.Join(root, "src")
	cache = filepath.Join(root, "cache")
	for _, stack := range []string{"network", "app-web"} {
		dir := filepath.Join(src, "stacks", stack)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, tf.LockFileName), []byte(fixtureLock), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	return src, cache
}

func warmConfig(t *testing.T, src, cache, execPath string) config {
	t.Helper()
	return config{
		Src:       src,
		CacheDir:  cache,
		MountPath: cache,
		Platforms: []string{"linux_amd64"},
		ExecPath:  execPath,
	}
}

func TestWarmPopulatesTheMirrorAndDropsItsMarker(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)

	var log bytes.Buffer
	if err := warm(context.Background(), warmConfig(t, src, cache, execPath), &log); err != nil {
		t.Fatalf("warm: %v", err)
	}

	if got := invocations(t, record); len(got) != 1 {
		t.Fatalf("want exactly one providers-mirror invocation, got %v", got)
	} else if !strings.Contains(got[0], "providers mirror") || !strings.Contains(got[0], "-platform=linux_amd64") {
		t.Fatalf("unexpected invocation %q", got[0])
	}

	markerDir := filepath.Join(cache, ".warm")
	entries, err := os.ReadDir(markerDir)
	if err != nil {
		t.Fatalf("read marker directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one marker, got %v", entries)
	}
	key := entries[0].Name()

	// The mirror must have been promoted to its keyed home and the staging
	// directory cleaned up: a staging directory left behind would be mistaken
	// for cache content by anyone reading the volume.
	if _, err := os.Stat(filepath.Join(cache, "providers", key, "registry.terraform.io", "hashicorp", "null", "index.json")); err != nil {
		t.Fatalf("mirror was not promoted: %v", err)
	}
	if leftovers := stagingDirs(t, cache); len(leftovers) != 0 {
		t.Fatalf("staging directories survived: %v", leftovers)
	}

	rc, err := os.ReadFile(filepath.Join(cache, "terraformrc")) //nolint:gosec // test-local path.
	if err != nil {
		t.Fatalf("read terraformrc: %v", err)
	}
	if !strings.Contains(string(rc), filepath.Join(cache, "providers", key)) {
		t.Fatalf("terraformrc does not point at the promoted mirror:\n%s", rc)
	}
}

// The always-run + marker check is the whole reason the warm step is never
// given a Caesium cache block: a cache hit means no container ran, and a
// recreated volume would then be empty. So the SECOND run must still start,
// still look inside the volume, and only then exit fast.
func TestWarmSecondRunExitsOnTheMarkerWithoutRunningTerraform(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)

	var first bytes.Buffer
	if err := warm(context.Background(), cfg, &first); err != nil {
		t.Fatalf("first warm: %v", err)
	}
	before := len(invocations(t, record))

	var second bytes.Buffer
	if err := warm(context.Background(), cfg, &second); err != nil {
		t.Fatalf("second warm: %v", err)
	}
	if after := len(invocations(t, record)); after != before {
		t.Fatalf("second warm re-ran terraform (%d invocations, was %d)", after, before)
	}
	if !strings.Contains(second.String(), "already warm") {
		t.Fatalf("second warm should say it found the marker, said: %q", second.String())
	}
}

// Self-healing is the property the marker-in-the-volume design buys: if the
// volume is recreated, the marker is gone with it and the next run repopulates.
func TestWarmRepopulatesAfterTheVolumeIsRecreated(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)

	if err := warm(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatalf("first warm: %v", err)
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := warm(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatalf("second warm: %v", err)
	}
	if got := len(invocations(t, record)); got != 2 {
		t.Fatalf("an emptied volume must be re-mirrored; got %d invocations", got)
	}
}

// A changed lock file must move the key, or the marker from the previous
// provider set would let warm exit fast against a mirror missing the new one.
func TestWarmReMirrorsWhenTheLockFileChanges(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)

	if err := warm(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatalf("first warm: %v", err)
	}
	bumped := strings.Replace(fixtureLock, `version     = "3.3.1"`, `version     = "3.4.0"`, 1)
	for _, stack := range []string{"network", "app-web"} {
		if err := os.WriteFile(filepath.Join(src, "stacks", stack, tf.LockFileName), []byte(bumped), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := warm(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatalf("second warm: %v", err)
	}

	if got := len(invocations(t, record)); got != 2 {
		t.Fatalf("a bumped provider version must re-mirror; got %d invocations", got)
	}
	entries, err := os.ReadDir(filepath.Join(cache, ".warm"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want a marker per provider set, got %v", entries)
	}
}

// Two stacks pinning two different versions of the same provider both have to
// be able to init offline, so both packages must reach the mirror. A single
// required_providers block can only pin one version, hence more than one pass.
func TestWarmMirrorsEveryPinnedVersionOfTheSameProvider(t *testing.T) {
	src, cache := newWarmFixture(t)
	older := strings.Replace(fixtureLock, `version     = "3.3.1"`, `version     = "3.2.0"`, 1)
	if err := os.WriteFile(filepath.Join(src, "stacks", "app-web", tf.LockFileName), []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	execPath, record := fakeTerraform(t, 0)

	if err := warm(context.Background(), warmConfig(t, src, cache, execPath), &bytes.Buffer{}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if got := invocations(t, record); len(got) != 2 {
		t.Fatalf("want one mirror pass per distinct pinned version, got %v", got)
	}
}

// Every failure mode has to leave the marker absent: a marker is a promise that
// the mirror behind it is complete, and a run that exits fast on a false
// promise fails every downstream init with an unrelated diagnosis.
func TestWarmFailuresNeverDropAMarker(t *testing.T) {
	t.Run("mirror command fails", func(t *testing.T) {
		src, cache := newWarmFixture(t)
		execPath, _ := fakeTerraform(t, 1)
		if err := warm(context.Background(), warmConfig(t, src, cache, execPath), &bytes.Buffer{}); err == nil {
			t.Fatal("warm succeeded despite a failing mirror")
		}
		assertNoMarker(t, cache)
	})

	t.Run("no lock files", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		cache := filepath.Join(root, "cache")
		if err := os.MkdirAll(filepath.Join(src, "stacks", "network"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(cache, 0o755); err != nil {
			t.Fatal(err)
		}
		execPath, _ := fakeTerraform(t, 0)
		err := warm(context.Background(), warmConfig(t, src, cache, execPath), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tf.LockFileName) {
			t.Fatalf("want a lock-file error, got %v", err)
		}
		assertNoMarker(t, cache)
	})

	t.Run("unparseable lock file", func(t *testing.T) {
		src, cache := newWarmFixture(t)
		if err := os.WriteFile(filepath.Join(src, "stacks", "network", tf.LockFileName), []byte("garbage\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		execPath, record := fakeTerraform(t, 0)
		if err := warm(context.Background(), warmConfig(t, src, cache, execPath), &bytes.Buffer{}); err == nil {
			t.Fatal("warm succeeded on an unparseable lock file")
		}
		if got := invocations(t, record); len(got) != 0 {
			t.Fatalf("terraform ran despite an unreadable lock file: %v", got)
		}
		assertNoMarker(t, cache)
	})
}

func assertNoMarker(t *testing.T, cache string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cache, ".warm"))
	if err == nil && len(entries) > 0 {
		t.Fatalf("a failed warm dropped markers: %v", entries)
	}
}

// Consumers resolve the mirror path in their OWN mount namespace, so the
// generated CLI configuration has to be written in their view of the volume,
// not the warm container's.
func TestWarmWritesTheConsumerViewOfTheMirrorPath(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, _ := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)
	cfg.MountPath = "/cache"

	if err := warm(context.Background(), cfg, &bytes.Buffer{}); err != nil {
		t.Fatalf("warm: %v", err)
	}
	rc, err := os.ReadFile(filepath.Join(cache, "terraformrc")) //nolint:gosec // test-local path.
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rc), `path    = "/cache/providers/`) {
		t.Fatalf("terraformrc uses the warm container's path, not the consumers':\n%s", rc)
	}
}

// The role emits nothing. An output would make every consumer's cache key
// depend on a step whose entire purpose is to be invisible to them — and the
// warm step always runs, so that dependency would never be stable.
func TestWarmEmitsNoMarkers(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, _ := fakeTerraform(t, 0)

	var log bytes.Buffer
	if err := warm(context.Background(), warmConfig(t, src, cache, execPath), &log); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if strings.Contains(log.String(), "##caesium::") {
		t.Fatalf("warm emitted a marker: %q", log.String())
	}
}

func TestLoadConfigDefaultsAndValidation(t *testing.T) {
	src := t.TempDir()
	env := map[string]string{"SRC": src, "TF_CLI_PATH": "/usr/bin/terraform"}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.CacheDir != tf.DefaultCacheDir || cfg.MountPath != tf.DefaultCacheDir {
		t.Fatalf("cache defaults = %q/%q", cfg.CacheDir, cfg.MountPath)
	}
	if len(cfg.Platforms) != 1 || !strings.Contains(cfg.Platforms[0], "_") {
		t.Fatalf("platform default = %v", cfg.Platforms)
	}

	env["TARGET_PLATFORM"] = "linux_arm64, linux_amd64"
	cfg, err = loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if strings.Join(cfg.Platforms, ",") != "linux_amd64,linux_arm64" {
		t.Fatalf("platforms = %v (want sorted, so the mirror key is order-independent)", cfg.Platforms)
	}

	// The platform reaches a command line, so it is validated rather than
	// passed through.
	for _, bad := range []string{"linux", "linux_amd64;rm -rf /", "-flag_amd64"} {
		env["TARGET_PLATFORM"] = bad
		if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
			t.Fatalf("loadConfig accepted TARGET_PLATFORM %q", bad)
		}
	}

	delete(env, "TARGET_PLATFORM")
	env["SRC"] = filepath.Join(src, "missing")
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a missing SRC")
	}

	env["SRC"] = src
	env["CACHE_DIR"] = "relative/cache"
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a relative CACHE_DIR")
	}
}

// stagingDirs lists the staging directories left in a cache volume.
func stagingDirs(t *testing.T, cache string) []string {
	t.Helper()
	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "providers.tmp.") {
			out = append(out, entry.Name())
		}
	}
	return out
}

// Design §3.5 and §6.3 both justify having NO named lock by saying concurrent
// warms of one key are benign. They are only benign if each warm stages into a
// directory of its own: sharing one directory per key lets a second warm clear
// it mid-flight, after which the first promotes an incomplete mirror and drops a
// marker vouching for it — and every later run exits fast on that marker while
// every consuming init fails offline.
//
// With the shared-staging implementation this test fails: one of the two mirrors
// is promoted missing a provider.
func TestConcurrentWarmsOfOneKeyBothPromoteACompleteMirror(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)
	t.Setenv("CAESIUM_FAKE_MIRROR_DELAY", "0.4")
	cfg := warmConfig(t, src, cache, execPath)

	// The second warm is staggered into the MIDDLE of the first one's mirror, so
	// it clears staging after the first package has been written and before the
	// second. Starting them simultaneously does not reproduce the bug: both
	// would clear an empty directory before either had written anything.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i > 0 {
				time.Sleep(200 * time.Millisecond)
			}
			errs[i] = warm(context.Background(), cfg, io.Discard)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("warm %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(cache, ".warm"))
	if err != nil {
		t.Fatalf("read marker directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one marker for one provider set, got %v", entries)
	}
	key := entries[0].Name()

	// The promoted mirror must hold EVERY package. A marker over a truncated
	// mirror is the silent failure this whole design is built to avoid.
	for _, provider := range []string{"null", "random"} {
		path := filepath.Join(cache, "providers", key, "registry.terraform.io", "hashicorp", provider, "index.json")
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the promoted mirror is missing %s, yet a marker vouches for it: %v", provider, err)
		}
	}
	if leftovers := stagingDirs(t, cache); len(leftovers) != 0 {
		t.Fatalf("staging directories survived: %v", leftovers)
	}

	// The invariant, asserted directly rather than raced for: the two warms
	// staged into DIFFERENT directories.
	//
	// Racing for the truncated-promotion outcome is not deterministic — it needs
	// the second warm's clear to land in the window between the first one's last
	// package write and its rename, which no seam here controls. The property
	// that makes the outcome impossible is testable exactly, and it is the one
	// design §3.5 and §6.3 actually claim ("concurrent warms of the same key are
	// benign"): with a shared per-key staging path this assertion fails, because
	// both mirrors are handed the same target.
	targets := map[string]struct{}{}
	for _, invocation := range invocations(t, record) {
		fields := strings.Fields(invocation)
		targets[fields[len(fields)-1]] = struct{}{}
	}
	if len(targets) < 2 {
		t.Fatalf("both warms staged into the same directory (%v); "+
			"one clearing it mid-flight can leave the other promoting a truncated mirror and vouching for it", targets)
	}
}

// A staging directory abandoned by a killed warm is swept, but only by AGE —
// deleting a sibling's in-flight staging directory is the failure the sweep
// exists to avoid.
func TestWarmSweepsOnlyAbandonedStagingDirectories(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, _ := fakeTerraform(t, 0)

	stale := filepath.Join(cache, "providers.tmp.deadbeef.oldrun")
	fresh := filepath.Join(cache, "providers.tmp.deadbeef.liverun")
	for _, dir := range []string{stale, fresh} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * stagingMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if err := warm(context.Background(), warmConfig(t, src, cache, execPath), io.Discard); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("an abandoned staging directory was not swept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a possibly in-flight staging directory was deleted: %v", err)
	}
}

// The mirror is keyed, but the CLI configuration pointing at one is a single
// global filename. A warm that exits fast on its own marker must still repair a
// terraformrc another provider set clobbered, or every consuming init resolves
// against the wrong mirror and fails offline with an unrelated diagnosis.
func TestWarmFastPathRepairsATerraformRCPointingElsewhere(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)

	if err := warm(context.Background(), cfg, io.Discard); err != nil {
		t.Fatalf("first warm: %v", err)
	}
	before := len(invocations(t, record))
	rcPath := filepath.Join(cache, "terraformrc")
	if err := os.WriteFile(rcPath, []byte(tf.TerraformRC("/cache/providers/some-other-key")), 0o600); err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	if err := warm(context.Background(), cfg, &log); err != nil {
		t.Fatalf("second warm: %v", err)
	}
	if after := len(invocations(t, record)); after != before {
		t.Fatalf("the fast path re-ran terraform (%d invocations, was %d)", after, before)
	}
	rc, err := os.ReadFile(rcPath) //nolint:gosec // test-local path.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rc), "some-other-key") {
		t.Fatalf("the fast path left terraformrc pointing at another mirror:\n%s", rc)
	}
	entries, err := os.ReadDir(filepath.Join(cache, ".warm"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rc), entries[0].Name()) {
		t.Fatalf("terraformrc does not name this warm's mirror:\n%s", rc)
	}
	if !strings.Contains(log.String(), "re-pointing") {
		t.Fatalf("the repair was not reported: %q", log.String())
	}
}

// A marker is a promise about the mirror, not a substitute for it. If the
// mirror directory disappeared while the marker survived, the next always-run
// warm must rebuild it rather than returning green with terraformrc pointing at
// a path every offline init will fail to open.
func TestWarmStaleMarkerRebuildsAMissingMirror(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)

	if err := warm(context.Background(), cfg, io.Discard); err != nil {
		t.Fatalf("first warm: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(cache, ".warm"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("find mirror key: entries=%v err=%v", entries, err)
	}
	key := entries[0].Name()
	mirror := filepath.Join(cache, "providers", key)
	if err := os.RemoveAll(mirror); err != nil {
		t.Fatal(err)
	}

	if err := warm(context.Background(), cfg, io.Discard); err != nil {
		t.Fatalf("repair warm: %v", err)
	}
	if got := len(invocations(t, record)); got != 2 {
		t.Fatalf("a stale marker over a missing mirror must re-run terraform; got %d invocations", got)
	}
	if _, err := os.Stat(filepath.Join(mirror, "registry.terraform.io", "hashicorp", "null", "index.json")); err != nil {
		t.Fatalf("the stale marker's mirror was not rebuilt: %v", err)
	}
}

func TestWarmStaleMarkerRepairsAPartiallyDeletedMirror(t *testing.T) {
	src, cache := newWarmFixture(t)
	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)

	if err := warm(context.Background(), cfg, io.Discard); err != nil {
		t.Fatalf("first warm: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(cache, ".warm"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("find mirror key: entries=%v err=%v", entries, err)
	}
	key := entries[0].Name()
	missing := filepath.Join(cache, "providers", key, "registry.terraform.io", "hashicorp", "null",
		"terraform-provider-null_3.3.1_linux_amd64.zip")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}

	if err := warm(context.Background(), cfg, io.Discard); err != nil {
		t.Fatalf("repair warm: %v", err)
	}
	if got := len(invocations(t, record)); got != 2 {
		t.Fatalf("a marker over a partial mirror must re-run terraform; got %d invocations", got)
	}
	if _, err := os.Stat(missing); err != nil {
		t.Fatalf("the missing provider package was not repaired: %v", err)
	}
	if leftovers := stagingDirs(t, cache); len(leftovers) != 0 {
		t.Fatalf("incomplete-mirror repair left quarantine/staging directories: %v", leftovers)
	}
}

func TestWarmKeyedSlotFailsClosedOnDirectoryAlias(t *testing.T) {
	src, cache := newWarmFixture(t)
	out := t.TempDir()
	if err := os.Symlink(out, filepath.Join(cache, "deploy")); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}
	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)
	cfg.CacheKey = "deploy"

	err := warm(context.Background(), cfg, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("warm followed a CACHE_KEY directory alias: %v", err)
	}
	if got := invocations(t, record); len(got) != 0 {
		t.Fatalf("terraform ran before the unsafe slot was refused: %v", got)
	}
	if _, err := os.Stat(filepath.Join(out, tf.TerraformRCName)); !os.IsNotExist(err) {
		t.Fatalf("warm wrote through the slot alias: %v", err)
	}
}

func TestWarmAtomicallyReplacesTerraformRCSymlinkAndRepairsModes(t *testing.T) {
	src, cache := newWarmFixture(t)
	key := "deploy"
	slot := filepath.Join(cache, key)
	if err := os.Mkdir(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside-terraformrc")
	const outside = "must stay untouched\n"
	if err := os.WriteFile(target, []byte(outside), 0o600); err != nil {
		t.Fatal(err)
	}
	rcPath := tf.CLIConfigFile(cache, key)
	if err := os.Symlink(target, rcPath); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	execPath, record := fakeTerraform(t, 0)
	cfg := warmConfig(t, src, cache, execPath)
	cfg.CacheKey = key
	if err := warm(context.Background(), cfg, io.Discard); err != nil {
		t.Fatalf("warm: %v", err)
	}
	info, err := os.Lstat(rcPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("terraformrc was not replaced with a regular file: %v", info.Mode())
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != outside {
		t.Fatalf("warm wrote through the old symlink: data=%q err=%v", data, err)
	}

	// Fast-path repair must also restore traversal/read modes without another
	// provider download. A different consumer uid otherwise sees a green warm
	// followed by a permission-denied init.
	if err := os.Chmod(slot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rcPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := warm(context.Background(), cfg, io.Discard); err != nil {
		t.Fatalf("mode repair warm: %v", err)
	}
	if got := len(invocations(t, record)); got != 1 {
		t.Fatalf("mode repair re-downloaded providers: %d invocations", got)
	}
	for path, want := range map[string]os.FileMode{slot: 0o755, rcPath: 0o644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %o, want %o", path, got, want)
		}
	}
}

func TestLoadConfigAcceptsAndRejectsCacheKey(t *testing.T) {
	src := t.TempDir()
	env := map[string]string{"SRC": src, "TF_CLI_PATH": "/usr/bin/terraform"}
	cfg, err := loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.CacheKey != "" {
		t.Fatalf("CacheKey = %q (unset CACHE_KEY is the unkeyed slot)", cfg.CacheKey)
	}

	env["CACHE_KEY"] = "deploy"
	cfg, err = loadConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.CacheKey != "deploy" {
		t.Fatalf("CacheKey = %q", cfg.CacheKey)
	}

	env["CACHE_KEY"] = "../escape"
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil {
		t.Fatal("loadConfig accepted a path-like CACHE_KEY")
	}
}

// Two jobs whose lock files resolve to different provider sets can share one
// tfcache volume when each names a CACHE_KEY: the CLI configuration is a file
// per slot, so the second warm cannot flip the first's terraformrc. That is
// the whole of #364.
func TestWarmTwoCacheKeysOnOneVolumeDoNotClobber(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	srcA := writeLockTree(t, filepath.Join(root, "src-a"), fixtureLock)
	bumped := strings.Replace(fixtureLock, `version     = "3.3.1"`, `version     = "3.4.0"`, 1)
	srcB := writeLockTree(t, filepath.Join(root, "src-b"), bumped)
	execPath, record := fakeTerraform(t, 0)

	cfgA := warmConfig(t, srcA, cache, execPath)
	cfgA.CacheKey = "set-a"
	cfgB := warmConfig(t, srcB, cache, execPath)
	cfgB.CacheKey = "set-b"

	if err := warm(context.Background(), cfgA, io.Discard); err != nil {
		t.Fatalf("warm set-a: %v", err)
	}
	if err := warm(context.Background(), cfgB, io.Discard); err != nil {
		t.Fatalf("warm set-b: %v", err)
	}
	if got := invocations(t, record); len(got) != 2 {
		t.Fatalf("want one mirror pass per provider set, got %v", got)
	}

	rcA := readTerraformRC(t, cache, "set-a")
	rcB := readTerraformRC(t, cache, "set-b")
	if rcA == rcB {
		t.Fatalf("both slots point at the same mirror:\n%s", rcA)
	}
	if _, err := os.Stat(tf.CLIConfigFile(cache, "")); !os.IsNotExist(err) {
		t.Fatalf("keyed warms must not write the unkeyed terraformrc: %v", err)
	}

	// Re-warming A repairs its own slot (the fast-path property) without
	// touching B — the clobber the unkeyed filename could not prevent.
	if err := os.WriteFile(tf.CLIConfigFile(cache, "set-a"), []byte(tf.TerraformRC("/cache/providers/clobbered")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := warm(context.Background(), cfgA, io.Discard); err != nil {
		t.Fatalf("re-warm set-a: %v", err)
	}
	if after := invocations(t, record); len(after) != 2 {
		t.Fatalf("re-warming an already-warm slot re-ran terraform: %v", after)
	}
	if strings.Contains(readTerraformRC(t, cache, "set-a"), "clobbered") {
		t.Fatal("set-a's fast path left terraformrc pointing at another mirror")
	}
	if got := readTerraformRC(t, cache, "set-b"); got != rcB {
		t.Fatalf("re-warming set-a clobbered set-b:\n%s", got)
	}
}

func TestConcurrentWarmsOfDifferentSlotsDoNotClobber(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	srcA := writeLockTree(t, filepath.Join(root, "src-a"), fixtureLock)
	bumped := strings.Replace(fixtureLock, `version     = "3.3.1"`, `version     = "3.4.0"`, 1)
	srcB := writeLockTree(t, filepath.Join(root, "src-b"), bumped)
	execPath, _ := fakeTerraform(t, 0)
	t.Setenv("CAESIUM_FAKE_MIRROR_DELAY", "0.2")

	cfgs := []config{
		warmConfig(t, srcA, cache, execPath),
		warmConfig(t, srcB, cache, execPath),
	}
	cfgs[0].CacheKey = "set-a"
	cfgs[1].CacheKey = "set-b"

	errCh := make(chan error, len(cfgs))
	var wg sync.WaitGroup
	for _, cfg := range cfgs {
		cfg := cfg
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- warm(context.Background(), cfg, io.Discard)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent warm: %v", err)
		}
	}

	rcA := readTerraformRC(t, cache, "set-a")
	rcB := readTerraformRC(t, cache, "set-b")
	if rcA == rcB {
		t.Fatalf("concurrent warms left both slots pointing at one mirror:\n%s", rcA)
	}
	for _, key := range []string{"set-a", "set-b"} {
		info, err := os.Stat(filepath.Join(cache, key))
		if err != nil {
			t.Fatalf("stat slot %s: %v", key, err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("slot %s mode = %o, want 755", key, info.Mode().Perm())
		}
	}
}

func writeLockTree(t *testing.T, src, lock string) string {
	t.Helper()
	dir := filepath.Join(src, "stacks", "network")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, tf.LockFileName), []byte(lock), 0o600); err != nil {
		t.Fatal(err)
	}
	return src
}

func readTerraformRC(t *testing.T, cache, key string) string {
	t.Helper()
	path := tf.CLIConfigFile(cache, key)
	data, err := os.ReadFile(path) //nolint:gosec // test-local path.
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

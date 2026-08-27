package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pack/internal/protocol"
	"github.com/caesium-cloud/caesium/pack/internal/tf"
)

// fixtureCopy materializes pack/testdata/infra in a temp directory. Every test
// works on its own copy: `terraform get` writes .terraform/ into the root
// module, which must never land back in the repository.
func fixtureCopy(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "infra")
	dst := t.TempDir()
	if err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func testConfig(t *testing.T, scanRoot string) config {
	t.Helper()
	path, err := exec.LookPath("terraform")
	if err != nil {
		t.Skipf("terraform is not on PATH (%v); run this suite via `just pack-test`", err)
	}
	return config{ScanRoot: scanRoot, Workspace: "default", ExecPath: path}
}

// runDiscover executes the role and returns the raw stdout it produced.
func runDiscover(t *testing.T, cfg config) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	e := protocol.New(roleName, &out, &errOut)
	err := discover(context.Background(), cfg, e)
	if flushErr := e.Flush(); flushErr != nil {
		t.Fatalf("flush: %v", flushErr)
	}
	return out.String(), err
}

func singleRootOutput(t *testing.T, cfg config) map[string]string {
	t.Helper()
	stdout, err := runDiscover(t, cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	payload, ok := strings.CutPrefix(strings.TrimRight(stdout, "\n"), protocol.OutputMarker+" ")
	if !ok {
		t.Fatalf("no output marker in %q", stdout)
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(payload), &values); err != nil {
		t.Fatalf("unmarshal %q: %v", payload, err)
	}
	return values
}

func TestSingleRootEmitsFingerprintAndPerInputDigests(t *testing.T) {
	root := fixtureCopy(t)
	values := singleRootOutput(t, testConfig(t, filepath.Join(root, "stacks", "network")))

	if !protocol.ValidDigest(values["fingerprint"]) {
		t.Fatalf("fingerprint = %q", values["fingerprint"])
	}
	// One digest per resolved module plus the workspace, so `why` can name the
	// input that moved rather than only reporting that the key changed.
	for _, key := range []string{"input_root", "input_tags", "input_tags_inner", "input_vpc", "input_workspace"} {
		if !protocol.ValidDigest(values[key]) {
			t.Fatalf("%s = %q, want a sha256 digest (got keys %v)", key, values[key], protocol.SortedKeys(values))
		}
	}
}

func TestFingerprintIsByteIdenticalAcrossRuns(t *testing.T) {
	root := fixtureCopy(t)
	cfg := testConfig(t, filepath.Join(root, "stacks", "network"))

	first := singleRootOutput(t, cfg)
	second := singleRootOutput(t, cfg)
	if first["fingerprint"] != second["fingerprint"] {
		t.Fatalf("fingerprint is not deterministic: %q vs %q", first["fingerprint"], second["fingerprint"])
	}

	// A separate copy of the same tree, at a different absolute path, must
	// produce the same fingerprint: an absolute path leaking into the digest
	// would split the cache between workers.
	other := fixtureCopy(t)
	third := singleRootOutput(t, testConfig(t, filepath.Join(other, "stacks", "network")))
	if third["fingerprint"] != first["fingerprint"] {
		t.Fatalf("fingerprint depends on the checkout path: %q vs %q", first["fingerprint"], third["fingerprint"])
	}
}

func TestFingerprintCoversTheNestedRelativeModule(t *testing.T) {
	root := fixtureCopy(t)
	cfg := testConfig(t, filepath.Join(root, "stacks", "network"))
	before := singleRootOutput(t, cfg)

	// modules/tags/inner is reached by a relative source two levels below the
	// root module. `terraform modules -json` cannot resolve it to a directory,
	// so a fingerprint built from that surface would not move here.
	inner := filepath.Join(root, "modules", "tags", "inner", "main.tf")
	data, err := os.ReadFile(inner)
	if err != nil {
		t.Fatalf("read inner module: %v", err)
	}
	if err := os.WriteFile(inner, append(data, []byte("\n# touched\n")...), 0o644); err != nil {
		t.Fatalf("edit inner module: %v", err)
	}

	after := singleRootOutput(t, cfg)
	if after["fingerprint"] == before["fingerprint"] {
		t.Fatal("editing modules/tags/inner did not move the fingerprint")
	}
	if after["input_tags_inner"] == before["input_tags_inner"] {
		t.Fatal("the nested module's own input digest did not move")
	}
	if after["input_vpc"] != before["input_vpc"] {
		t.Fatal("an unrelated module's input digest moved")
	}
}

func TestFingerprintCoversEveryDeclaredInputFileKind(t *testing.T) {
	cases := map[string]struct {
		stack string
		file  string
		body  string
	}{
		"tfvars":       {"network", "terraform.tfvars", "cidr_block = \"10.1.0.0/16\"\n"},
		"lock file":    {"network", ".terraform.lock.hcl", "# touched\n"},
		"tfvars.json":  {"app-web", "extra.auto.tfvars.json", "{\n  \"replica_count\": 5\n}\n"},
		"tfquery.hcl":  {"network", "probe.tfquery.hcl", "# a query file\n"},
		"new tf file":  {"network", "extra.tf", "locals {\n  touched = true\n}\n"},
		"module edit":  {"network", "../../modules/vpc/main.tf", ""},
		"tf.json file": {"network", "extra.tf.json", "{\"locals\":{\"touched\":true}}\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := fixtureCopy(t)
			cfg := testConfig(t, filepath.Join(root, "stacks", tc.stack))
			before := singleRootOutput(t, cfg)

			target := filepath.Join(root, "stacks", tc.stack, filepath.FromSlash(tc.file))
			body := tc.body
			if body == "" {
				existing, err := os.ReadFile(target)
				if err != nil {
					t.Fatalf("read %s: %v", tc.file, err)
				}
				body = string(existing) + "\n# touched\n"
			}
			if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
				t.Fatalf("write %s: %v", tc.file, err)
			}

			after := singleRootOutput(t, cfg)
			if after["fingerprint"] == before["fingerprint"] {
				t.Fatalf("changing %s did not move the fingerprint", tc.file)
			}
		})
	}
}

func TestWorkspaceIsPartOfTheFingerprint(t *testing.T) {
	root := fixtureCopy(t)
	base := testConfig(t, filepath.Join(root, "stacks", "network"))

	def := singleRootOutput(t, base)

	staging := base
	staging.Workspace = "staging"
	other := singleRootOutput(t, staging)

	if def["fingerprint"] == other["fingerprint"] {
		t.Fatal("the workspace name is not folded into the fingerprint")
	}
	if def["input_workspace"] == other["input_workspace"] {
		t.Fatal("input_workspace did not move with the workspace")
	}
}

func TestDynamicModuleSourceFailsClosed(t *testing.T) {
	root := fixtureCopy(t)
	cfg := testConfig(t, filepath.Join(root, "fail-closed", "dynamic-source"))

	stdout, err := runDiscover(t, cfg)
	if err == nil {
		t.Fatal("a module source that cannot reduce to a constant must fail discover")
	}
	if stdout != "" {
		t.Fatalf("failed discover still wrote to stdout: %q", stdout)
	}
	if !strings.Contains(err.Error(), "terraform get") {
		t.Fatalf("error should name the failing step, got %v", err)
	}
}

func TestMultiRootEmitsOrderedPartitions(t *testing.T) {
	root := fixtureCopy(t)
	cfg := testConfig(t, filepath.Join(root, "stacks"))

	stdout, err := runDiscover(t, cfg)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	payload, ok := strings.CutPrefix(strings.TrimRight(stdout, "\n"), protocol.PartitionsMarker+" ")
	if !ok {
		t.Fatalf("no partitions marker in %q", stdout)
	}
	var parts []struct {
		Key         string   `json:"key"`
		Fingerprint string   `json:"fingerprint"`
		DependsOn   []string `json:"dependsOn"`
		Root        string   `json:"root"`
	}
	if err := json.Unmarshal([]byte(payload), &parts); err != nil {
		t.Fatalf("unmarshal %q: %v", payload, err)
	}
	if len(parts) != 3 {
		t.Fatalf("got %d partitions, want 3: %v", len(parts), parts)
	}

	byKey := map[string]int{}
	for i, p := range parts {
		byKey[p.Key] = i
		if !protocol.ValidDigest(p.Fingerprint) {
			t.Fatalf("partition %s fingerprint = %q", p.Key, p.Fingerprint)
		}
		if p.Root == "" {
			t.Fatalf("partition %s has no root", p.Key)
		}
		if filepath.IsAbs(p.Root) {
			t.Fatalf("partition %s root %q is absolute; it must be relative to SCAN_ROOT", p.Key, p.Root)
		}
	}
	for _, key := range []string{"network", "account", "app-web"} {
		if _, ok := byKey[key]; !ok {
			t.Fatalf("missing partition %q", key)
		}
	}
	if deps := parts[byKey["network"]].DependsOn; len(deps) != 0 {
		t.Fatalf("network should have no dependencies, got %v", deps)
	}
	for _, key := range []string{"account", "app-web"} {
		deps := parts[byKey[key]].DependsOn
		if len(deps) != 1 || deps[0] != "network" {
			t.Fatalf("%s dependsOn = %v, want [network]", key, deps)
		}
	}

	// account and app-web share modules/tags but nothing else, so their
	// fingerprints must differ: a shared fingerprint would make one stack's
	// edit look like the other's.
	if parts[byKey["account"]].Fingerprint == parts[byKey["app-web"]].Fingerprint {
		t.Fatal("two different stacks produced the same fingerprint")
	}
}

func TestMultiRootFailsClosedOnAnUndeclaredStack(t *testing.T) {
	root := fixtureCopy(t)
	// A new stack on disk that stacks.yaml does not name: silently dropping it
	// would be a green run that deployed nothing.
	extra := filepath.Join(root, "stacks", "orphan")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extra, "main.tf"), []byte("locals {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	stdout, err := runDiscover(t, testConfig(t, filepath.Join(root, "stacks")))
	if err == nil {
		t.Fatal("an undeclared stack directory must fail discover")
	}
	if stdout != "" {
		t.Fatalf("failed discover still wrote to stdout: %q", stdout)
	}
}

func TestMultiRootFailsClosedOnAMissingStackDirectory(t *testing.T) {
	root := fixtureCopy(t)
	if err := os.RemoveAll(filepath.Join(root, "stacks", "account")); err != nil {
		t.Fatalf("remove stack: %v", err)
	}
	if _, err := runDiscover(t, testConfig(t, filepath.Join(root, "stacks"))); err == nil {
		t.Fatal("a declared stack with no directory must fail discover")
	}
}

func TestReadManifestRejectsAnUnexpectedShape(t *testing.T) {
	dataDir := t.TempDir()
	manifestDir := filepath.Join(dataDir, "modules")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A field this parser does not know about: the manifest is an internal file
	// with no compatibility promise, so an unrecognised shape must be a hard
	// failure rather than a fingerprint over a partly-understood document.
	body := `{"Modules":[{"Key":"","Source":"","Dir":".","Unexpected":true}]}`
	if err := os.WriteFile(filepath.Join(manifestDir, "modules.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := tf.ReadManifest(dataDir); err == nil {
		t.Fatal("an unknown manifest field was accepted")
	}
}

func TestDiscoverLeavesTheSourceTreeUntouched(t *testing.T) {
	root := fixtureCopy(t)
	cfg := testConfig(t, filepath.Join(root, "stacks"))
	if _, err := runDiscover(t, cfg); err != nil {
		t.Fatalf("discover: %v", err)
	}
	// The design mounts the source read-only for discover, so `terraform get`
	// must not need to write a .terraform directory into it.
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == tf.DataDirName {
			rel, _ := filepath.Rel(root, path)
			t.Fatalf("discover wrote %s into the source tree", rel)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
}

func TestLoadConfigRequiresAScanRoot(t *testing.T) {
	if _, err := loadConfig(func(string) string { return "" }); err == nil {
		t.Fatal("missing SCAN_ROOT accepted")
	}
	if _, err := loadConfig(func(k string) string {
		if k == "SCAN_ROOT" {
			return filepath.Join(t.TempDir(), "absent")
		}
		return ""
	}); err == nil {
		t.Fatal("nonexistent SCAN_ROOT accepted")
	}

	dir := t.TempDir()
	cfg, err := loadConfig(func(k string) string {
		if k == "SCAN_ROOT" {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Workspace != "default" {
		t.Fatalf("workspace default = %q", cfg.Workspace)
	}
	if cfg.ExecPath == "" {
		t.Fatal("terraform exec path was not resolved")
	}
}

// remoteModuleStack prepares the fixture's non-local-module stack inside an
// already-copied fixture root: it turns remote/subrepo into a git repository
// and renders remote/consumer/main.tf to point at it over git::file://. It
// returns the consumer stack's path.
//
// This is the shape almost every real Terraform repo has and the rest of the
// fixture deliberately does not: a module Terraform must FETCH, which it
// installs into TF_DATA_DIR rather than reading from the source tree.
func remoteModuleStack(t *testing.T, root string) string {
	t.Helper()

	subrepo := filepath.Join(root, "remote", "subrepo")
	gitInFixture(t, subrepo, "init", "--quiet", "--initial-branch=main")
	commitFixtureRepo(t, subrepo, "subrepo")

	consumer := filepath.Join(root, "remote", "consumer")
	tmpl, err := os.ReadFile(filepath.Join(consumer, "main.tf.tmpl"))
	if err != nil {
		t.Fatalf("read consumer template: %v", err)
	}
	rendered := strings.ReplaceAll(string(tmpl), "__SUBREPO__", subrepo)
	if err := os.WriteFile(filepath.Join(consumer, "main.tf"), []byte(rendered), 0o644); err != nil {
		t.Fatalf("render consumer stack: %v", err)
	}
	return consumer
}

func gitInFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.name=Pack Test",
		"-c", "user.email=pack@caesium.test",
		"-c", "commit.gpgsign=false",
		"-c", "safe.directory=" + dir,
	}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func commitFixtureRepo(t *testing.T, dir, msg string) {
	t.Helper()
	gitInFixture(t, dir, "add", "-A")
	gitInFixture(t, dir, "commit", "-m", msg)
}

// TestFingerprintsAStackWithANonLocalModuleSource is the regression guard for
// the shape the rest of the fixture cannot reach.
//
// Terraform installs a git/registry/http module into TF_DATA_DIR and records
// the install path as the manifest Dir; because discover relocates TF_DATA_DIR
// to an absolute per-run scratch directory (so the source mount can stay
// read-only), that Dir is ABSOLUTE. Joining it onto the root module produced a
// path that does not exist, so discover exited 1 on every such stack.
func TestFingerprintsAStackWithANonLocalModuleSource(t *testing.T) {
	root := fixtureCopy(t)
	stack := remoteModuleStack(t, root)

	values := singleRootOutput(t, testConfig(t, stack))
	if !protocol.ValidDigest(values["fingerprint"]) {
		t.Fatalf("fingerprint = %q", values["fingerprint"])
	}
	// The fetched module contributes its own input digest, so a change to it is
	// visible to `caesium why` exactly like a local one.
	if !protocol.ValidDigest(values["input_remote"]) {
		t.Fatalf("input_remote = %q, want the fetched module's digest (keys %v)",
			values["input_remote"], protocol.SortedKeys(values))
	}
	for _, key := range []string{"input_root", "input_local", "input_local_inner"} {
		if !protocol.ValidDigest(values[key]) {
			t.Fatalf("%s = %q (keys %v)", key, values[key], protocol.SortedKeys(values))
		}
	}
}

// TestInstalledModuleFingerprintIsIndependentOfTheDataDirectory is the
// second-order half of the same defect: every run gets a fresh TF_DATA_DIR, so
// if the install path reached the digest the fingerprint would move on every
// single run and re-apply every stack forever (design §8, "fingerprint
// nondeterminism").
func TestInstalledModuleFingerprintIsIndependentOfTheDataDirectory(t *testing.T) {
	root := fixtureCopy(t)
	stack := remoteModuleStack(t, root)
	cfg := testConfig(t, stack)

	first := singleRootOutput(t, cfg)
	second := singleRootOutput(t, cfg)

	if first["fingerprint"] != second["fingerprint"] {
		t.Fatalf("two runs of the same tree produced different fingerprints (%q vs %q); "+
			"the per-run TF_DATA_DIR is leaking into the digest",
			first["fingerprint"], second["fingerprint"])
	}
	if first["input_remote"] != second["input_remote"] {
		t.Fatalf("the fetched module's input digest moved between runs: %q vs %q",
			first["input_remote"], second["input_remote"])
	}
}

// TestInstalledModuleFingerprintTracksItsContent proves the previous test is
// not passing because the fetched module is being ignored.
func TestInstalledModuleFingerprintTracksItsContent(t *testing.T) {
	root := fixtureCopy(t)
	stack := remoteModuleStack(t, root)
	cfg := testConfig(t, stack)

	before := singleRootOutput(t, cfg)

	subrepo := filepath.Join(root, "remote", "subrepo")
	main := filepath.Join(subrepo, "main.tf")
	body, err := os.ReadFile(main)
	if err != nil {
		t.Fatalf("read subrepo module: %v", err)
	}
	if err := os.WriteFile(main, append(body, []byte("\n# touched\n")...), 0o644); err != nil {
		t.Fatalf("edit subrepo module: %v", err)
	}
	commitFixtureRepo(t, subrepo, "touch")

	after := singleRootOutput(t, cfg)
	if after["input_remote"] == before["input_remote"] {
		t.Fatal("editing the fetched module did not move its input digest")
	}
	if after["fingerprint"] == before["fingerprint"] {
		t.Fatal("editing the fetched module did not move the stack fingerprint")
	}
}

// TestDiscoverLeavesTheSourceTreeUntouchedWithARemoteModule keeps the
// read-only-source property that motivated relocating TF_DATA_DIR in the first
// place — the fetched module must land in the scratch directory, not in the
// stack.
func TestDiscoverLeavesTheSourceTreeUntouchedWithARemoteModule(t *testing.T) {
	root := fixtureCopy(t)
	stack := remoteModuleStack(t, root)

	if _, err := runDiscover(t, testConfig(t, stack)); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == tf.DataDirName {
			rel, _ := filepath.Rel(root, path)
			t.Fatalf("discover wrote %s into the source tree", rel)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
}

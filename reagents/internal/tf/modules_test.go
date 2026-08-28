package tf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verifiedManifest is the exact document Terraform 1.15.9 writes for the
// hermetic fixture's network stack. It is reproduced verbatim because the whole
// reason this parser exists is that `terraform modules -json` loses the two
// fields shown here: the fully-qualified Key and the resolved Dir.
const verifiedManifest = `{"Modules":[` +
	`{"Key":"","Source":"","Dir":"."},` +
	`{"Key":"tags","Source":"../../modules/tags","Dir":"../../modules/tags"},` +
	`{"Key":"tags.inner","Source":"./inner","Dir":"../../modules/tags/inner"},` +
	`{"Key":"vpc","Source":"../../modules/vpc","Dir":"../../modules/vpc"}]}`

func TestParseManifestReadsTheVerifiedShape(t *testing.T) {
	m, err := ParseManifest([]byte(verifiedManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if len(m.Modules) != 4 {
		t.Fatalf("got %d modules, want 4", len(m.Modules))
	}

	byKey := map[string]ModuleEntry{}
	for _, entry := range m.Modules {
		byKey[entry.Key] = entry
	}
	inner, ok := byKey["tags.inner"]
	if !ok {
		t.Fatalf("the fully-qualified nested key is missing: %v", byKey)
	}
	if inner.Dir != "../../modules/tags/inner" {
		t.Fatalf("nested Dir = %q, want the resolved path", inner.Dir)
	}
	if inner.Source != "./inner" {
		t.Fatalf("nested Source = %q", inner.Source)
	}
}

func TestParseManifestAcceptsARegistryVersion(t *testing.T) {
	body := `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
		`{"Key":"vpc","Source":"terraform-aws-modules/vpc/aws","Version":"5.1.2","Dir":".terraform/modules/vpc"}]}`
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Modules[1].Version != "5.1.2" {
		t.Fatalf("Version = %q", m.Modules[1].Version)
	}
}

func TestParseManifestRejectsUnexpectedShapes(t *testing.T) {
	cases := map[string]string{
		"unknown field":    `{"Modules":[{"Key":"","Source":"","Dir":".","Surprise":1}]}`,
		"unknown top key":  `{"Modules":[{"Key":"","Source":"","Dir":"."}],"FormatVersion":"2"}`,
		"no modules":       `{"Modules":[]}`,
		"no root entry":    `{"Modules":[{"Key":"vpc","Source":"./vpc","Dir":"vpc"}]}`,
		"empty dir":        `{"Modules":[{"Key":"","Source":"","Dir":""}]}`,
		"duplicate key":    `{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"","Source":"","Dir":"x"}]}`,
		"not json":         `Modules: []`,
		"array not object": `[{"Key":"","Dir":"."}]`,
		"trailing content": `{"Modules":[{"Key":"","Source":"","Dir":"."}]} {"Modules":[]}`,
		"wrong dir type":   `{"Modules":[{"Key":"","Source":"","Dir":42}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(body)); err == nil {
				t.Fatalf("ParseManifest(%s) = nil error, want a hard failure", body)
			}
		})
	}
}

func TestReadManifestReportsAMissingFile(t *testing.T) {
	_, err := ReadManifest(t.TempDir())
	if err == nil {
		t.Fatal("a missing manifest was accepted")
	}
	if !strings.Contains(err.Error(), ManifestPath) {
		t.Fatalf("error should name the manifest path, got %v", err)
	}
}

func TestReadManifestParsesFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, filepath.FromSlash(ManifestPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(verifiedManifest), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	m, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(m.Modules) != 4 {
		t.Fatalf("got %d modules", len(m.Modules))
	}
}

func TestModuleName(t *testing.T) {
	cases := map[string]string{
		"":                 "root",
		"tags":             "tags",
		"tags.inner":       "tags_inner",
		"app-web.vpc":      "app_web_vpc",
		"a.b.c":            "a_b_c",
		"with space":       "with_space",
		"nested/call.deep": "nested_call_deep",
	}
	for key, want := range cases {
		if got := ModuleName(key); got != want {
			t.Fatalf("ModuleName(%q) = %q, want %q", key, got, want)
		}
	}
}

// installedManifest is the document Terraform 1.15.9 writes when TF_DATA_DIR is
// an absolute path and the root module declares a git module alongside a local
// one. Captured by running `terraform get` under the pinned CLI — the absolute
// Dir on the fetched module is the whole point.
const installedManifest = `{"Modules":[` +
	`{"Key":"","Source":"","Dir":"."},` +
	`{"Key":"local","Source":"./localmod","Dir":"localmod"},` +
	`{"Key":"remote","Source":"git::file:///tmp/sub","Dir":"/tmp/absdata/modules/remote"}]}`

func TestResolveSeparatesTheReadPathFromTheDigestedIdentity(t *testing.T) {
	m, err := ParseManifest([]byte(installedManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	resolved, err := m.Resolve("/src/stacks/network", "/tmp/absdata")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved) != 3 {
		t.Fatalf("got %d modules, want 3", len(resolved))
	}

	byKey := map[string]ResolvedModule{}
	for _, r := range resolved {
		byKey[r.Key] = r
	}

	// The fetched module is read from where Terraform actually put it...
	remote := byKey["remote"]
	if remote.Dir != filepath.Clean("/tmp/absdata/modules/remote") {
		t.Fatalf("remote.Dir = %q, want the install path", remote.Dir)
	}
	if !remote.Installed {
		t.Fatal("remote should be recognised as installed into the data directory")
	}
	// ...but the per-run install path must not be what identifies it.
	if remote.Identity != "git::file:///tmp/sub" {
		t.Fatalf("remote.Identity = %q, want the declared source", remote.Identity)
	}
	if strings.Contains(remote.Identity, "absdata") {
		t.Fatalf("the data directory leaked into the identity: %q", remote.Identity)
	}

	local := byKey["local"]
	if local.Installed {
		t.Fatal("a local module must not be marked installed")
	}
	if local.Dir != filepath.Clean("/src/stacks/network/localmod") {
		t.Fatalf("local.Dir = %q", local.Dir)
	}
	if local.Identity != "localmod" {
		t.Fatalf("local.Identity = %q, want the relative directory", local.Identity)
	}

	root := byKey[""]
	if root.Dir != filepath.Clean("/src/stacks/network") {
		t.Fatalf("root.Dir = %q", root.Dir)
	}
	if root.Identity != "." {
		t.Fatalf("root.Identity = %q", root.Identity)
	}
}

func TestResolveIsIndependentOfTheDataDirectoryPath(t *testing.T) {
	identityFor := func(dataDir string) string {
		t.Helper()
		body := `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
			`{"Key":"remote","Source":"git::file:///tmp/sub","Dir":"` + dataDir + `/modules/remote"}]}`
		m, err := ParseManifest([]byte(body))
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		resolved, err := m.Resolve("/src/stacks/network", dataDir)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		for _, r := range resolved {
			if r.Key == "remote" {
				return r.Identity
			}
		}
		t.Fatal("remote module missing")
		return ""
	}
	// Two runs, two scratch directories: the identity that enters the digest
	// must be the same, or every run re-fingerprints every stack (design §8).
	if a, b := identityFor("/tmp/tf-discover-data-111"), identityFor("/tmp/tf-discover-data-222"); a != b {
		t.Fatalf("identity depends on the data directory: %q vs %q", a, b)
	}
}

func TestResolveHandlesTheDefaultRelativeDataDirectory(t *testing.T) {
	// With no TF_DATA_DIR, Terraform records `.terraform/modules/<key>` —
	// relative to the root module, but still an install path.
	body := `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
		`{"Key":"remote","Source":"git::file:///tmp/sub","Dir":".terraform/modules/remote"}]}`
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	resolved, err := m.Resolve("/src/stacks/network", "/src/stacks/network/.terraform")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, r := range resolved {
		if r.Key != "remote" {
			continue
		}
		if !r.Installed {
			t.Fatal("a module under the default data directory is still installed")
		}
		if r.Identity != "git::file:///tmp/sub" {
			t.Fatalf("Identity = %q", r.Identity)
		}
		if r.Dir != filepath.Clean("/src/stacks/network/.terraform/modules/remote") {
			t.Fatalf("Dir = %q", r.Dir)
		}
		return
	}
	t.Fatal("remote module missing")
}

func TestResolveIncludesTheRegistryVersionInTheIdentity(t *testing.T) {
	body := `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
		`{"Key":"vpc","Source":"terraform-aws-modules/vpc/aws","Version":"5.1.2","Dir":"/tmp/d/modules/vpc"}]}`
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	resolved, err := m.Resolve("/src", "/tmp/d")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, r := range resolved {
		if r.Key == "vpc" {
			// Two versions of the same registry module are different inputs
			// even when their content happens to coincide.
			if r.Identity != "terraform-aws-modules/vpc/aws@5.1.2" {
				t.Fatalf("Identity = %q", r.Identity)
			}
			return
		}
	}
	t.Fatal("vpc module missing")
}

func TestResolveFailsClosedOnAnAbsoluteLocalModule(t *testing.T) {
	// An absolute Dir outside the data directory would put a machine-specific
	// path into the fingerprint, so two workers would disagree.
	body := `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
		`{"Key":"weird","Source":"/elsewhere/mod","Dir":"/elsewhere/mod"}]}`
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if _, err := m.Resolve("/src", "/tmp/d"); err == nil {
		t.Fatal("an absolute module directory outside the data dir was accepted")
	}
}

func TestResolveFailsClosedOnAnInstalledModuleWithNoSource(t *testing.T) {
	body := `{"Modules":[{"Key":"","Source":"","Dir":"."},` +
		`{"Key":"remote","Source":"","Dir":"/tmp/d/modules/remote"}]}`
	m, err := ParseManifest([]byte(body))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if _, err := m.Resolve("/src", "/tmp/d"); err == nil {
		t.Fatal("an installed module with no source was accepted; its identity would be the install path")
	}
}

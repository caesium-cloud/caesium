package tf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nullLock = `# This file is maintained automatically by "terraform init".
# Manual edits may be lost in future updates.

provider "registry.terraform.io/hashicorp/null" {
  version     = "3.3.1"
  constraints = "~> 3.2"
  hashes = [
    "h1:4pjRixNj9/nijyC0jrCr8tYOpZ8afFwZ2M86y81PMa0=",
    "zh:08c59776542ea16e5a8545752787b17ff412922182b4cfabe16139197be8ac44",
  ]
}
`

const twoProviderLock = nullLock + `
provider "registry.terraform.io/hashicorp/random" {
  version     = "3.9.0"
  constraints = "~> 3.6"
  hashes = [
    "h1:UlBuNVuCGJ39tTv2c5gz2NRZnQbXfbIWbTzWcth5o74=",
  ]
}
`

func TestParseLockFileReadsProviderVersionAndHashes(t *testing.T) {
	providers, err := ParseLockFile([]byte(twoProviderLock))
	if err != nil {
		t.Fatalf("ParseLockFile: %v", err)
	}
	if len(providers) != 2 {
		t.Fatalf("want 2 providers, got %d (%+v)", len(providers), providers)
	}
	if providers[0].Source != "registry.terraform.io/hashicorp/null" || providers[0].Version != "3.3.1" {
		t.Fatalf("first provider = %+v", providers[0])
	}
	if len(providers[0].Hashes) != 2 {
		t.Fatalf("want 2 hashes, got %v", providers[0].Hashes)
	}
	if providers[0].Namespace() != "hashicorp" || providers[0].Type() != "null" {
		t.Fatalf("namespace/type = %q/%q", providers[0].Namespace(), providers[0].Type())
	}
	if providers[1].Version != "3.9.0" {
		t.Fatalf("second provider = %+v", providers[1])
	}
}

// The lock file is an HCL document the reagents parse by hand. A shape it does not
// recognise must be an error naming the line, never a silently dropped entry:
// a dropped provider yields a mirror key that does not cover it, so the warm
// step would exit fast on a marker for a mirror missing that provider and every
// downstream init would fail offline.
func TestParseLockFileFailsClosedOnUnrecognisedShapes(t *testing.T) {
	cases := map[string]string{
		"unknown attribute": `provider "registry.terraform.io/hashicorp/null" {
  version = "3.3.1"
  surprise = "yes"
  hashes = []
}
`,
		"no version": `provider "registry.terraform.io/hashicorp/null" {
  constraints = "~> 3.2"
}
`,
		"duplicate provider": nullLock + nullLock,
		"unterminated block": `provider "registry.terraform.io/hashicorp/null" {
  version = "3.3.1"
`,
		"unterminated hash list": `provider "registry.terraform.io/hashicorp/null" {
  version = "3.3.1"
  hashes = [
    "h1:abc",
`,
		"not a provider block": `resource "null_resource" "x" {
}
`,
		"empty file":            ``,
		"bad source address":    "provider \"HASHICORP/../null\" {\n  version = \"1.0.0\"\n}\n",
		"bad version":           "provider \"hashicorp/null\" {\n  version = \"$(id)\"\n}\n",
		"bad hash":              "provider \"hashicorp/null\" {\n  version = \"1.0.0\"\n  hashes = [\"not a hash\"]\n}\n",
		"escaped string":        "provider \"hashicorp/null\" {\n  version = \"1.0\\\"0\"\n}\n",
		"unquoted version":      "provider \"hashicorp/null\" {\n  version = 3.3.1\n}\n",
		"attribute outside any": "version = \"1.0.0\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseLockFile([]byte(body)); err == nil {
				t.Fatalf("ParseLockFile accepted %s", name)
			}
		})
	}
}

func TestParseLockFileAcceptsTheInlineHashListForm(t *testing.T) {
	providers, err := ParseLockFile([]byte("provider \"hashicorp/null\" {\n  version = \"3.3.1\"\n  hashes = [\"h1:abc=\", \"zh:def\"]\n}\n"))
	if err != nil {
		t.Fatalf("ParseLockFile: %v", err)
	}
	if got := providers[0].Hashes; len(got) != 2 || got[0] != "h1:abc=" {
		t.Fatalf("hashes = %v", got)
	}
}

func TestParseLockFileIsIndependentOfProviderOrder(t *testing.T) {
	reversed := "provider \"registry.terraform.io/hashicorp/random\" {\n  version = \"3.9.0\"\n  hashes = [\"h1:UlBuNVuCGJ39tTv2c5gz2NRZnQbXfbIWbTzWcth5o74=\"]\n}\n" + nullLock
	a, err := ParseLockFile([]byte(twoProviderLock))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseLockFile([]byte(reversed))
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Source != b[0].Source || a[1].Source != b[1].Source {
		t.Fatalf("provider order is not canonical: %v vs %v", a, b)
	}
}

// The whole point of the key is that it moves with the provider set and with
// nothing else. If it moved with traversal order every run would re-mirror; if
// it did not move with a version bump the marker would vouch for a mirror that
// does not contain the new package.
func TestMirrorKeyTracksTheProviderSetAndNotTheOrder(t *testing.T) {
	base := []LockedProvider{
		{Source: "registry.terraform.io/hashicorp/null", Version: "3.3.1", Hashes: []string{"h1:a", "zh:b"}},
		{Source: "registry.terraform.io/hashicorp/random", Version: "3.9.0", Hashes: []string{"h1:c"}},
	}
	reversed := []LockedProvider{base[1], base[0]}
	if MirrorKey(base, []string{"linux_amd64"}) != MirrorKey(reversed, []string{"linux_amd64"}) {
		t.Fatal("mirror key depends on the order providers were discovered in")
	}

	shuffledHashes := []LockedProvider{
		{Source: base[0].Source, Version: base[0].Version, Hashes: []string{"zh:b", "h1:a"}},
		base[1],
	}
	if MirrorKey(base, []string{"linux_amd64"}) != MirrorKey(shuffledHashes, []string{"linux_amd64"}) {
		t.Fatal("mirror key depends on hash order")
	}

	bumped := []LockedProvider{{Source: base[0].Source, Version: "3.3.2", Hashes: base[0].Hashes}, base[1]}
	if MirrorKey(base, []string{"linux_amd64"}) == MirrorKey(bumped, []string{"linux_amd64"}) {
		t.Fatal("mirror key did not move with a provider version bump")
	}

	rehashed := []LockedProvider{{Source: base[0].Source, Version: base[0].Version, Hashes: []string{"h1:different"}}, base[1]}
	if MirrorKey(base, []string{"linux_amd64"}) == MirrorKey(rehashed, []string{"linux_amd64"}) {
		t.Fatal("mirror key did not move with a changed package checksum")
	}

	// A mirror populated for one platform is useless to a runner on another, so
	// the marker must not claim otherwise.
	if MirrorKey(base, []string{"linux_amd64"}) == MirrorKey(base, []string{"linux_arm64"}) {
		t.Fatal("mirror key does not cover the target platform")
	}
	if MirrorKey(base, []string{"linux_amd64", "linux_arm64"}) != MirrorKey(base, []string{"linux_arm64", "linux_amd64"}) {
		t.Fatal("mirror key depends on the order platforms were listed in")
	}
}

func TestMergeLockedUnionsHashesAndKeepsBothVersions(t *testing.T) {
	merged := MergeLocked(
		[]LockedProvider{{Source: "hashicorp/null", Version: "3.3.1", Hashes: []string{"h1:a"}}},
		[]LockedProvider{{Source: "hashicorp/null", Version: "3.3.1", Hashes: []string{"zh:b"}}},
		[]LockedProvider{{Source: "hashicorp/null", Version: "3.2.0", Hashes: []string{"h1:c"}}},
	)
	if len(merged) != 2 {
		t.Fatalf("want two entries (one per version), got %+v", merged)
	}
	if merged[0].Version != "3.2.0" || merged[1].Version != "3.3.1" {
		t.Fatalf("entries are not version-ordered: %+v", merged)
	}
	if got := strings.Join(merged[1].Hashes, ","); got != "h1:a,zh:b" {
		t.Fatalf("hash union = %q", got)
	}
}

func TestRequiredProvidersHCLPinsTheExactLockedVersion(t *testing.T) {
	hcl, err := RequiredProvidersHCL([]LockedProvider{
		{Source: "registry.terraform.io/hashicorp/null", Version: "3.3.1"},
		{Source: "registry.terraform.io/hashicorp/random", Version: "3.9.0"},
	})
	if err != nil {
		t.Fatalf("RequiredProvidersHCL: %v", err)
	}
	for _, want := range []string{
		`null = {`,
		`source  = "registry.terraform.io/hashicorp/null"`,
		`version = "= 3.3.1"`,
		`random = {`,
		`version = "= 3.9.0"`,
	} {
		if !strings.Contains(hcl, want) {
			t.Fatalf("generated HCL is missing %q:\n%s", want, hcl)
		}
	}
}

func TestRequiredProvidersHCLDisambiguatesCollidingLocalNames(t *testing.T) {
	hcl, err := RequiredProvidersHCL([]LockedProvider{
		{Source: "registry.terraform.io/hashicorp/null", Version: "3.3.1"},
		{Source: "registry.terraform.io/acme/null", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("RequiredProvidersHCL: %v", err)
	}
	for _, want := range []string{"hashicorp_null = {", "acme_null = {"} {
		if !strings.Contains(hcl, want) {
			t.Fatalf("generated HCL is missing %q:\n%s", want, hcl)
		}
	}
}

// The source address and version are interpolated into generated HCL that
// Terraform then evaluates, so a lock file is an injection surface. Both the
// parser and the generator reject anything outside the address grammar.
func TestRequiredProvidersHCLRejectsAnAddressItWouldInterpolate(t *testing.T) {
	for _, p := range []LockedProvider{
		{Source: "hashicorp/null\"\n}\nresource \"x\"", Version: "1.0.0"},
		{Source: "hashicorp/null", Version: "1.0.0\"\n}\n#"},
	} {
		if _, err := RequiredProvidersHCL([]LockedProvider{p}); err == nil {
			t.Fatalf("RequiredProvidersHCL accepted %+v", p)
		}
	}
	if _, err := RequiredProvidersHCL(nil); err == nil {
		t.Fatal("RequiredProvidersHCL accepted an empty provider set")
	}
}

// `direct` must be excluded, not merely unmentioned: leaving Terraform's
// default installation methods in place means a provider missing from the
// mirror reaches the public registry instead of failing the init, which is the
// difference between a hermetic run and one that only looks hermetic.
func TestTerraformRCExcludesDirectInstallation(t *testing.T) {
	rc := TerraformRC("/cache/providers/abc123")
	for _, want := range []string{
		"provider_installation {",
		"filesystem_mirror {",
		`path    = "/cache/providers/abc123"`,
		"direct {",
		`exclude = ["*/*/*"]`,
	} {
		if !strings.Contains(rc, want) {
			t.Fatalf("terraformrc is missing %q:\n%s", want, rc)
		}
	}
}

func TestFindLockFilesWalksStacksAndSkipsWorkingDirectories(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"stacks/network/" + LockFileName,
		"stacks/app-web/" + LockFileName,
		"modules/vpc/main.tf",
		".git/" + LockFileName,
		"stacks/network/" + DataDirName + "/" + LockFileName,
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(nullLock), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	found, err := FindLockFiles(root)
	if err != nil {
		t.Fatalf("FindLockFiles: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("want the two stack lock files, got %v", found)
	}
	for _, path := range found {
		dir := filepath.Dir(path)
		if strings.Contains(dir, ".git") || strings.Contains(dir, DataDirName) {
			t.Fatalf("FindLockFiles descended into a working directory: %s", path)
		}
	}
	if !strings.HasSuffix(found[0], filepath.Join("app-web", LockFileName)) {
		t.Fatalf("results are not sorted: %v", found)
	}
}

func TestReadLockFileNamesThePathInItsError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, LockFileName)
	if err := os.WriteFile(path, []byte("nonsense\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadLockFile(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error should name the file, got %v", err)
	}
}

// The committed fixture is what every infra scenario mirrors from; a parser
// that cannot read it would only surface as a lane failure.
func TestParsesTheCommittedFixtureLockFiles(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "infra")
	found, err := FindLockFiles(root)
	if err != nil {
		t.Fatalf("FindLockFiles: %v", err)
	}
	if len(found) == 0 {
		t.Fatalf("the fixture has no %s", LockFileName)
	}
	sets := make([][]LockedProvider, 0, len(found))
	for _, path := range found {
		providers, err := ReadLockFile(path)
		if err != nil {
			t.Fatalf("ReadLockFile(%s): %v", path, err)
		}
		sets = append(sets, providers)
	}
	merged := MergeLocked(sets...)
	if len(merged) == 0 {
		t.Fatal("the fixture's lock files resolved to no providers")
	}
	if _, err := RequiredProvidersHCL(merged); err != nil {
		t.Fatalf("the fixture's provider set does not render: %v", err)
	}
}

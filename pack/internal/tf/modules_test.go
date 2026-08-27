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

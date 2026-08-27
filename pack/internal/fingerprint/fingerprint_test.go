package fingerprint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestDigestDirIsStableAndPathIndependent(t *testing.T) {
	files := map[string]string{
		"main.tf":             "resource \"null_resource\" \"a\" {}\n",
		"variables.tf":        "variable \"x\" { type = string }\n",
		".terraform.lock.hcl": "# lock\n",
	}
	a := writeDir(t, files)
	b := writeDir(t, files)

	first, err := DigestDir(a)
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	if !strings.HasPrefix(first, DigestPrefix) {
		t.Fatalf("digest = %q", first)
	}

	again, err := DigestDir(a)
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	if again != first {
		t.Fatal("DigestDir is not deterministic for the same directory")
	}

	other, err := DigestDir(b)
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	if other != first {
		t.Fatal("DigestDir depends on the directory's absolute path")
	}
}

func TestDigestDirCoversEveryConfigGlob(t *testing.T) {
	base := map[string]string{"main.tf": "locals {}\n"}
	baseline, err := DigestDir(writeDir(t, base))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}

	for _, name := range []string{
		"extra.tf", "extra.tf.json", "terraform.tfvars",
		"extra.auto.tfvars.json", "probe.tfquery.hcl", ".terraform.lock.hcl",
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"main.tf": "locals {}\n", name: "x\n"}
			got, err := DigestDir(writeDir(t, files))
			if err != nil {
				t.Fatalf("DigestDir: %v", err)
			}
			if got == baseline {
				t.Fatalf("adding %s did not move the digest", name)
			}
		})
	}
}

func TestDigestDirIgnoresNonConfigFilesAndSubdirectories(t *testing.T) {
	base := map[string]string{"main.tf": "locals {}\n"}
	baseline, err := DigestDir(writeDir(t, base))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}

	// A nested directory is its own manifest entry with its own digest;
	// recursing here would double-count it.
	extended := map[string]string{
		"main.tf":           "locals {}\n",
		"README.md":         "docs\n",
		"terraform.tfstate": "{}\n",
		"inner/main.tf":     "locals {}\n",
	}
	got, err := DigestDir(writeDir(t, extended))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	if got != baseline {
		t.Fatal("a non-config file or a subdirectory changed the digest")
	}
}

func TestDigestDirTracksRenames(t *testing.T) {
	before, err := DigestDir(writeDir(t, map[string]string{"a.tf": "locals {}\n"}))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	after, err := DigestDir(writeDir(t, map[string]string{"b.tf": "locals {}\n"}))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	if before == after {
		t.Fatal("renaming a file with identical content did not move the digest")
	}
}

func TestDigestDirFailsClosedOnAnEmptyModule(t *testing.T) {
	if _, err := DigestDir(t.TempDir()); err == nil {
		t.Fatal("a directory with no Terraform configuration was digested anyway")
	}
	if _, err := DigestDir(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing directory was digested anyway")
	}
}

func TestCombineIsOrderIndependentAndCoversEveryField(t *testing.T) {
	a := Input{Name: "root", Path: "./", Digest: DigestPrefix + "aa"}
	b := Input{Name: "tags", Path: "../../modules/tags", Digest: DigestPrefix + "bb"}

	if Combine([]Input{a, b}) != Combine([]Input{b, a}) {
		t.Fatal("Combine depends on input order")
	}

	base := Combine([]Input{a, b}, "workspace=default")
	if base == Combine([]Input{a, b}, "workspace=staging") {
		t.Fatal("an extra fact did not change the fingerprint")
	}

	moved := b
	moved.Digest = DigestPrefix + "cc"
	if base == Combine([]Input{a, moved}, "workspace=default") {
		t.Fatal("a changed input digest did not change the fingerprint")
	}

	relocated := b
	relocated.Path = "../../shared/tags"
	if base == Combine([]Input{a, relocated}, "workspace=default") {
		t.Fatal("a relocated module did not change the fingerprint")
	}

	renamed := b
	renamed.Name = "tags2"
	if base == Combine([]Input{a, renamed}, "workspace=default") {
		t.Fatal("a renamed module call did not change the fingerprint")
	}
}

func TestCombineNormalizesEquivalentPaths(t *testing.T) {
	a := Input{Name: "tags", Path: "../../modules/tags", Digest: DigestPrefix + "aa"}
	b := Input{Name: "tags", Path: "../../modules/./tags/", Digest: DigestPrefix + "aa"}
	if Combine([]Input{a}) != Combine([]Input{b}) {
		t.Fatal("two spellings of the same relative path produced different fingerprints")
	}
}

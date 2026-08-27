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

func mustCombine(t *testing.T, inputs []Input, extras ...string) string {
	t.Helper()
	got, err := Combine(inputs, extras...)
	if err != nil {
		t.Fatalf("Combine: %v", err)
	}
	return got
}

func TestCombineIsOrderIndependentAndCoversEveryField(t *testing.T) {
	a := Input{Name: "root", Identity: ".", Digest: DigestPrefix + "aa"}
	b := Input{Name: "tags", Identity: "../../modules/tags", Digest: DigestPrefix + "bb"}

	if mustCombine(t, []Input{a, b}) != mustCombine(t, []Input{b, a}) {
		t.Fatal("Combine depends on input order")
	}

	base := mustCombine(t, []Input{a, b}, "workspace=default")
	if base == mustCombine(t, []Input{a, b}, "workspace=staging") {
		t.Fatal("an extra fact did not change the fingerprint")
	}

	moved := b
	moved.Digest = DigestPrefix + "cc"
	if base == mustCombine(t, []Input{a, moved}, "workspace=default") {
		t.Fatal("a changed input digest did not change the fingerprint")
	}

	relocated := b
	relocated.Identity = "../../shared/tags"
	if base == mustCombine(t, []Input{a, relocated}, "workspace=default") {
		t.Fatal("a relocated module did not change the fingerprint")
	}

	renamed := b
	renamed.Name = "tags2"
	if base == mustCombine(t, []Input{a, renamed}, "workspace=default") {
		t.Fatal("a renamed module call did not change the fingerprint")
	}
}

// TestCombineRejectsCollidingNames guards the ordering hazard directly: module
// names are a lossy projection of module keys ("a-b" and "a.b" both reduce to
// "a_b"), and a set with two equal names has no defined order, so the digest
// could differ between two runs over the same tree. Rejecting is the only
// answer that keeps the fingerprint well defined — and the caller would also
// have silently dropped one of the two per-input output rows.
func TestCombineRejectsCollidingNames(t *testing.T) {
	a := Input{Name: "a_b", Identity: "modules/a-b", Digest: DigestPrefix + "aa"}
	b := Input{Name: "a_b", Identity: "modules/a/b", Digest: DigestPrefix + "bb"}

	_, err := Combine([]Input{a, b})
	if err == nil {
		t.Fatal("two inputs reducing to the same name were accepted")
	}
	if !strings.Contains(err.Error(), "a_b") {
		t.Fatalf("error should name the collision, got %v", err)
	}
	// Both spellings must be named so the operator knows which calls to rename.
	for _, want := range []string{"modules/a-b", "modules/a/b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q, got %v", want, err)
		}
	}

	// The same rejection regardless of the order they arrive in.
	if _, err := Combine([]Input{b, a}); err == nil {
		t.Fatal("the collision was accepted in the reverse order")
	}
}

// TestCombineIsDeterministicForNearCollisions is the property the non-stable
// sort put at risk: a set must fold to the same digest whatever order it
// arrives in.
func TestCombineIsDeterministicForNearCollisions(t *testing.T) {
	inputs := []Input{
		{Name: "tags", Identity: "modules/tags", Digest: DigestPrefix + "aa"},
		{Name: "vpc", Identity: "modules/vpc", Digest: DigestPrefix + "bb"},
		{Name: "root", Identity: ".", Digest: DigestPrefix + "cc"},
	}
	want := mustCombine(t, inputs)
	for range 25 {
		shuffled := []Input{inputs[2], inputs[0], inputs[1]}
		if got := mustCombine(t, shuffled); got != want {
			t.Fatalf("fingerprint is not order-stable: %q vs %q", got, want)
		}
	}
}

// TestCombineTakesIdentityVerbatim documents that normalization is the
// caller's job: an identity may be a relative path OR a module source string,
// and path-cleaning the latter would mangle its scheme separators.
func TestCombineTakesIdentityVerbatim(t *testing.T) {
	a := Input{Name: "m", Identity: "git::https://example.test/m.git", Digest: DigestPrefix + "aa"}
	b := Input{Name: "m", Identity: "git:/https:/example.test/m.git", Digest: DigestPrefix + "aa"}
	if mustCombine(t, []Input{a}) == mustCombine(t, []Input{b}) {
		t.Fatal("Combine collapsed two distinct source strings")
	}
}

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

// A module's behaviour is decided by far more than its *.tf files. Hashing only
// the configuration extensions let a templatefile template, a policy document
// or a cloud-init script change without moving the stack's fingerprint — and
// because plan steps key on `chain: values`, plan and apply then cache-hit and
// the run goes green having deployed none of the edit.
func TestDigestDirCoversEveryFileTheModuleOwns(t *testing.T) {
	base := map[string]string{"main.tf": "locals {}\n"}
	baseline, err := DigestDir(writeDir(t, base))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}

	for name, body := range map[string]string{
		// The motivating case: templatefile("${path.module}/templates/…").
		"templates/userdata.tftpl": "#!/bin/sh\necho ${name}\n",
		"policies/bucket.json":     `{"Version":"2012-10-17"}`,
		"scripts/bootstrap.sh":     "#!/bin/sh\nexit 0\n",
		"files/nested/deep/asset":  "payload\n",
		"README.md":                "docs\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"main.tf": "locals {}\n", name: body}
			got, err := DigestDir(writeDir(t, files))
			if err != nil {
				t.Fatalf("DigestDir: %v", err)
			}
			if got == baseline {
				t.Fatalf("adding %s did not move the digest", name)
			}
		})
	}

	// Editing one, rather than adding it, is the shape a real regression takes.
	dir := writeDir(t, map[string]string{
		"main.tf":                  "locals {}\n",
		"templates/userdata.tftpl": "#!/bin/sh\necho ${name}\n",
	})
	before, err := DigestDir(dir)
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	template := filepath.Join(dir, "templates", "userdata.tftpl")
	if err := os.WriteFile(template, []byte("#!/bin/sh\necho ${name} > /etc/motd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := DigestDir(dir)
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	if after == before {
		t.Fatal("editing a template the configuration reads did not move the digest")
	}
}

// The exclusions are the other half: they cover what Terraform and Caesium
// GENERATE, which moves on its own. Digesting state in particular would move
// every stack's fingerprint on every deploy and re-apply everything forever.
func TestDigestDirIgnoresGeneratedTerraformData(t *testing.T) {
	base := map[string]string{"main.tf": "locals {}\n"}
	baseline, err := DigestDir(writeDir(t, base))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}

	generated := map[string]string{
		"main.tf": "locals {}\n",
		// State: written by the apply this fingerprint gates.
		"terraform.tfstate":               `{"serial":1}`,
		"terraform.tfstate.backup":        `{"serial":0}`,
		"other.tfstate":                   `{"serial":9}`,
		".terraform.tfstate.lock.info":    `{"ID":"x"}`,
		".terraform/modules/modules.json": `{"Modules":[]}`,
		".terraform/providers/registry/x": "binary\n",
		// The default ARTIFACT_DIR: saved plans and apply receipts.
		".caesium/tf.plan":     "plan bytes\n",
		".caesium/applied.abc": "receipt\n",
		// The checkout's own metadata moves on every commit.
		".git/HEAD":                "ref: refs/heads/main\n",
		".git/objects/ab/cdef1234": "object\n",
	}
	got, err := DigestDir(writeDir(t, generated))
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}
	if got != baseline {
		t.Fatal("generated Terraform, state or Caesium data changed the digest; every stack would re-apply forever")
	}
}

// A relocated ARTIFACT_DIR inside the source tree is the one exclusion the
// package cannot infer from a name, so the caller passes it.
func TestDigestDirExcludesARelocatedArtifactDir(t *testing.T) {
	dir := writeDir(t, map[string]string{
		"main.tf":                  "locals {}\n",
		"build/artifacts/tf.plan":  "plan bytes\n",
		"templates/userdata.tftpl": "echo hi\n",
	})
	artifacts := filepath.Join(dir, "build", "artifacts")

	baseline, err := DigestDir(dir, artifacts)
	if err != nil {
		t.Fatalf("DigestDir: %v", err)
	}

	// A new plan artifact in there must not move the stack that produced it.
	if err := os.WriteFile(filepath.Join(artifacts, "tf.plan"), []byte("a different plan\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := DigestDir(dir, artifacts); err != nil {
		t.Fatalf("DigestDir: %v", err)
	} else if got != baseline {
		t.Fatal("a plan artifact under the excluded ARTIFACT_DIR moved the stack's own fingerprint")
	}

	// Without the exclusion it is just another module-owned file, which is what
	// makes the exclusion load-bearing rather than decorative.
	if got, err := DigestDir(dir); err != nil {
		t.Fatalf("DigestDir: %v", err)
	} else if got == baseline {
		t.Fatal("the excluded path was being skipped anyway; the exclusion proves nothing")
	}

	// And an ordinary asset next door is still covered.
	if err := os.WriteFile(filepath.Join(dir, "templates", "userdata.tftpl"), []byte("echo bye\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := DigestDir(dir, artifacts); err != nil {
		t.Fatalf("DigestDir: %v", err)
	} else if got == baseline {
		t.Fatal("the exclusion swallowed a file the module owns")
	}
}

// A symlink is recorded by its target rather than followed: following would
// either escape the module or digest the same bytes twice, and ignoring it
// would let a re-target change what Terraform reads invisibly.
func TestDigestDirTracksSymlinkTargets(t *testing.T) {
	build := func(target string) string {
		dir := writeDir(t, map[string]string{
			"main.tf": "locals {}\n",
			"files/a": "first\n",
			"files/b": "second\n",
		})
		if err := os.Symlink(target, filepath.Join(dir, "current")); err != nil {
			t.Skipf("symlinks are unavailable here: %v", err)
		}
		got, err := DigestDir(dir)
		if err != nil {
			t.Fatalf("DigestDir: %v", err)
		}
		return got
	}
	if build("files/a") == build("files/b") {
		t.Fatal("re-pointing a symlink did not move the digest")
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

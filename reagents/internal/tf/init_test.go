package tf

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// staleLockFile records a provider the offline fixture does not require, so
// `terraform init` wants to rewrite .terraform.lock.hcl to prune it. That is
// the cheapest hermetic stand-in for the real hazard (a lock file that drifted
// out of sync with the stack's provider requirements) and it needs no network,
// no mirror and no provider download.
const staleLockFile = `provider "registry.terraform.io/hashicorp/null" {
  version = "3.3.1"
  hashes = [
    "h1:4pjRixNj9/nijyC0jrCr8tYOpZ8afFwZ2M86y81PMa0=",
  ]
}
`

func terraformPath(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("TF_CLI_PATH"); path != "" {
		return path
	}
	return "/usr/local/bin/terraform"
}

// offlineStack copies reagents/testdata/offline/stack (provider-free by
// construction) into a scratch directory so a test can run REAL terraform
// init against it with no network.
func offlineStack(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(terraformPath(t)); err != nil {
		t.Skipf("terraform is not on PATH: %v", err)
	}

	root := filepath.Join(t.TempDir(), "stack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join("..", "..", "testdata", "offline", "stack")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, entry.Name())) //nolint:gosec // fixture path.
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TF_DATA_DIR", filepath.Join(t.TempDir(), "tfdata"))
	return root
}

// TestInitRefusesToRewriteADriftedLockFile is the whole point of
// -lockfile=readonly: the reference manifests mount `src` read-only for
// plan/apply/drift, and a lock file Terraform wants to update would otherwise
// be a silent write attempt into that mount. The lock file must come back
// byte-identical and the error must name the lock file, not a filesystem
// permission.
func TestInitRefusesToRewriteADriftedLockFile(t *testing.T) {
	root := offlineStack(t)
	lock := filepath.Join(root, LockFileName)
	if err := os.WriteFile(lock, []byte(staleLockFile), 0o600); err != nil {
		t.Fatal(err)
	}

	runner, err := NewRunner(root, terraformPath(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	err = runner.Init(context.Background(), nil)
	if err == nil {
		t.Fatal("expected terraform init to fail on a lock file it wants to rewrite")
	}
	// Terraform hard-wraps its diagnostics, so match on fragments that cannot
	// straddle a line break.
	for _, want := range []string{"Provider dependency changes detected", lockfileReadonlyArg} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the failure does not mention %q, so an operator cannot tell it apart from a mount problem: %v", want, err)
		}
	}

	after, readErr := os.ReadFile(lock) //nolint:gosec // fixture path.
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != staleLockFile {
		t.Fatalf("the lock file was rewritten despite -lockfile=readonly:\n%s", after)
	}
}

// TestInitSucceedsWhenTheLockFileIsInSync guards against the flag being so
// strict it breaks the steady state the pipeline actually runs in: the offline
// fixture requires no providers at all, so there is nothing for Terraform to
// record and init must still succeed.
func TestInitSucceedsWhenTheLockFileIsInSync(t *testing.T) {
	root := offlineStack(t)

	runner, err := NewRunner(root, terraformPath(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Init(context.Background(), nil); err != nil {
		t.Fatalf("init against an in-sync stack failed: %v", err)
	}
}

// TestInitRestoresTheProcessEnvironment proves the flag is scoped to the one
// Init call. tf-runner sets TF_VAR_* on this same process for cross-stack
// wiring (ExportVariable), so an Init that permanently mutated the environment
// would be leaking state into every later phase.
func TestInitRestoresTheProcessEnvironment(t *testing.T) {
	root := offlineStack(t)
	t.Setenv(initArgsEnvVar, "-no-color")

	runner, err := NewRunner(root, terraformPath(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Init(context.Background(), nil); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	if got := os.Getenv(initArgsEnvVar); got != "-no-color" {
		t.Fatalf("%s = %q after Init, want it restored to %q", initArgsEnvVar, got, "-no-color")
	}
}

// TestInitRestoresAnUnsetProcessEnvironment covers the other branch: the
// variable was absent before Init and must be absent after, not left as an
// empty string (which Terraform would still parse as an argument list).
func TestInitRestoresAnUnsetProcessEnvironment(t *testing.T) {
	root := offlineStack(t)
	// t.Setenv registers the restore; Unsetenv makes the pre-state "absent".
	t.Setenv(initArgsEnvVar, "")
	if err := os.Unsetenv(initArgsEnvVar); err != nil {
		t.Fatal(err)
	}

	runner, err := NewRunner(root, terraformPath(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Init(context.Background(), nil); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	if _, ok := os.LookupEnv(initArgsEnvVar); ok {
		t.Fatalf("%s is still set after Init", initArgsEnvVar)
	}
}

// TestInitPlansTheModuleRevisionThatIsCurrent is the plan-side half of the
// fingerprint contract.
//
// discover resolves modules in a fresh temp TF_DATA_DIR on every run, so it
// fingerprints what a source address means today. The runner's TF_DATA_DIR
// persists on the state volume, and Terraform's installer leaves an
// already-installed module alone while the source ADDRESS is unchanged — so
// after the branch behind that address advanced, discover fingerprinted v2
// while plan silently reused v1, and Caesium cached the v2 fingerprint against
// a plan produced from v1. A green run for code that was never planned.
func TestInitPlansTheModuleRevisionThatIsCurrent(t *testing.T) {
	if _, err := os.Stat(terraformPath(t)); err != nil {
		t.Skipf("terraform is not on PATH: %v", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}

	// The identity tf-runner's prepare pins in production (pinGitIdentity):
	// without it git resolves the machine's own hostname to synthesize one for
	// the clone's reflog, which stalls on a container with no DNS entry for its
	// name. This package is the library, so the test supplies it.
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME":     "Pack Test",
		"GIT_AUTHOR_EMAIL":    "reagent@caesium.test",
		"GIT_COMMITTER_NAME":  "Pack Test",
		"GIT_COMMITTER_EMAIL": "reagent@caesium.test",
	} {
		t.Setenv(key, value)
	}

	base := t.TempDir()
	module := filepath.Join(base, "module")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	writeModuleValue(t, module, "v1")
	git(t, module, "init", "--quiet", "--initial-branch=main")
	commit(t, module, "v1")

	root := filepath.Join(base, "stack")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	stack := `terraform {
  required_version = ">= 1.10.0"

  backend "local" {
    path = "terraform.tfstate"
  }
}

module "m" {
  source = "git::file://` + module + `"
}

resource "terraform_data" "probe" {
  input = module.m.value
}
`
	if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte(stack), 0o600); err != nil {
		t.Fatal(err)
	}

	// The runner's data directory PERSISTS across phases and runs; that is the
	// whole hazard, so the test must reuse it rather than start clean.
	t.Setenv("TF_DATA_DIR", filepath.Join(base, "tfdata"))

	runner, err := NewRunner(root, terraformPath(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedProbeInput(t, runner, filepath.Join(base, "first.plan")); got != "v1" {
		t.Fatalf("the first plan used %q, want v1; the fixture is wrong", got)
	}

	// The branch advances. The source ADDRESS is unchanged — which is exactly
	// the case the installer skips.
	writeModuleValue(t, module, "v2")
	commit(t, module, "v2")

	if got := plannedProbeInput(t, runner, filepath.Join(base, "second.plan")); got != "v2" {
		t.Fatalf("plan used the cached module revision (%q, want v2): the stack discover fingerprinted "+
			"is not the stack that would be applied", got)
	}
}

func writeModuleValue(t *testing.T, dir, value string) {
	t.Helper()
	body := "output \"value\" {\n  value = \"" + value + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// plannedProbeInput runs one full init+plan cycle and returns what the plan
// says terraform_data.probe will be created with — i.e. the module's output as
// the CURRENT plan resolved it.
func plannedProbeInput(t *testing.T, runner *Runner, planPath string) string {
	t.Helper()
	ctx := t.Context()
	if err := runner.Init(ctx, nil); err != nil {
		t.Fatalf("terraform init: %v", err)
	}
	if _, err := runner.Plan(ctx, planPath); err != nil {
		t.Fatalf("terraform plan: %v", err)
	}
	plan, err := runner.ShowPlanFile(ctx, planPath)
	if err != nil {
		t.Fatalf("terraform show: %v", err)
	}
	for _, change := range plan.ResourceChanges {
		if change.Address != "terraform_data.probe" || change.Change == nil {
			continue
		}
		after, ok := change.Change.After.(map[string]any)
		if !ok {
			t.Fatalf("planned probe has no attributes: %#v", change.Change.After)
		}
		value, ok := after["input"].(string)
		if !ok {
			t.Fatalf("planned probe input is not a string: %#v", after["input"])
		}
		return value
	}
	t.Fatal("the plan contains no terraform_data.probe")
	return ""
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{
		"-c", "user.name=Pack Test",
		"-c", "user.email=reagent@caesium.test",
		"-c", "commit.gpgsign=false",
		"-c", "safe.directory=" + dir,
	}, args...)
	cmd := exec.CommandContext(t.Context(), "git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func commit(t *testing.T, dir, msg string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-m", msg)
}

// A relative TF_DATA_DIR must be refused, not resolved against the process's
// working directory.
//
// The failure it would otherwise cause is invisible: discardInstalledModules
// would RemoveAll a path that does not exist, os.RemoveAll returns nil for
// that, and the stale-module reuse of 3881714399 would come back with nothing
// logged. tf-runner's prepare always sets an absolute path, so this is a guard
// on the library's own contract rather than a live bug — which is exactly why
// it needs a test to stay true.
func TestInitRejectsARelativeDataDir(t *testing.T) {
	root := offlineStack(t)
	t.Setenv(dataDirEnvVar, filepath.Join("relative", "tfdata"))

	runner, err := NewRunner(root, terraformPath(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	err = runner.Init(t.Context(), nil)
	if err == nil {
		t.Fatal("init accepted a relative TF_DATA_DIR; module re-resolution would silently do nothing")
	}
	for _, want := range []string{dataDirEnvVar, "absolute"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the failure does not mention %q, so an operator cannot act on it: %v", want, err)
		}
	}
}

func TestDataDirResolution(t *testing.T) {
	root := t.TempDir()
	runner := &Runner{root: root}

	t.Run("absent falls back beside the configuration", func(t *testing.T) {
		t.Setenv(dataDirEnvVar, "")
		if err := os.Unsetenv(dataDirEnvVar); err != nil {
			t.Fatal(err)
		}
		got, err := runner.dataDir()
		if err != nil {
			t.Fatalf("dataDir: %v", err)
		}
		if want := filepath.Join(root, DataDirName); got != want {
			t.Fatalf("dataDir = %q, want %q", got, want)
		}
	})

	t.Run("absolute is taken as given", func(t *testing.T) {
		t.Setenv(dataDirEnvVar, "/state/artifacts/tfdata")
		got, err := runner.dataDir()
		if err != nil {
			t.Fatalf("dataDir: %v", err)
		}
		if got != "/state/artifacts/tfdata" {
			t.Fatalf("dataDir = %q", got)
		}
	})

	// Whitespace around an absolute path is trimmed, not treated as relative.
	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		t.Setenv(dataDirEnvVar, "  /state/tfdata  ")
		got, err := runner.dataDir()
		if err != nil {
			t.Fatalf("dataDir: %v", err)
		}
		if got != "/state/tfdata" {
			t.Fatalf("dataDir = %q", got)
		}
	})

	for name, value := range map[string]string{
		"a bare name":   "tfdata",
		"a dotted path": "./tfdata",
		"an ascent":     "../tfdata",
	} {
		t.Run("relative is refused: "+name, func(t *testing.T) {
			t.Setenv(dataDirEnvVar, value)
			if got, err := runner.dataDir(); err == nil {
				t.Fatalf("dataDir accepted the relative %q as %q", value, got)
			}
		})
	}
}

func TestInitCLIArgs(t *testing.T) {
	cases := map[string]struct{ existing, want string }{
		"empty":                  {"", "-lockfile=readonly"},
		"whitespace only":        {"   ", "-lockfile=readonly"},
		"appends to other args":  {"-no-color", "-no-color -lockfile=readonly"},
		"respects an operator's": {"-lockfile=false", "-lockfile=false"},
		"does not double up":     {"-lockfile=readonly", "-lockfile=readonly"},
	}
	for name, tc := range cases {
		if got := initCLIArgs(tc.existing); got != tc.want {
			t.Errorf("%s: initCLIArgs(%q) = %q, want %q", name, tc.existing, got, tc.want)
		}
	}
}

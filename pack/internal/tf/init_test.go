package tf

import (
	"context"
	"io"
	"os"
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

// offlineStack copies pack/testdata/offline/stack (provider-free by
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

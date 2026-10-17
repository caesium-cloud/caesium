package guardrails_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
)

// TestIntegrationLanesServePolicyFixture pins the plan item H-1 wiring the
// integration scenarios rely on to fail rather than skip: every justfile
// integration server serves test/testdata/policy/two-namespaces.yaml, the
// runners carry the env that says so, and the one auth lane that deliberately
// has no policy file (the coverage journey) is marked as such. A recipe that
// silently stopped mounting the fixture would otherwise turn the
// fixture-dependent scenarios into skips.
func TestIntegrationLanesServePolicyFixture(t *testing.T) {
	root := repoRoot(t)
	justfileBytes, err := os.ReadFile(filepath.Join(root, "justfile"))
	if err != nil {
		t.Fatalf("read justfile: %v", err)
	}
	justfile := string(justfileBytes)

	const (
		mount = "-v {{ repo_dir }}/test/testdata/policy:/etc/caesium/policy-fixture:ro"
		file  = "-e CAESIUM_AUTH_POLICY_FILE=/etc/caesium/policy-fixture/two-namespaces.yaml"
	)
	for _, recipe := range []string{"integration-up", "integration-up-distributed", "integration-up-owner-memory", "integration-up-infra"} {
		body := justRecipeBody(justfile, recipe)
		if !strings.Contains(body, mount) || !strings.Contains(body, file) {
			t.Errorf("%s must mount the policy fixture (%q and %q)", recipe, mount, file)
		}
	}
	for recipe, needle := range map[string]string{
		"integration-up-agent":   "cp {{ repo_dir }}/test/testdata/policy/two-namespaces.yaml {{ repo_dir }}/.tmp/auth-policy/policy.yaml",
		"integration-test":       "-e CAESIUM_IT_POLICY_FIXTURE=two-namespaces",
		"integration-test-agent": "-e CAESIUM_IT_AUTH_POLICY_PATH=",
	} {
		if !strings.Contains(justRecipeBody(justfile, recipe), needle) {
			t.Errorf("%s is missing %q", recipe, needle)
		}
	}

	journeys, err := os.ReadFile(filepath.Join(root, "scripts", "coverage-journeys.sh"))
	if err != nil {
		t.Fatalf("read coverage journeys: %v", err)
	}
	if !strings.Contains(string(journeys), "-e CAESIUM_IT_AUTH_POLICY=none") {
		t.Error("the coverage journey's auth lane serves no policy file and must say so with CAESIUM_IT_AUTH_POLICY=none")
	}

	fixture, err := os.ReadFile(filepath.Join(root, "test", "testdata", "policy", "two-namespaces.yaml"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	parsed, err := policy.Parse(fixture)
	if err != nil {
		t.Fatalf("the fixture must be a valid access policy: %v", err)
	}
	marketing, ok := parsed.Namespaces["marketing"]
	if !ok || marketing.Secrets == nil || len(marketing.Secrets.Allow) == 0 || marketing.Quotas == nil || marketing.Quotas.MaxConcurrentRuns != 1 {
		t.Errorf("the fixture's marketing namespace needs secret rules and a run quota of one: %+v", marketing)
	}
	if _, ok := parsed.Namespaces["finance"]; !ok {
		t.Error("the fixture must declare finance")
	}
	if len(parsed.Bindings) == 0 || strings.Join(parsed.Bindings[0].Subjects.Groups, ",") != "iaa-reload-viewers" {
		t.Error("the fixture's first binding must be the policy-reload scenarios' viewer group")
	}
	for _, binding := range parsed.Bindings {
		for _, group := range binding.Subjects.Groups {
			if group == "caesium-admins" || group == "*" {
				t.Errorf("the fixture must not bind %q (TestAuthSessionCookieWithoutGrantsIsRefused relies on it)", group)
			}
		}
	}
	if strings.Count(string(fixture), "iaa-reload-viewers") != 1 {
		t.Error("TestAuthPolicyReload renames the first occurrence of its group: name it exactly once")
	}
}

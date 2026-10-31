package guardrails_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

// The Keycloak lane (plan item G1) wires one realm fixture, one policy file,
// two jobs and the justfile recipes together; a drift between any two of them
// fails only on the live lane, minutes into CI. These tests catch it here.

const (
	keycloakFixtureDir = "test/testdata/keycloak"
	keycloakCallback   = "http://127.0.0.1:8080/auth/sso/oidc/callback"
)

type keycloakRealmFixture struct {
	Realm                       string   `json:"realm"`
	SSLRequired                 string   `json:"sslRequired"`
	RevokeRefreshToken          bool     `json:"revokeRefreshToken"`
	RefreshTokenMaxReuse        *int     `json:"refreshTokenMaxReuse"`
	DefaultDefaultClientScopes  []string `json:"defaultDefaultClientScopes"`
	DefaultOptionalClientScopes []string `json:"defaultOptionalClientScopes"`
	ClientScopes                []struct {
		Name            string `json:"name"`
		ProtocolMappers []struct {
			ProtocolMapper string            `json:"protocolMapper"`
			Config         map[string]string `json:"config"`
		} `json:"protocolMappers"`
	} `json:"clientScopes"`
	Clients []struct {
		ClientID                  string            `json:"clientId"`
		PublicClient              bool              `json:"publicClient"`
		Secret                    string            `json:"secret"`
		StandardFlowEnabled       bool              `json:"standardFlowEnabled"`
		ImplicitFlowEnabled       bool              `json:"implicitFlowEnabled"`
		DirectAccessGrantsEnabled bool              `json:"directAccessGrantsEnabled"`
		RedirectURIs              []string          `json:"redirectUris"`
		Attributes                map[string]string `json:"attributes"`
		DefaultClientScopes       []string          `json:"defaultClientScopes"`
		OptionalClientScopes      []string          `json:"optionalClientScopes"`
	} `json:"clients"`
	Groups []struct {
		Name string `json:"name"`
	} `json:"groups"`
	Users []struct {
		Username      string   `json:"username"`
		Email         string   `json:"email"`
		EmailVerified bool     `json:"emailVerified"`
		Enabled       bool     `json:"enabled"`
		FirstName     string   `json:"firstName"`
		LastName      string   `json:"lastName"`
		Groups        []string `json:"groups"`
		Credentials   []struct {
			Type      string `json:"type"`
			Value     string `json:"value"`
			Temporary bool   `json:"temporary"`
		} `json:"credentials"`
	} `json:"users"`
}

// keycloakMatrixGroup names the realm group bound to role in namespace ns
// ("all" for every namespace) by the lane policy.
func keycloakMatrixGroup(role, ns string) string { return "kc-matrix-" + role + "-" + ns }

func readKeycloakRealm(t *testing.T, root string) keycloakRealmFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, keycloakFixtureDir, "caesium-realm.json"))
	if err != nil {
		t.Fatalf("read realm fixture: %v", err)
	}
	var realm keycloakRealmFixture
	if err := json.Unmarshal(raw, &realm); err != nil {
		t.Fatalf("parse realm fixture: %v", err)
	}
	return realm
}

func readPolicyFixture(t *testing.T, root, rel string) *policy.AccessPolicy {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	parsed, err := policy.Parse(raw)
	if err != nil {
		t.Fatalf("%s must be a valid access policy: %v", rel, err)
	}
	return parsed
}

// TestKeycloakLanePolicyMatchesTheSharedFixture: the lane serves its own
// policy only because two-namespaces.yaml must never bind caesium-admins. Its
// namespaces (with their secret rules and quota) and the shared bindings stay
// identical; the extra bindings are exactly the realm users' ones.
func TestKeycloakLanePolicyMatchesTheSharedFixture(t *testing.T) {
	root := repoRoot(t)
	shared := readPolicyFixture(t, root, "test/testdata/policy/two-namespaces.yaml")
	lane := readPolicyFixture(t, root, keycloakFixtureDir+"/policy.yaml")

	if !reflect.DeepEqual(shared.Namespaces, lane.Namespaces) {
		t.Errorf("the Keycloak lane policy's namespaces must equal two-namespaces.yaml's:\nshared: %+v\nlane:   %+v", shared.Namespaces, lane.Namespaces)
	}
	for _, binding := range shared.Bindings {
		groups := binding.Subjects.Groups
		if slices.Contains(groups, "iaa-reload-viewers") {
			continue // the api-key lane's reload scenarios' group, not a realm group
		}
		if slices.ContainsFunc(groups, func(g string) bool { return strings.HasPrefix(g, "iaa-ldap-") }) {
			continue // the api-key lane's OpenLDAP fixture groups (E1b), not realm groups
		}
		if !slices.ContainsFunc(lane.Bindings, func(b policy.Binding) bool { return reflect.DeepEqual(b, binding) }) {
			t.Errorf("the Keycloak lane policy is missing two-namespaces.yaml's binding %+v", binding)
		}
	}

	want := map[string]policy.Binding{
		"group:caesium-admins":  {Role: "admin", Namespaces: []string{"*"}},
		"group:marketing-eng":   {Role: "operator", Namespaces: []string{"marketing"}},
		"group:finance-eng":     {Role: "operator", Namespaces: []string{"finance"}},
		"user:dave@example.com": {Role: "admin", Namespaces: []string{"*"}},
		"user:erin@example.com": {Role: "viewer", Namespaces: []string{"finance"}},
	}
	// G3: one group per role and namespace for the SSO principals the lane
	// scenarios sign in (users created at runtime and joined to them).
	for _, role := range []string{"viewer", "runner", "operator", "admin"} {
		for _, ns := range []string{"default", "marketing", "finance", "all"} {
			grant := ns
			if ns == "all" {
				grant = "*"
			}
			want["group:"+keycloakMatrixGroup(role, ns)] = policy.Binding{Role: models.Role(role), Namespaces: []string{grant}}
		}
	}
	got := map[string]policy.Binding{}
	for _, b := range lane.Bindings {
		subjects := len(b.Subjects.Groups) + len(b.Subjects.Users)
		if subjects != 1 {
			t.Errorf("keep one subject per lane binding so each realm user maps to one row: %+v", b)
		}
		for _, g := range b.Subjects.Groups {
			got["group:"+g] = policy.Binding{Role: b.Role, Namespaces: b.Namespaces}
		}
		for _, u := range b.Subjects.Users {
			got["user:"+u] = policy.Binding{Role: b.Role, Namespaces: b.Namespaces}
		}
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("Keycloak lane bindings drifted:\nwant %+v\ngot  %+v", want, got)
	}
}

// TestKeycloakRealmFixtureShape pins what the lane scenarios rely on: a
// confidential code-flow client with an exact redirect, the groups scope with
// a flat Group Membership mapper in the ID token, and the five users with the
// group membership and email verification the policy expects.
func TestKeycloakRealmFixtureShape(t *testing.T) {
	realm := readKeycloakRealm(t, repoRoot(t))
	if realm.Realm != "caesium" {
		t.Fatalf("realm = %q, want caesium (the justfile issuer URL names it)", realm.Realm)
	}

	scopes := map[string]bool{}
	groupsMapper := false
	for _, scope := range realm.ClientScopes {
		scopes[scope.Name] = true
		for _, m := range scope.ProtocolMappers {
			if scope.Name == "groups" && m.ProtocolMapper == "oidc-group-membership-mapper" &&
				m.Config["claim.name"] == "groups" && m.Config["full.path"] == "false" && m.Config["id.token.claim"] == "true" {
				groupsMapper = true
			}
		}
	}
	// An import that lists clientScopes gets no built-in ones: everything the
	// client and Caesium's default scopes (openid profile email groups) need
	// must be defined here, including basic (the sub claim).
	for _, name := range []string{"basic", "profile", "email", "groups"} {
		if !scopes[name] {
			t.Errorf("client scope %q must be defined in the realm fixture", name)
		}
	}
	if !groupsMapper {
		t.Error("the groups scope needs a Group Membership mapper: claim groups, full path off, in the ID token (Caesium reads groups from the ID token)")
	}

	if len(realm.Clients) != 1 || realm.Clients[0].ClientID != "caesium" {
		t.Fatalf("the realm must define exactly the caesium client: %+v", realm.Clients)
	}
	client := realm.Clients[0]
	switch {
	case client.PublicClient || client.Secret == "":
		t.Error("the caesium client must be confidential with a secret")
	case !client.StandardFlowEnabled || client.ImplicitFlowEnabled:
		t.Error("the caesium client must use the standard (code) flow only")
	case !client.DirectAccessGrantsEnabled:
		t.Error("direct access grants stay enabled for the G3 scenarios")
	case !reflect.DeepEqual(client.RedirectURIs, []string{keycloakCallback}):
		t.Errorf("redirect URIs = %v, want exactly [%s]", client.RedirectURIs, keycloakCallback)
	case client.Attributes["pkce.code.challenge.method"] != "S256":
		t.Error("the caesium client must require PKCE S256 (Caesium always sends it)")
	case !slices.Contains(client.OptionalClientScopes, "groups"):
		t.Error("groups must be an optional client scope (Caesium requests it)")
	}
	for _, name := range append(append([]string{}, client.DefaultClientScopes...), client.OptionalClientScopes...) {
		if !scopes[name] {
			t.Errorf("the caesium client references undefined client scope %q", name)
		}
	}

	// G3: strict rotation, so a refresh that redeemed a superseded refresh
	// token fails with invalid_grant instead of passing unnoticed.
	if !realm.RevokeRefreshToken || realm.RefreshTokenMaxReuse == nil || *realm.RefreshTokenMaxReuse != 0 {
		t.Error("the realm must revoke refresh tokens on use (revokeRefreshToken true, refreshTokenMaxReuse 0): the G3 rotation scenario relies on it")
	}

	declared := map[string]bool{}
	for _, g := range realm.Groups {
		declared["/"+g.Name] = true
	}
	for _, role := range []string{"viewer", "runner", "operator", "admin"} {
		for _, ns := range []string{"default", "marketing", "finance", "all"} {
			if !declared["/"+keycloakMatrixGroup(role, ns)] {
				t.Errorf("the realm must declare group %q (the lane policy binds it)", keycloakMatrixGroup(role, ns))
			}
		}
	}
	type user struct {
		verified bool
		groups   []string
	}
	want := map[string]user{
		"admin@example.com": {true, []string{"/caesium-admins"}},
		"bob@example.com":   {true, []string{"/marketing-eng"}},
		"carol@example.com": {true, []string{}},
		"dave@example.com":  {false, []string{}},
		"erin@example.com":  {true, []string{}},
	}
	seen := map[string]bool{}
	for _, u := range realm.Users {
		seen[u.Username] = true
		w, ok := want[u.Username]
		if !ok {
			t.Errorf("unexpected realm user %q", u.Username)
			continue
		}
		groups := u.Groups
		if groups == nil {
			groups = []string{}
		}
		if u.Email != u.Username || u.EmailVerified != w.verified || !u.Enabled || !reflect.DeepEqual(groups, w.groups) {
			t.Errorf("realm user %q drifted: email %q verified %v enabled %v groups %v", u.Username, u.Email, u.EmailVerified, u.Enabled, u.Groups)
		}
		// Keycloak's user profile interposes an update-profile page on login
		// for a user without a first and last name.
		if u.FirstName == "" || u.LastName == "" {
			t.Errorf("realm user %q needs a first and last name", u.Username)
		}
		if len(u.Credentials) != 1 || u.Credentials[0].Type != "password" || u.Credentials[0].Value == "" || u.Credentials[0].Temporary {
			t.Errorf("realm user %q needs one non-temporary password", u.Username)
		}
		for _, g := range u.Groups {
			if !declared[g] {
				t.Errorf("realm user %q is in undeclared group %q", u.Username, g)
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("realm user %q is missing", name)
		}
	}
}

// TestKeycloakLaneJobsApplyIntoDeclaredNamespaces: the two jobs the lane
// applies are valid and land one in each non-default lane namespace.
func TestKeycloakLaneJobsApplyIntoDeclaredNamespaces(t *testing.T) {
	root := repoRoot(t)
	lane := readPolicyFixture(t, root, keycloakFixtureDir+"/policy.yaml")
	want := map[string]string{"kc-lane-marketing": "marketing", "kc-lane-finance": "finance"}
	paths, err := filepath.Glob(filepath.Join(root, keycloakFixtureDir, "jobs", "*.job.yaml"))
	if err != nil || len(paths) != len(want) {
		t.Fatalf("want %d lane jobs, found %v (%v)", len(want), paths, err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		def, err := jobdef.Parse(raw)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want[def.Metadata.Alias] != def.Metadata.Namespace {
			t.Errorf("%s: alias %q namespace %q, want one of %v", path, def.Metadata.Alias, def.Metadata.Namespace, want)
		}
		if _, ok := lane.Namespaces[def.Metadata.Namespace]; !ok {
			t.Errorf("%s: namespace %q is not declared by the lane policy", path, def.Metadata.Namespace)
		}
	}
}

// TestKeycloakLaneRecipesMatchTheFixtures ties the justfile to the fixtures:
// the client id/secret, issuer realm and public base the server is started
// with, the files it serves, the digest-pinned image, the runner's lane env
// and -run pattern, and the failure handling (trap before the first container,
// every log dump redacted).
func TestKeycloakLaneRecipesMatchTheFixtures(t *testing.T) {
	root := repoRoot(t)
	justfileBytes, err := os.ReadFile(filepath.Join(root, "justfile"))
	if err != nil {
		t.Fatalf("read justfile: %v", err)
	}
	justfile := string(justfileBytes)
	realm := readKeycloakRealm(t, root)
	if len(realm.Clients) != 1 {
		t.Fatalf("realm clients: %+v", realm.Clients)
	}

	image := regexp.MustCompile(`(?m)^keycloak_image := "(quay\.io/keycloak/keycloak:\d+\.\d+\.\d+@sha256:[0-9a-f]{64})"$`)
	if !image.MatchString(justfile) {
		t.Error(`keycloak_image must be pinned as quay.io/keycloak/keycloak:<x.y.z>@sha256:<digest> (quay.io re-pushes version tags)`)
	}
	if !strings.Contains(justfile, "\nkeycloak_port := \"8180\"\n") {
		t.Error(`keycloak_port must stay "8180": the realm's issuer and the docs name it`)
	}
	if !strings.Contains(justfile, `keycloak_integration_run := env("CAESIUM_KEYCLOAK_INTEGRATION_RUN", "TestIntegrationTestSuite/TestKeycloak")`) {
		t.Error("keycloak_integration_run must be suite-qualified (a bare method name matches nothing)")
	}

	up := justRecipeBody(justfile, "integration-up-keycloak")
	for _, needle := range []string{
		// Loopback only: the realm carries fixture admin credentials and
		// no TLS, and Caesium holds the host Docker socket.
		"-p 127.0.0.1:8080:8080 -p 127.0.0.1:{{ keycloak_port }}:{{ keycloak_port }}",
		"-v {{ repo_dir }}/test/testdata/keycloak/caesium-realm.json:/opt/keycloak/data/import/caesium-realm.json:ro",
		"start-dev --import-realm --http-port={{ keycloak_port }} --hostname=http://127.0.0.1:{{ keycloak_port }}",
		"--network=container:{{ keycloak_it_container }}",
		"-v {{ repo_dir }}/test/testdata/keycloak:/etc/caesium/keycloak-fixture:ro",
		"-e CAESIUM_AUTH_POLICY_FILE=/etc/caesium/keycloak-fixture/policy.yaml",
		"-e CAESIUM_AUTH_MODE=api-key",
		"-e CAESIUM_AUTH_REQUIRE_TLS=false",
		"-e CAESIUM_AUTH_PUBLIC_BASE_URL=" + strings.TrimSuffix(keycloakCallback, "/auth/sso/oidc/callback"),
		"-e CAESIUM_AUTH_OIDC_ENABLED=true",
		"-e CAESIUM_AUTH_OIDC_ISSUER_URL=http://127.0.0.1:{{ keycloak_port }}/realms/" + realm.Realm,
		"-e CAESIUM_AUTH_OIDC_CLIENT_ID=" + realm.Clients[0].ClientID,
		"-e CAESIUM_AUTH_OIDC_CLIENT_SECRET=" + realm.Clients[0].Secret,
		// G3: refresh is due in seconds on this lane only.
		"-e CAESIUM_AUTH_GROUP_REFRESH_INTERVAL={{ keycloak_group_refresh_interval }}",
		"-e CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT={{ keycloak_group_refresh_timeout }}",
		"-e CAESIUM_AUTH_GROUP_MAX_STALENESS={{ keycloak_group_max_staleness }}",
		"-e KC_BOOTSTRAP_ADMIN_USERNAME={{ keycloak_admin_user }}",
		"-e KC_BOOTSTRAP_ADMIN_PASSWORD={{ keycloak_admin_password }}",
		"-v {{ repo_dir }}/test/testdata/keycloak/jobs:/keycloak-jobs:ro",
		"job apply --path /keycloak-jobs --server http://127.0.0.1:8080",
	} {
		if !strings.Contains(up, needle) {
			t.Errorf("integration-up-keycloak is missing %q", needle)
		}
	}
	test := justRecipeBody(justfile, "integration-test-keycloak")
	for _, needle := range []string{
		"just tag={{ tag }} integration-up-keycloak",
		"--network=container:{{ keycloak_it_container }}",
		"-e CAESIUM_KEYCLOAK_LANE=true",
		"-e CAESIUM_KEYCLOAK_URL=http://127.0.0.1:{{ keycloak_port }}",
		"-e CAESIUM_KEYCLOAK_REALM=" + realm.Realm,
		"-e CAESIUM_API_KEY",
		`-test.run "{{ keycloak_integration_run }}"`,
		"-e CAESIUM_KEYCLOAK_ADMIN_USER={{ keycloak_admin_user }}",
		"-e CAESIUM_KEYCLOAK_ADMIN_PASSWORD={{ keycloak_admin_password }}",
		"-e CAESIUM_KEYCLOAK_GROUP_REFRESH_INTERVAL={{ keycloak_group_refresh_interval }}",
		"-e CAESIUM_KEYCLOAK_GROUP_MAX_STALENESS={{ keycloak_group_max_staleness }}",
		"for scenario in {{ keycloak_required_scenarios }}; do",
		`--- PASS: TestIntegrationTestSuite/${scenario} \(`,
		`--- SKIP: TestIntegrationTestSuite/TestKeycloak[A-Za-z0-9_]* \(`,
	} {
		if !strings.Contains(test, needle) {
			t.Errorf("integration-test-keycloak is missing %q", needle)
		}
	}

	for _, name := range []string{"integration-up-keycloak", "integration-test-keycloak", "ui-e2e-keycloak"} {
		body := justRecipeBody(justfile, name)
		trap := strings.Index(body, "trap on_exit EXIT")
		if trap < 0 {
			t.Errorf("%s must install its on_exit trap", name)
			continue
		}
		for _, start := range []string{" run -d ", "integration-up-keycloak\n"} {
			if i := strings.Index(body, start); i >= 0 && i < trap {
				t.Errorf("%s starts the lane (%q) before installing its cleanup trap", name, strings.TrimSpace(start))
			}
		}
		// Container logs carry the plaintext bootstrap key; only the
		// redacting helper may print them. The key capture pipes into awk.
		for _, published := range regexp.MustCompile(`(?:^|\s)-p\s+((?:[0-9.]+:)?(?:\{\{ \w+ \}\}|\d+):(?:\{\{ \w+ \}\}|\d+))`).FindAllStringSubmatch(body, -1) {
			if !strings.HasPrefix(published[1], "127.0.0.1:") {
				t.Errorf("%s publishes %s on every host interface; bind it to 127.0.0.1", name, published[1])
			}
		}
		for _, line := range strings.Split(body, "\n") {
			if strings.Contains(line, " logs ") && !strings.Contains(line, "scripts/lane-logs.sh") && !strings.Contains(line, "| awk ") {
				t.Errorf("%s dumps container logs without scripts/lane-logs.sh: %s", name, strings.TrimSpace(line))
			}
		}
	}
	if !strings.Contains(test, `sed -E 's/(csk|css)_[A-Za-z0-9_-]+/[REDACTED_CREDENTIAL]/g' "$log"`) {
		t.Error("integration-test-keycloak must print the runner log through the credential filter")
	}
}

// TestKeycloakRequiredScenariosMatchTheSuite: integration-test-keycloak proves
// the lane by a top-level PASS per scenario named in
// keycloak_required_scenarios (G3), so that list must be exactly the
// TestKeycloak* suite methods in test/: a new scenario left off it would be
// allowed to skip, a stale name would fail every run.
func TestKeycloakRequiredScenariosMatchTheSuite(t *testing.T) {
	root := repoRoot(t)
	justfileBytes, err := os.ReadFile(filepath.Join(root, "justfile"))
	if err != nil {
		t.Fatalf("read justfile: %v", err)
	}
	m := regexp.MustCompile(`(?m)^keycloak_required_scenarios := env\("CAESIUM_KEYCLOAK_REQUIRED_SCENARIOS", "([^"]*)"\)$`).FindStringSubmatch(string(justfileBytes))
	if m == nil {
		t.Fatal(`justfile must define keycloak_required_scenarios := env("CAESIUM_KEYCLOAK_REQUIRED_SCENARIOS", "<names>")`)
	}
	required := strings.Fields(m[1])

	method := regexp.MustCompile(`(?m)^func \(s \*IntegrationTestSuite\) (TestKeycloak[A-Za-z0-9_]*)\(\)`)
	files, err := filepath.Glob(filepath.Join(root, "test", "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	var defined []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, sm := range method.FindAllStringSubmatch(string(raw), -1) {
			defined = append(defined, sm[1])
		}
	}
	slices.Sort(required)
	slices.Sort(defined)
	if len(defined) == 0 {
		t.Fatal("no TestKeycloak* suite methods found under test/; the scan is broken")
	}
	if !slices.Equal(required, defined) {
		t.Errorf("keycloak_required_scenarios drifted from the suite:\njustfile: %v\ntest/:    %v", required, defined)
	}
}

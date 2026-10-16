//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"gopkg.in/yaml.v3"
)

// TestAuthKeyNamespacesGrantResolution drives API-key grant resolution through
// the live auth lane: a key minted today has NULL namespaces and keeps its role
// cluster-wide, and the stored namespaces decide its grants on every request.
//
// The UPDATEs seed stored values minting cannot produce (an explicit ["*"],
// which minting stores as NULL, and an empty list); minting with
// `--namespace` is TestAuthNamespacedKeyMintingCLI.
func (s *IntegrationTestSuite) TestAuthKeyNamespacesGrantResolution() {
	s.requireAuthLane()

	description := fmt.Sprintf("auth-lane namespaces key %d", time.Now().UnixNano())
	created := s.createAPIKeyCLI("--role", "viewer", "--description", description)
	defer func() {
		out, err := s.runCLIStdout("auth", "key", "revoke", "--id", created.Meta.ID, "--server", s.caesiumURL)
		s.NoError(err, "caesium auth key revoke failed:\n%s", out)
	}()

	// /v1/atoms is a cluster route, so it needs the viewer role at "*".
	s.requireKeyStatus(created.Plaintext, http.StatusOK)
	s.requireWhoamiRole(created.Plaintext, "viewer")

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()

	// The live (sharded) server migrated the column; a new key stores NULL.
	var isNull bool
	s.Require().NoError(catalogDB.QueryRowContext(s.T().Context(),
		`SELECT namespaces IS NULL FROM api_keys WHERE id = ?`, created.Meta.ID).Scan(&isNull))
	s.True(isNull, "a key minted without namespaces must store SQL NULL")

	// NULL namespaces stay off the wire: list output is byte-compatible.
	listed := s.listAPIKeysRaw()
	entry, ok := listed[created.Meta.ID]
	s.Require().True(ok, "key %s must appear in `auth key list`", created.Meta.ID)
	s.NotContains(entry, "namespaces", "a NULL-namespaces key must not serialise a namespaces field")

	setNamespaces := func(value string) {
		s.T().Helper()
		result, err := catalogDB.ExecContext(s.T().Context(),
			`UPDATE api_keys SET namespaces = ? WHERE id = ?`, value, created.Meta.ID)
		s.Require().NoError(err)
		affected, err := result.RowsAffected()
		s.Require().NoError(err)
		s.Require().Equal(int64(1), affected)
	}

	// Confined to marketing: no cluster role, so a cluster route is denied and
	// the denial is audited with an empty cluster role.
	setNamespaces(`["marketing"]`)
	s.requireKeyStatus(created.Plaintext, http.StatusForbidden)

	auditOut, auditErr, err := s.runCLISeparate(
		"auth", "audit",
		"--action", "auth.denied",
		"--actor", created.Meta.KeyPrefix,
		"--limit", "50",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium auth audit failed:\nstdout: %s\nstderr: %s", auditOut, auditErr)
	var denials []struct {
		Actor    string          `json:"actor"`
		Outcome  string          `json:"outcome"`
		Metadata json.RawMessage `json:"metadata"`
	}
	s.Require().NoError(json.Unmarshal([]byte(auditOut), &denials),
		"caesium auth audit stdout must be exactly one JSON array:\n%s", auditOut)
	s.Require().Len(denials, 1, "exactly one denial for the confined key:\n%s", auditOut)
	s.Equal(created.Meta.KeyPrefix, denials[0].Actor)
	s.Equal("denied", denials[0].Outcome)
	var metadata struct {
		KeyRole string `json:"key_role"`
		Reason  string `json:"reason"`
		Path    string `json:"path"`
	}
	s.Require().NoError(json.Unmarshal(denials[0].Metadata, &metadata))
	s.Equal("", metadata.KeyRole, "a confined key has no cluster role")
	s.Equal("insufficient_role", metadata.Reason)
	s.Equal("/v1/atoms", metadata.Path)

	listed = s.listAPIKeysRaw()
	s.Require().Contains(listed, created.Meta.ID)
	s.JSONEq(`["marketing"]`, string(listed[created.Meta.ID]["namespaces"]))

	// An explicit cluster grant restores the key's role everywhere.
	setNamespaces(`["*"]`)
	s.requireKeyStatus(created.Plaintext, http.StatusOK)
	s.requireWhoamiRole(created.Plaintext, "viewer")

	// An empty list is not "cluster": it fails closed.
	setNamespaces(`[]`)
	s.requireKeyStatus(created.Plaintext, http.StatusForbidden)
}

// TestAuthKeyRotationKeepsNamespaces drives `caesium auth key rotate` on a
// key minted with `--namespace` and proves the replacement secret stays
// confined and keeps its expiry: rotation replaces the secret, never the
// grant or its lifetime. A NULL key rotates to NULL, so ordinary keys keep
// their cluster role, and a key confined to a namespace the policy file does
// not declare is not rotated at all.
func (s *IntegrationTestSuite) TestAuthKeyRotationKeepsNamespaces() {
	s.requireAuthLane()
	s.useKeyPolicy()

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()
	var minted []string
	defer func() {
		for _, id := range minted {
			out, err := s.runCLIStdout("auth", "key", "revoke", "--id", id, "--server", s.caesiumURL)
			s.NoError(err, "caesium auth key revoke %s failed:\n%s", id, out)
		}
	}()
	rotate := func(id string) createdAPIKey {
		s.T().Helper()
		out, err := s.runCLIStdout("auth", "key", "rotate", "--id", id, "--grace-period", "1m", "--server", s.caesiumURL)
		s.Require().NoError(err, "caesium auth key rotate failed:\n%s", out)
		rotated := s.parseCreatedKey(out)
		minted = append(minted, rotated.Meta.ID)
		return rotated
	}
	storedNamespaces := func(id string) (string, bool) {
		s.T().Helper()
		var value *string
		s.Require().NoError(catalogDB.QueryRowContext(s.T().Context(),
			`SELECT namespaces FROM api_keys WHERE id = ?`, id).Scan(&value))
		if value == nil {
			return "", false
		}
		return *value, true
	}

	confined := s.createAPIKeyCLI("--role", "viewer", "--namespace", "marketing", "--expires", "2h",
		"--description", fmt.Sprintf("auth-lane rotate confined key %d", time.Now().UnixNano()))
	minted = append(minted, confined.Meta.ID)
	s.Equal([]string{"marketing"}, confined.Meta.Namespaces)
	s.Require().NotNil(confined.Meta.ExpiresAt)
	s.requireKeyStatus(confined.Plaintext, http.StatusForbidden)
	storedExpiry := s.listedKey(confined.Meta.ID).ExpiresAt
	s.Require().NotNil(storedExpiry)

	rotated := rotate(confined.Meta.ID)
	s.NotEqual(confined.Meta.ID, rotated.Meta.ID)
	s.requireKeyStatus(rotated.Plaintext, http.StatusForbidden)
	stored, ok := storedNamespaces(rotated.Meta.ID)
	s.Require().True(ok, "the rotated key must not store NULL (cluster-wide) namespaces")
	s.JSONEq(`["marketing"]`, stored)
	s.Require().NotNil(rotated.Meta.ExpiresAt, "a key with a TTL must not rotate into one without")
	s.True(rotated.Meta.ExpiresAt.Equal(*storedExpiry),
		"the replacement keeps the remaining lifetime: %s, want %s", rotated.Meta.ExpiresAt, storedExpiry)

	listed := s.listAPIKeysRaw()
	s.Require().Contains(listed, rotated.Meta.ID)
	s.JSONEq(`["marketing"]`, string(listed[rotated.Meta.ID]["namespaces"]),
		"`auth key list` must show the rotated key still confined")

	// Control: an unconfined key rotates to NULL and keeps its cluster role.
	cluster := s.createAPIKeyCLI("--role", "viewer", "--description",
		fmt.Sprintf("auth-lane rotate cluster key %d", time.Now().UnixNano()))
	minted = append(minted, cluster.Meta.ID)
	rotatedCluster := rotate(cluster.Meta.ID)
	_, ok = storedNamespaces(rotatedCluster.Meta.ID)
	s.False(ok, "a NULL-namespaces key must rotate to NULL")
	s.requireKeyStatus(rotatedCluster.Plaintext, http.StatusOK)

	// A key confined to a namespace the served policy does not declare (seeded:
	// minting refuses it) is refused rotation, naming the namespace.
	undeclared := s.createAPIKeyCLI("--role", "viewer", "--namespace", "marketing", "--description",
		fmt.Sprintf("auth-lane rotate undeclared key %d", time.Now().UnixNano()))
	minted = append(minted, undeclared.Meta.ID)
	result, err := catalogDB.ExecContext(s.T().Context(),
		`UPDATE api_keys SET namespaces = ? WHERE id = ?`, `["iaa-a5-undeclared"]`, undeclared.Meta.ID)
	s.Require().NoError(err)
	affected, err := result.RowsAffected()
	s.Require().NoError(err)
	s.Require().Equal(int64(1), affected)
	stdout, stderr, err := s.runCLISeparate("auth", "key", "rotate", "--id", undeclared.Meta.ID, "--server", s.caesiumURL)
	s.Require().Error(err, "rotating a key in an undeclared namespace must fail; stdout: %s", stdout)
	s.Contains(stderr, "(409)")
	s.Contains(stderr, "iaa-a5-undeclared")
	s.Contains(stderr, "not declared in the access policy")
	s.Empty(strings.TrimSpace(stdout))
}

// requireKeyStatus asserts GET /v1/atoms with the given key answers status.
func (s *IntegrationTestSuite) requireKeyStatus(token string, want int) {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/v1/atoms", token, nil)
	s.Require().Equal(want, status, "GET /v1/atoms: %s", body)
}

func (s *IntegrationTestSuite) requireWhoamiRole(token, want string) {
	s.T().Helper()
	status, body := s.requestWithKey(http.MethodGet, "/auth/whoami", token, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var whoami struct {
		Kind string `json:"kind"`
		Role string `json:"role"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &whoami), body)
	s.Equal("api_key", whoami.Kind)
	s.Equal(want, whoami.Role)
}

// listAPIKeysRaw runs `caesium auth key list` and returns each key's raw JSON
// fields by id, so a test can assert which fields are present at all.
func (s *IntegrationTestSuite) listAPIKeysRaw() map[string]map[string]json.RawMessage {
	s.T().Helper()
	out, err := s.runCLIStdout("auth", "key", "list", "--server", s.caesiumURL)
	s.Require().NoError(err, "caesium auth key list failed:\n%s", out)
	var keys []map[string]json.RawMessage
	s.Require().NoError(json.Unmarshal([]byte(out), &keys),
		"auth key list stdout must be exactly one JSON array:\n%s", out)
	byID := make(map[string]map[string]json.RawMessage, len(keys))
	for _, key := range keys {
		var id string
		s.Require().NoError(json.Unmarshal(key["id"], &id))
		byID[id] = key
	}
	return byID
}

// SSO groups the A5 scenarios bind in their policy revision (useKeyPolicy).
// Neither is seeded by any other scenario.
const (
	a5ClusterKeyAdmins   = "iaa-a5-key-admins"
	a5MarketingKeyAdmins = "iaa-a5-mkt-key-admins"
)

// useKeyPolicy serves, for the rest of the scenario, the lane's policy file
// plus the namespaces marketing and finance and the A5 group bindings
// (a5ClusterKeyAdmins → admin at "*", a5MarketingKeyAdmins → admin in
// marketing), reloaded through the CLI. Cleanup restores the original file,
// reloads it and requires the original revision back in force, so the
// scenario holds on whichever policy the lane serves.
func (s *IntegrationTestSuite) useKeyPolicy() {
	s.T().Helper()
	path, original := s.authPolicyFixture()
	served, err := policy.Parse(original)
	s.Require().NoError(err, "the lane's policy file must parse")
	if served.Namespaces == nil {
		served.Namespaces = map[string]policy.Namespace{}
	}
	for _, namespace := range []string{policy.DefaultNamespace, "marketing", "finance"} {
		if _, declared := served.Namespaces[namespace]; !declared {
			served.Namespaces[namespace] = policy.Namespace{}
		}
	}
	served.Bindings = append(served.Bindings,
		policy.Binding{Subjects: policy.Subjects{Groups: []string{a5ClusterKeyAdmins}}, Role: models.RoleAdmin, Namespaces: []string{policy.ClusterNamespace}},
		policy.Binding{Subjects: policy.Subjects{Groups: []string{a5MarketingKeyAdmins}}, Role: models.RoleAdmin, Namespaces: []string{"marketing"}},
	)
	revision, err := yaml.Marshal(served)
	s.Require().NoError(err)

	s.T().Cleanup(func() {
		s.restoreAuthPolicy(path, original)
		if _, stderr, err := s.runCLISeparate("auth", "policy", "reload", "--server", s.caesiumURL); err != nil {
			s.T().Errorf("restore the auth-lane policy revision: %v: %s", err, stderr)
		}
		if !s.waitForPolicyRevision(policy.Hash(original)) {
			s.T().Errorf("the original auth-lane policy revision %s is not back in force", policy.Hash(original))
		}
	})
	s.writeAuthPolicy(path, revision)
	reload := runCLIJSON[policyReloadDoc](s, "auth", "policy", "reload", "--json", "--server", s.caesiumURL)
	s.Require().Equal(policy.Hash(revision), reload.Revision, "the A5 policy revision must be in force")
}

// whoamiGrantsDoc is the GET /auth/whoami body with the A5 grant fields.
type whoamiGrantsDoc struct {
	Kind        string            `json:"kind"`
	Subject     string            `json:"subject"`
	Role        string            `json:"role"`
	ClusterRole string            `json:"cluster_role"`
	Grants      map[string]string `json:"grants"`
	Namespaces  []string          `json:"namespaces"`
	CSRFToken   string            `json:"csrf_token"`
}

func decodeWhoami(s *IntegrationTestSuite, status int, body string) whoamiGrantsDoc {
	s.T().Helper()
	s.Require().Equal(http.StatusOK, status, body)
	var doc whoamiGrantsDoc
	s.Require().NoError(json.Unmarshal([]byte(body), &doc), body)
	return doc
}

// listedKey returns key id as `caesium auth key list` reports it (the stored
// values, read back from the catalog).
func (s *IntegrationTestSuite) listedKey(id string) apiKeyMetadata {
	s.T().Helper()
	for _, key := range runCLIJSON[[]apiKeyMetadata](s, "auth", "key", "list", "--server", s.caesiumURL) {
		if key.ID == id {
			return key
		}
	}
	s.Require().Failf("key not listed", "key %s is not in `caesium auth key list`", id)
	return apiKeyMetadata{}
}

// keyAuditRows reads `caesium auth audit` (always JSON) as the suite's admin,
// stdout captured on its own.
func (s *IntegrationTestSuite) keyAuditRows(action, actor string) []auditEntry {
	s.T().Helper()
	return runCLIJSON[[]auditEntry](s, "auth", "audit", "--action", action, "--actor", actor, "--limit", "100", "--server", s.caesiumURL)
}

// TestAuthNamespacedKeyMintingCLI drives namespaced key minting and
// administration through the `caesium auth key` binary: a namespace admin
// key mints, lists, rotates and revokes keys confined to its namespace, is
// refused (403, audited) anything wider, and the minted key's grants hold on
// real namespaced routes. whoami is asserted over REST: `caesium whoami` is
// plan item E5.
func (s *IntegrationTestSuite) TestAuthNamespacedKeyMintingCLI() {
	s.requireAuthLane()
	s.useKeyPolicy()
	f := s.newNamespacedRoutesFixture("a5")
	defer s.closeNamespacedRoutesFixture(f)
	describe := func(what string) string { return fmt.Sprintf("a5 %s %d", what, f.stamp) }

	mktAdmin := s.createAPIKeyCLI("--role", "admin", "--namespace", "marketing", "--description", describe("marketing admin"))
	f.keys = append(f.keys, mktAdmin.Meta.ID)
	s.Equal([]string{"marketing"}, mktAdmin.Meta.Namespaces)
	finRunner := s.createAPIKeyCLI("--role", "runner", "--namespace", "finance", "--jobs", f.finAlias, "--description", describe("finance runner"))
	f.keys = append(f.keys, finRunner.Meta.ID)
	s.Equal([]string{"finance"}, finRunner.Meta.Namespaces)
	s.Require().NotNil(finRunner.Meta.Scope, "--jobs must scope the key")
	s.Equal([]string{f.finAlias}, finRunner.Meta.Scope.Jobs)

	asMarketingAdmin := []string{"CAESIUM_API_KEY=" + mktAdmin.Plaintext}
	keyCLI := func(args ...string) (string, string, error) {
		return s.runCLIWithEnv(asMarketingAdmin, append(append([]string{"auth", "key"}, args...), "--server", s.caesiumURL)...)
	}

	// Within its grants: a marketing operator key with a TTL.
	stdout, stderr, err := keyCLI("create", "--role", "operator", "--namespace", "marketing", "--expires", "2h", "--description", describe("marketing operator"))
	s.Require().NoError(err, "stderr: %s", stderr)
	mktOperator := s.parseCreatedKey(stdout)
	f.keys = append(f.keys, mktOperator.Meta.ID)
	s.Equal([]string{"marketing"}, mktOperator.Meta.Namespaces)
	s.Equal(mktAdmin.Meta.KeyPrefix, mktOperator.Meta.CreatedBy)
	s.Require().NotNil(mktOperator.Meta.ExpiresAt)

	// The minted key holds operator in marketing and nothing in finance.
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/v1/jobs/"+f.mktJob.ID+"/dag", mktOperator.Plaintext, "")
	s.requireNSStatus(http.StatusForbidden, http.MethodGet, "/v1/jobs/"+f.finJob.ID+"/dag", mktOperator.Plaintext, "")

	// Wider than its grants: refused with 403, and each refusal is audited.
	for _, args := range [][]string{
		{"--role", "operator", "--namespace", "finance"},
		{"--role", "viewer"},
		{"--role", "viewer", "--namespace", "marketing,finance"},
	} {
		stdout, stderr, err := keyCLI(append([]string{"create", "--description", describe("refused")}, args...)...)
		s.Require().Error(err, "%v must be refused; stdout: %s", args, stdout)
		s.Contains(stderr, "(403)", "%v", args)
		s.Empty(strings.TrimSpace(stdout), "a refused mint prints no key")
	}
	var deniedNamespaces []string
	for _, row := range s.keyAuditRows("auth.denied", mktAdmin.Meta.KeyPrefix) {
		var meta map[string]any
		s.Require().NoError(json.Unmarshal(row.Metadata, &meta))
		if meta["policy_key"] == "POST /v1/auth/keys" {
			deniedNamespaces = append(deniedNamespaces, row.Namespace)
		}
	}
	s.ElementsMatch([]string{"finance", "*", "*"}, deniedNamespaces, "one audited denial per refused mint")

	// Undeclared or malformed namespaces are refused before anything else.
	for _, ns := range []string{"iaa-a5-nowhere", "Bad_NS"} {
		stdout, stderr, err := s.runCLISeparate("auth", "key", "create", "--role", "viewer", "--namespace", ns, "--server", s.caesiumURL)
		s.Require().Error(err, "--namespace %s must be refused; stdout: %s", ns, stdout)
		s.Contains(stderr, "(400)")
		s.Contains(stderr, ns)
	}
	// An empty --namespace (say, an unset "$NS") or --jobs never widens the
	// key to cluster-wide or unscoped: no credential is issued.
	emptyDescription := describe("empty flag")
	for _, args := range [][]string{
		{"--role", "admin", "--namespace", ""},
		{"--role", "runner", "--namespace", "marketing", "--jobs", ""},
		{"--role", "runner", "--namespace", "marketing", "--jobs", ","},
		{"--role", "runner", "--namespace", "marketing", "--scope-jobs", " "},
	} {
		stdout, stderr, err := s.runCLISeparate(append([]string{"auth", "key", "create", "--description", emptyDescription, "--server", s.caesiumURL}, args...)...)
		s.Require().Error(err, "%v must be refused; stdout: %s", args, stdout)
		s.Empty(strings.TrimSpace(stdout), "%v: no key on stdout (stderr: %s)", args, stderr)
	}
	for _, key := range runCLIJSON[[]apiKeyMetadata](s, "auth", "key", "list", "--server", s.caesiumURL) {
		s.NotEqual(emptyDescription, key.Description, "an empty flag must not mint key %s", key.ID)
	}

	// The marketing admin's list holds marketing keys only; the cluster admin
	// sees them all.
	stdout, stderr, err = keyCLI("list")
	s.Require().NoError(err, "stderr: %s", stderr)
	var mktListed []apiKeyMetadata
	s.Require().NoError(decodeCLIJSONStdout([]byte(stdout), &mktListed))
	s.True(containsAPIKeyID(mktListed, mktAdmin.Meta.ID))
	s.True(containsAPIKeyID(mktListed, mktOperator.Meta.ID))
	s.False(containsAPIKeyID(mktListed, finRunner.Meta.ID))
	for _, key := range mktListed {
		s.Equal([]string{"marketing"}, key.Namespaces, "key %s is not confined to marketing", key.ID)
	}
	allListed := runCLIJSON[[]apiKeyMetadata](s, "auth", "key", "list", "--server", s.caesiumURL)
	s.True(containsAPIKeyID(allListed, finRunner.Meta.ID))
	s.True(containsAPIKeyID(allListed, mktOperator.Meta.ID))

	// Rotate within marketing: the replacement keeps namespaces and expiry.
	storedExpiry := s.listedKey(mktOperator.Meta.ID).ExpiresAt
	s.Require().NotNil(storedExpiry)
	stdout, stderr, err = keyCLI("rotate", "--id", mktOperator.Meta.ID, "--grace-period", "1m")
	s.Require().NoError(err, "stderr: %s", stderr)
	rotated := s.parseCreatedKey(stdout)
	s.Equal([]string{"marketing"}, rotated.Meta.Namespaces)
	s.Require().NotNil(rotated.Meta.ExpiresAt)
	s.True(rotated.Meta.ExpiresAt.Equal(*storedExpiry), "rotated expiry %s, want %s", rotated.Meta.ExpiresAt, storedExpiry)
	s.Equal(mktAdmin.Meta.KeyPrefix, rotated.Meta.CreatedBy)

	// Another namespace's key: neither rotated nor revoked.
	for _, verb := range []string{"rotate", "revoke"} {
		stdout, stderr, err := keyCLI(verb, "--id", finRunner.Meta.ID)
		s.Require().Error(err, "%s of the finance key must be refused; stdout: %s", verb, stdout)
		s.Contains(stderr, "(403)", verb)
	}
	s.requireNSStatus(http.StatusOK, http.MethodGet, "/auth/whoami", finRunner.Plaintext, "")

	// Revoke the replacement: it stops authenticating.
	stdout, stderr, err = keyCLI("revoke", "--id", rotated.Meta.ID)
	s.Require().NoError(err, "stdout: %s stderr: %s", stdout, stderr)
	status, body := s.requestWithKey(http.MethodGet, "/auth/whoami", rotated.Plaintext, nil)
	s.Equal(http.StatusUnauthorized, status, body)

	// whoami reports the grants; role stays the cluster role.
	status, body = s.requestWithKey(http.MethodGet, "/auth/whoami", mktAdmin.Plaintext, nil)
	who := decodeWhoami(s, status, body)
	s.Equal("", who.Role)
	s.Equal("", who.ClusterRole)
	s.Equal(map[string]string{"marketing": "admin"}, who.Grants)
	s.Equal([]string{"marketing"}, who.Namespaces)
	status, body = s.requestWithKey(http.MethodGet, "/auth/whoami", s.authAPIKey, nil)
	who = decodeWhoami(s, status, body)
	s.Equal("admin", who.Role)
	s.Equal("admin", who.ClusterRole)
	s.Equal(map[string]string{"*": "admin"}, who.Grants)
	s.Equal([]string{"*"}, who.Namespaces)
}

// cookieJSONRequest sends a request with a session cookie, the CSRF header
// when csrf is set, and an optional JSON body.
func (s *IntegrationTestSuite) cookieJSONRequest(method, path string, cookie *http.Cookie, csrf, body string) (int, string) {
	s.T().Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(s.T().Context(), method, s.caesiumURL+path, reader)
	s.Require().NoError(err)
	req.AddCookie(cookie)
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(out)
}

// TestAuthSSOAdminManagesKeys: SSO users whose policy grant is admin manage
// API keys with their browser session (before A5 the key handlers read only
// an API-key caller and answered 401), the key records them as its creator,
// CSRF still guards the unsafe requests, and a namespace admin is confined to
// its namespace.
func (s *IntegrationTestSuite) TestAuthSSOAdminManagesKeys() {
	s.requireAuthLane()
	s.useKeyPolicy()
	var active []string
	defer func() {
		for _, id := range active {
			out, err := s.runCLIStdout("auth", "key", "revoke", "--id", id, "--server", s.caesiumURL)
			s.NoError(err, "caesium auth key revoke %s failed:\n%s", id, out)
		}
	}()
	created := func(status int, body string) apiKeyMetadata {
		s.T().Helper()
		s.Require().Equal(http.StatusCreated, status, body)
		key := s.parseCreatedKey(body)
		return key.Meta
	}

	admin := s.seedPolicySessionUser(a5ClusterKeyAdmins)
	status, body := s.cookieJSONRequest(http.MethodGet, "/auth/whoami", admin, "", "")
	adminWho := decodeWhoami(s, status, body)
	s.Equal("user", adminWho.Kind)
	s.Equal("admin", adminWho.Role)
	s.Equal(map[string]string{"*": "admin"}, adminWho.Grants)
	s.Equal([]string{"*"}, adminWho.Namespaces)
	s.Require().NotEmpty(adminWho.CSRFToken)

	clusterKey := created(s.cookieJSONRequest(http.MethodPost, "/v1/auth/keys", admin, adminWho.CSRFToken,
		`{"role":"runner","description":"a5 sso cluster key"}`))
	active = append(active, clusterKey.ID)
	s.Equal(adminWho.Subject, clusterKey.CreatedBy, "the key records the SSO user as its creator")
	s.Empty(clusterKey.Namespaces)
	rows := s.keyAuditRows("api_key.create", adminWho.Subject)
	s.Require().NotEmpty(rows, "the mint is audited with the user as actor")
	s.True(auditContainsResource(rows, clusterKey.ID))

	status, body = s.cookieJSONRequest(http.MethodPost, "/v1/auth/keys", admin, "", `{"role":"runner"}`)
	s.Equal(http.StatusForbidden, status, "a cookie POST without the CSRF token is refused: %s", body)

	mkt := s.seedPolicySessionUser(a5MarketingKeyAdmins)
	status, body = s.cookieJSONRequest(http.MethodGet, "/auth/whoami", mkt, "", "")
	mktWho := decodeWhoami(s, status, body)
	s.Equal("", mktWho.Role)
	s.Equal(map[string]string{"marketing": "admin"}, mktWho.Grants)
	s.Equal([]string{"marketing"}, mktWho.Namespaces)

	mktKey := created(s.cookieJSONRequest(http.MethodPost, "/v1/auth/keys", mkt, mktWho.CSRFToken,
		`{"role":"operator","namespaces":["marketing"],"description":"a5 sso marketing key"}`))
	s.Equal([]string{"marketing"}, mktKey.Namespaces)
	s.Equal(mktWho.Subject, mktKey.CreatedBy)
	for _, body := range []string{`{"role":"operator","namespaces":["finance"]}`, `{"role":"viewer"}`} {
		status, out := s.cookieJSONRequest(http.MethodPost, "/v1/auth/keys", mkt, mktWho.CSRFToken, body)
		s.Equal(http.StatusForbidden, status, "%s: %s", body, out)
	}

	status, body = s.cookieJSONRequest(http.MethodGet, "/v1/auth/keys", mkt, "", "")
	s.Require().Equal(http.StatusOK, status, body)
	var listed []apiKeyMetadata
	s.Require().NoError(json.Unmarshal([]byte(body), &listed))
	s.True(containsAPIKeyID(listed, mktKey.ID))
	s.False(containsAPIKeyID(listed, clusterKey.ID), "a namespace admin never sees a cluster key")
	for _, key := range listed {
		s.Equal([]string{"marketing"}, key.Namespaces)
	}

	status, body = s.cookieJSONRequest(http.MethodPost, "/v1/auth/keys/"+clusterKey.ID+"/revoke", mkt, mktWho.CSRFToken, "")
	s.Equal(http.StatusForbidden, status, body)
	status, body = s.cookieJSONRequest(http.MethodPost, "/v1/auth/keys/"+mktKey.ID+"/revoke", mkt, mktWho.CSRFToken, "")
	s.Equal(http.StatusOK, status, body)
	rows = s.keyAuditRows("api_key.revoke", mktWho.Subject)
	s.True(auditContainsResource(rows, mktKey.ID), "the revoke is audited with the user as actor")
}

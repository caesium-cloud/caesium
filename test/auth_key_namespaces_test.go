//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TestAuthKeyNamespacesGrantResolution drives API-key grant resolution through
// the live auth lane: a key minted today has NULL namespaces and keeps its role
// cluster-wide, and the stored namespaces decide its grants on every request.
//
// This is a seeded-state scenario until key minting accepts namespaces: the
// UPDATEs below stand in for `caesium auth key create --namespace`, which
// replaces them when that flag lands.
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
// confined key and proves the replacement secret stays confined: rotation
// must never turn a namespace-confined key into a cluster key. A NULL key
// rotates to NULL, so ordinary keys keep their cluster role.
func (s *IntegrationTestSuite) TestAuthKeyRotationKeepsNamespaces() {
	s.requireAuthLane()

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

	confined := s.createAPIKeyCLI("--role", "viewer", "--description",
		fmt.Sprintf("auth-lane rotate confined key %d", time.Now().UnixNano()))
	minted = append(minted, confined.Meta.ID)
	result, err := catalogDB.ExecContext(s.T().Context(),
		`UPDATE api_keys SET namespaces = ? WHERE id = ?`, `["marketing"]`, confined.Meta.ID)
	s.Require().NoError(err)
	affected, err := result.RowsAffected()
	s.Require().NoError(err)
	s.Require().Equal(int64(1), affected)
	s.requireKeyStatus(confined.Plaintext, http.StatusForbidden)

	rotated := rotate(confined.Meta.ID)
	s.NotEqual(confined.Meta.ID, rotated.Meta.ID)
	s.requireKeyStatus(rotated.Plaintext, http.StatusForbidden)
	stored, ok := storedNamespaces(rotated.Meta.ID)
	s.Require().True(ok, "the rotated key must not store NULL (cluster-wide) namespaces")
	s.JSONEq(`["marketing"]`, stored)

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

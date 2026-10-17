//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"time"
)

// mintNamespacedKey mints an API key holding role in each of namespaces (no
// namespaces: role cluster-wide) and revokes it when the scenario ends (plan
// item H-1).
//
// The key is minted through the real `caesium auth key create`; until A5's
// `--namespace` flag lands, its namespaces column is then seeded in the
// catalog, exactly as A3's namespacedKey does. Seeding produces the
// precondition only: every assertion still goes through the server.
func (s *IntegrationTestSuite) mintNamespacedKey(role string, namespaces ...string) createdAPIKey {
	s.T().Helper()

	key := s.createAPIKeyCLI("--role", role, "--description",
		fmt.Sprintf("h1 %s %v %d", role, namespaces, time.Now().UnixNano()))
	s.T().Cleanup(func() {
		if out, err := s.runCLIStdout("auth", "key", "revoke", "--id", key.Meta.ID, "--server", s.caesiumURL); err != nil {
			s.T().Errorf("revoke namespaced key %s: %v\n%s", key.Meta.ID, err, out)
		}
	})
	if len(namespaces) == 0 {
		return key
	}

	encoded, err := json.Marshal(namespaces)
	s.Require().NoError(err)
	catalog := s.openIntegrationCatalogDB()
	defer func() { s.NoError(catalog.Close()) }()
	result, err := catalog.ExecContext(s.T().Context(), `UPDATE api_keys SET namespaces = ? WHERE id = ?`, string(encoded), key.Meta.ID)
	s.Require().NoError(err)
	affected, err := result.RowsAffected()
	s.Require().NoError(err)
	s.Require().Equal(int64(1), affected, "seed the namespaces of key %s", key.Meta.ID)
	return key
}

//go:build integration

package test

import (
	"fmt"
	"time"
)

// mintNamespacedKey mints an API key holding role in each of namespaces (no
// namespaces: role cluster-wide) and revokes it when the scenario ends (plan
// item H-1).
//
// The key is minted through the real `caesium auth key create --namespace`
// (A5), as the admin key the lane exports, so the namespaces are stored and
// validated by the server exactly as an operator's mint would be.
func (s *IntegrationTestSuite) mintNamespacedKey(role string, namespaces ...string) createdAPIKey {
	s.T().Helper()

	args := []string{"--role", role, "--description", fmt.Sprintf("h1 %s %v %d", role, namespaces, time.Now().UnixNano())}
	for _, namespace := range namespaces {
		args = append(args, "--namespace", namespace)
	}
	key := s.createAPIKeyCLI(args...)
	s.T().Cleanup(func() {
		if out, err := s.runCLIStdout("auth", "key", "revoke", "--id", key.Meta.ID, "--server", s.caesiumURL); err != nil {
			s.T().Errorf("revoke namespaced key %s: %v\n%s", key.Meta.ID, err, out)
		}
	})
	if len(namespaces) > 0 {
		s.Require().ElementsMatch(namespaces, key.Meta.Namespaces, "the minted key holds %s in %v", role, namespaces)
	}
	return key
}

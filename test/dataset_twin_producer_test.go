//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

const multipleProducersMessage = "dataset produced by multiple jobs"

// TestAuthDatasetCanonicalTwinOfLegacyProducerIsASecondProducer
// (identity-and-access W7-ε): a finance job stored as "<alias> " (before
// applies refused surrounding whitespace) produces a dataset; its declarations
// store the trimmed alias. The canonical "<alias>" is a different job, so
// applying it with the same output is a second producer: server lint (HTTP and
// `caesium job lint --server --json`) reports it, and apply refuses it with
// nothing written, for the cluster admin and a marketing operator alike; the
// operator's errors name nothing of finance. Keying the declared graph on the
// trimmed alias used to fold the two jobs into one producer.
func (s *IntegrationTestSuite) TestAuthDatasetCanonicalTwinOfLegacyProducerIsASecondProducer() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	canonical := fmt.Sprintf("w7e-payroll-%d", stamp)
	legacy := canonical + " "
	seed := canonical + "-seed"
	ledger := fmt.Sprintf("w7e-ledger-%d", stamp)
	s.b9Cleanup(seed, legacy, canonical)

	// Precondition: the legacy producer, applied under a seed alias and
	// renamed in the catalog (every writer now refuses the legacy alias).
	status, body := s.b9Apply(s.authAPIKey, false, s.datasetJob(seed, "finance", nil, []schema.ProducedDataset{{Name: ledger}}))
	s.Require().Equal(http.StatusOK, status, body)
	catalog := s.openIntegrationCatalogDB()
	defer func() { s.NoError(catalog.Close()) }()
	for _, stmt := range []struct{ query, to string }{
		{"UPDATE jobs SET alias = ? WHERE alias = ?", legacy},
		// As BuildDeclarations stores a legacy job's rows: its alias trimmed.
		{"UPDATE dataset_declarations SET job_alias = ? WHERE job_alias = ?", canonical},
	} {
		result, err := catalog.ExecContext(s.T().Context(), stmt.query, stmt.to, seed)
		s.Require().NoError(err, stmt.query)
		affected, err := result.RowsAffected()
		s.Require().NoError(err)
		s.Require().Positive(affected, stmt.query)
	}
	s.Require().NotNil(s.b9Job(legacy))

	twin := s.datasetJob(canonical, "marketing", nil, []schema.ProducedDataset{{Name: ledger}})
	dir := s.writeNamespaceManifests(twin)
	operator := s.mintNamespacedKey("operator", "marketing").Plaintext

	for _, caller := range []struct {
		name, key string
		confined  bool
	}{
		{"cluster admin", s.authAPIKey, false},
		{"marketing operator", operator, true},
	} {
		lint := strings.Join(s.serverLint(caller.key, twin).messages(), "\n")
		s.Contains(lint, multipleProducersMessage, "%s: HTTP lint", caller.name)

		stdout, stderr, err := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + caller.key},
			"job", "lint", "--path", dir, "--server", s.caesiumURL, "--json")
		s.Require().Error(err, "%s: lint must fail: %s", caller.name, stdout)
		var cli jobdefLintResult
		s.Require().NoError(json.Unmarshal([]byte(stdout), &cli), "%s: stdout must be clean JSON: %s\nstderr: %s", caller.name, stdout, stderr)
		cliLint := strings.Join(cli.messages(), "\n")
		s.Contains(cliLint, multipleProducersMessage, "%s: CLI lint", caller.name)

		status, body := s.b9Apply(caller.key, false, twin)
		s.NotEqual(http.StatusOK, status, "%s: apply: %s", caller.name, body)
		s.Contains(body, multipleProducersMessage, "%s: apply", caller.name)
		s.Nil(s.b9Job(canonical), "%s: the refused twin was not written", caller.name)
		s.NotNil(s.b9Job(legacy), "%s: the legacy producer is untouched", caller.name)

		if caller.confined {
			for _, out := range []string{lint, cliLint, body} {
				s.NotContains(out, "finance", "%s: no foreign namespace", caller.name)
				s.NotContains(out, seed)
			}
		}
	}
}

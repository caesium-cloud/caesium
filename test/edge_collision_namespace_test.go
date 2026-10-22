//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// TestAuthJobdefSurfacesWithholdCollidingForeignFinding (identity-and-access
// W7-ε): derived contract edge ids are plain concatenations, so marketing's
// edge P -> "C:/z" on dataset D and finance's edge P -> "C" on "z:/D" share an
// id and derivation merges finance's findings into the marketing edge. P (in
// marketing) drops a key only finance's consumer requires: a breaking change
// whose finding names the key. The cluster admin reads it from lint, diff and
// the refused apply; a marketing operator gets lint and diff without the
// merged edge and an apply refusal that only counts the withheld finding.
func (s *IntegrationTestSuite) TestAuthJobdefSurfacesWithholdCollidingForeignFinding() {
	s.requireAuthLane()
	stamp := time.Now().UnixNano()
	producer := fmt.Sprintf("w7e-cp-%d", stamp)
	financeConsumer := fmt.Sprintf("w7e-cc-%d", stamp)
	marketingConsumer := financeConsumer + ":/z"
	dataset := fmt.Sprintf("w7e-pub-%d", stamp)
	financeKey := fmt.Sprintf("w7efin_dep_%d", stamp)
	s.cleanupInOrder(marketingConsumer, financeConsumer, producer)

	shared := func(keys ...string) map[string]any {
		properties := map[string]any{"other": map[string]any{"type": "string"}}
		out := map[string]any{"type": "object", "properties": properties}
		if len(keys) == 0 {
			return out
		}
		required := make([]any, 0, len(keys))
		for _, key := range keys {
			properties[key] = map[string]any{"type": "string"}
			required = append(required, key)
		}
		out["required"] = required
		return out
	}
	producing := func(keys ...string) schema.Definition {
		return s.datasetJob(producer, "marketing", nil, []schema.ProducedDataset{
			{Name: dataset}, {Name: "z:/" + dataset, Schema: shared(keys...)},
		})
	}
	s.applyDefinition(producing(financeKey))
	s.applyDefinition(s.datasetJob(marketingConsumer, "marketing", []schema.ConsumedDataset{{Name: dataset}}, nil))
	s.applyDefinition(s.datasetJob(financeConsumer, "finance",
		[]schema.ConsumedDataset{{Name: "z:/" + dataset, Schema: shared(financeKey)}}, nil))

	breaking := producing()
	// Dataset schemas are not part of the diff's job spec; a command change
	// makes the producer a modified entry.
	breaking.Steps[0].Command = []string{"sh", "-c", "echo w7e-changed"}
	dir := s.writeNamespaceManifests(breaking)
	operator := s.mintNamespacedKey("operator", "marketing").Plaintext

	lint := func(key string) (contractCLILintResponse, string) {
		s.T().Helper()
		stdout, stderr, _ := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + key},
			"job", "lint", "--path", dir, "--server", s.caesiumURL, "--json")
		var out contractCLILintResponse
		s.Require().NoError(json.Unmarshal([]byte(stdout), &out), "stdout must be clean JSON: %s\nstderr: %s", stdout, stderr)
		s.Require().NotNil(out.Contracts, stdout)
		return out, stdout
	}
	diff := func(key string) string {
		s.T().Helper()
		stdout, stderr, _ := s.runCLIWithEnv([]string{"CAESIUM_API_KEY=" + key},
			"job", "diff", "--path", dir, "--json", "--server", s.caesiumURL)
		var out struct {
			Modified []struct {
				Alias            string          `json:"alias"`
				ContractFindings json.RawMessage `json:"contractFindings"`
			} `json:"modified"`
		}
		s.Require().NoError(json.Unmarshal([]byte(stdout), &out), "stdout must be clean JSON: %s\nstderr: %s", stdout, stderr)
		s.Require().Len(out.Modified, 1, stdout)
		s.Equal(producer, out.Modified[0].Alias, stdout)
		// The definition diff itself shows the key P drops; only the
		// contract findings are the server's to withhold.
		return string(out.Modified[0].ContractFindings)
	}
	apply := func(key string) (refusal struct {
		Findings         []json.RawMessage `json:"findings"`
		WithheldFindings int               `json:"withheld_findings"`
		Message          string            `json:"message"`
	}, body string) {
		s.T().Helper()
		var status int
		status, body = s.jobdefRequest("/v1/jobdefs/apply", key, map[string]any{"definitions": []schema.Definition{breaking}})
		s.Require().Equal(http.StatusConflict, status, body)
		s.Require().NoError(json.Unmarshal([]byte(body), &refusal), body)
		return refusal, body
	}

	// The cluster admin first: every surface reports finance's finding.
	adminLint, adminLintOut := lint(s.authAPIKey)
	s.NotEmpty(adminLint.Contracts.Breaking, adminLintOut)
	s.Contains(adminLintOut, financeKey)
	s.Contains(diff(s.authAPIKey), financeKey)
	adminRefusal, adminBody := apply(s.authAPIKey)
	s.NotEmpty(adminRefusal.Findings, adminBody)
	s.Zero(adminRefusal.WithheldFindings, adminBody)
	s.Contains(adminBody, financeKey)

	// The marketing operator: the merged edge is withheld whole.
	mktLint, mktLintOut := lint(operator)
	s.Empty(mktLint.Contracts.Breaking, mktLintOut)
	s.NotContains(mktLintOut, financeKey)
	s.NotContains(diff(operator), financeKey)
	mktRefusal, mktBody := apply(operator)
	s.Empty(mktRefusal.Findings, mktBody)
	s.Positive(mktRefusal.WithheldFindings, mktBody)
	s.Contains(mktRefusal.Message, "a contract with a job outside your namespaces is affected")
	s.NotContains(mktBody, financeKey)
	s.NotContains(mktBody, financeConsumer)
}

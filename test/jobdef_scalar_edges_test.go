//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gopkg.in/yaml.v3"
)

// The console's YAML-to-JSON conversion preserves scalar edge values. Drive
// lint, diff and apply with that shape, then verify the persisted DAG edges.
func (s *IntegrationTestSuite) TestJobdefJSONAcceptsDocumentedScalarEdges() {
	for _, filename := range []string{"fanout-join.job.yaml", "incremental-cache.job.yaml", "task-outputs.job.yaml"} {
		s.Run(filename, func() {
			source, err := os.ReadFile(filepath.Join(s.projectRoot, "docs", "examples", filename))
			s.Require().NoError(err)
			canonical, err := schema.Parse(source)
			s.Require().NoError(err)
			var definition map[string]any
			s.Require().NoError(yaml.Unmarshal(source, &definition))
			alias := fmt.Sprintf("scalar-edges-%d", time.Now().UnixNano())
			definition["metadata"].(map[string]any)["alias"] = alias
			trigger := definition["trigger"].(map[string]any)
			switch trigger["type"] {
			case "http":
				trigger["configuration"].(map[string]any)["path"] = "/hooks/scalar-edges/" + alias
			case "cron":
				trigger["configuration"].(map[string]any)["cron"] = "0 0 1 1 *"
			}
			payload, err := json.Marshal(map[string]any{"definitions": []any{definition}})
			s.Require().NoError(err)
			for _, route := range []string{"lint", "diff", "apply"} {
				resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/"+route, bytes.NewReader(payload))
				s.Require().NoError(err)
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				s.Require().NoError(err)
				s.Require().Equal(http.StatusOK, resp.StatusCode, "%s: %s", route, body)
				if route == "lint" {
					var lint struct {
						Errors []json.RawMessage `json:"errors"`
					}
					s.Require().NoError(json.Unmarshal(body, &lint))
					s.Require().Empty(lint.Errors, "%s", body)
				}
			}
			job := s.requireJobByAlias(alias)
			body, _ := s.getRaw(fmt.Sprintf("/v1/jobs/%s/manifest?format=json", job.ID))
			var persisted schema.Definition
			s.Require().NoError(json.Unmarshal(body, &persisted))
			s.Require().Len(persisted.Steps, len(canonical.Steps))
			// Export reconstructs the graph using next edges; compare its meaning
			// with the documented scalar/list next and dependsOn forms.
			s.Equal(jobdefEdgePairs(*canonical), jobdefEdgePairs(persisted))
		})
	}
}

func jobdefEdgePairs(def schema.Definition) map[string]struct{} {
	edges := make(map[string]struct{})
	for _, step := range def.Steps {
		for _, next := range step.Next {
			edges[step.Name+"->"+next] = struct{}{}
		}
		for _, previous := range step.DependsOn {
			edges[previous+"->"+step.Name] = struct{}{}
		}
	}
	return edges
}

func (s *IntegrationTestSuite) TestJobdefJSONScalarEdgesRejectInvalidWithFieldName() {
	for _, field := range []string{"next", "dependsOn"} {
		s.Run(field, func() {
			payload := fmt.Sprintf(`{"definitions":[{
				"apiVersion":"v1", "kind":"Job", "metadata":{"alias":"invalid-scalar-edge"},
				"trigger":{"type":"cron","configuration":{"cron":"0 0 1 1 *"}},
				"steps":[{"name":"consumer","image":"alpine:3.23","%s":42}]
			}]}`, field)
			resp, err := s.doJSONRequest(http.MethodPost, s.caesiumURL+"/v1/jobdefs/lint", bytes.NewBufferString(payload))
			s.Require().NoError(err)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			s.Require().NoError(err)
			s.Require().Equal(http.StatusOK, resp.StatusCode, "%s", body)
			s.Contains(string(body), "step consumer "+field+": expected string or list")
			s.NotContains(string(body), "rawStep")
		})
	}
}

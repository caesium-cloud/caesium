package jobdef

import (
	"net/http"
	"sort"
	"strings"
	"fmt"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/labstack/echo/v5"
)

type LintRequest struct {
	Definitions []schema.Definition `json:"definitions"`
}

type LintMessage struct {
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
}

type LintSummary struct {
	Steps string `json:"steps"`
}

type LintResponse struct {
	Errors   []LintMessage `json:"errors"`
	Warnings []LintMessage `json:"warnings"`
	Summary  LintSummary   `json:"summary"`
}

func Lint(c *echo.Context) error {
	var req LintRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusOK, LintResponse{
			Errors: []LintMessage{{Message: "Invalid JSON format: " + err.Error()}},
		})
	}

	if len(req.Definitions) == 0 {
		return c.JSON(http.StatusOK, LintResponse{
			Errors: []LintMessage{{Message: "No job definitions found"}},
		})
	}

	resp := LintResponse{
		Errors:   make([]LintMessage, 0),
		Warnings: make([]LintMessage, 0),
	}

	for _, def := range req.Definitions {
		if err := def.Validate(); err != nil {
			resp.Errors = append(resp.Errors, LintMessage{Message: err.Error()})
		}
	}

	var stepsSummary string
	if len(resp.Errors) == 0 {
		var allSteps []schema.Step
		for _, def := range req.Definitions {
			allSteps = append(allSteps, def.Steps...)
		}
		stepsSummary = contractSummary(allSteps)
	}
	resp.Summary.Steps = stepsSummary

	return c.JSON(http.StatusOK, resp)
}

func contractSummary(steps []schema.Step) string {
	type contract struct {
		producer string
		consumer string
		keys     []string
	}
	var contracts []contract

	for _, step := range steps {
		if step.InputSchema == nil {
			continue
		}
		for producerName, stepSchema := range step.InputSchema {
			var keys []string
			if req, ok := stepSchema["required"].([]any); ok {
				for _, k := range req {
					if s, ok := k.(string); ok {
						keys = append(keys, s)
					}
				}
			}
			sort.Strings(keys)
			contracts = append(contracts, contract{
				producer: producerName,
				consumer: step.Name,
				keys:     keys,
			})
		}
	}

	if len(contracts) == 0 {
		return ""
	}

	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].producer != contracts[j].producer {
			return contracts[i].producer < contracts[j].producer
		}
		if contracts[i].consumer != contracts[j].consumer {
			return contracts[i].consumer < contracts[j].consumer
		}
		return strings.Join(contracts[i].keys, "\x00") < strings.Join(contracts[j].keys, "\x00")
	})

	parts := make([]string, 0, len(contracts))
	for _, c := range contracts {
		if len(c.keys) > 0 {
			parts = append(parts, fmt.Sprintf("%s \u2192 %s: %s", c.producer, c.consumer, strings.Join(c.keys, ", ")))
		} else {
			parts = append(parts, fmt.Sprintf("%s \u2192 %s", c.producer, c.consumer))
		}
	}

	n := len(contracts)
	noun := "data contract"
	if n != 1 {
		noun = "data contracts"
	}
	return fmt.Sprintf("%d %s (%s)", n, noun, strings.Join(parts, "; "))
}

package report

import (
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/suite"
)

type ReportSuite struct {
	suite.Suite
}

func TestReportSuite(t *testing.T) {
	suite.Run(t, new(ReportSuite))
}

func (s *ReportSuite) TestAnalyzeSummary() {
	defs := []schema.Definition{
		{
			APIVersion: schema.APIVersionV1,
			Kind:       schema.KindJob,
			Metadata:   schema.Metadata{Alias: "a"},
			Trigger:    schema.Trigger{Type: schema.TriggerCron, Configuration: map[string]any{}},
			Steps:      []schema.Step{{Name: "one", Engine: schema.EngineDocker, Image: "img"}},
		},
		{
			APIVersion: schema.APIVersionV1,
			Kind:       schema.KindJob,
			Metadata:   schema.Metadata{},
			Trigger:    schema.Trigger{Type: schema.TriggerHTTP, Configuration: map[string]any{}},
			Steps:      []schema.Step{{Name: "two", Engine: schema.EngineKubernetes, Image: "img2"}},
			Callbacks:  []schema.Callback{{Type: schema.CallbackNotification}},
		},
	}

	summary := Analyze(defs)
	s.Equal(2, summary.Total)
	s.Equal(1, summary.TriggerTypes[schema.TriggerCron])
	s.Equal(1, summary.TriggerTypes[schema.TriggerHTTP])
	s.Equal(1, summary.Engines[schema.EngineDocker])
	s.Equal(1, summary.Engines[schema.EngineKubernetes])
	s.Equal(1, summary.CallbackTypes[schema.CallbackNotification])
	s.Equal([]string{"definition[1]"}, summary.MissingAliases)
}

func (s *ReportSuite) TestMarkdownGeneration() {
	md := Markdown()
	s.Contains(md, "# Job Definition Schema")
	s.Contains(md, "| `engine` | string")
	s.Contains(md, "| `nodeSelector` | map[string]string")
	s.Contains(md, "| `dependsOn` | array[string]")
}

func (s *ReportSuite) TestRenderSummaryMarkdown() {
	summary := Summary{
		Total:        2,
		TriggerTypes: map[string]int{"cron": 1},
		Engines:      map[string]int{"docker": 2},
	}

	md := RenderSummaryMarkdown(summary)
	s.Contains(md, "Total definitions: **2**")
	s.Contains(md, "cron")
	s.Contains(md, "docker")
}

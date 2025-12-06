package app

import (
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/config"
	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/suite"
)

type ModelSuite struct {
	suite.Suite
}

func TestModelSuite(t *testing.T) {
	suite.Run(t, new(ModelSuite))
}

func (s *ModelSuite) newReadyModel() Model {
	m := New(nil)
	m.state = statusReady
	m.active = sectionJobs
	return m
}

func (s *ModelSuite) TestJobDetailLoadedBuildsGraph() {
	model := s.newReadyModel()
	detail := &api.JobDetail{
		Job: api.JobDescriptor{ID: "job-1", Alias: "demo"},
		DAG: &api.JobDAG{Nodes: []api.JobDAGNode{{ID: "task-a", AtomID: "atom-1"}}},
	}

	res, cmd := model.Update(jobDetailLoadedMsg{detail: detail})
	s.Require().Nil(cmd)
	updated := res.(Model)
	s.Require().NotNil(updated.graph)
	s.Equal("task-a", updated.focusedNodeID)
	s.NoError(updated.detailErr)
}

func (s *ModelSuite) TestJobDetailErrorSetsDetailError() {
	model := s.newReadyModel()
	errBoom := errors.New("boom")
	res, _ := model.Update(jobDetailErrMsg{err: errBoom})
	updated := res.(Model)
	s.ErrorIs(updated.detailErr, errBoom)
	s.Nil(updated.graph)
}

func (s *ModelSuite) TestSetFocusedNodePreloadsAtomMetadata() {
	cfg := &config.Config{BaseURL: s.mustParseURL("http://example.com"), HTTPTimeout: time.Second}
	client := api.New(cfg)
	model := New(client)
	graph, err := dag.FromJobDAG(&api.JobDAG{Nodes: []api.JobDAGNode{{ID: "task-a", AtomID: "atom-1"}}})
	s.Require().NoError(err)
	model.graph = graph

	cmd := model.setFocusedNode("task-a")
	s.Require().NotNil(cmd)
	s.Equal("atom-1", model.loadingAtomID)

	res, _ := model.Update(atomDetailLoadedMsg{id: "atom-1", atom: &api.Atom{ID: "atom-1"}})
	updated := res.(Model)
	s.Empty(updated.loadingAtomID)
	s.Contains(updated.atomDetails, "atom-1")
}

func (s *ModelSuite) TestAtomDetailErrorSetsAtomErr() {
	model := s.newReadyModel()
	model.loadingAtomID = "atom-1"

	res, _ := model.Update(atomDetailErrMsg{id: "atom-1", err: errors.New("fail")})
	updated := res.(Model)
	s.Empty(updated.loadingAtomID)
	s.Error(updated.atomErr)
}

func (s *ModelSuite) TestEnterActivatesDetailView() {
	model := s.newReadyModel()
	model.jobDetail = &api.JobDetail{Job: api.JobDescriptor{ID: "job-1", Alias: "demo"}}

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := res.(Model)
	s.True(updated.showDetail)
	s.False(updated.detailLoading)
}

func (s *ModelSuite) TestEnterTriggersDetailFetchWhenMissing() {
	model := s.newReadyModel()
	model.jobs.SetRows([]table.Row{{"alias", "-", "-", "-", "job-1", ""}})

	res, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s.NotNil(cmd)
	updated := res.(Model)
	s.True(updated.showDetail)
	s.True(updated.detailLoading)
}

func (s *ModelSuite) TestEscAndQExitDetailView() {
	model := s.newReadyModel()
	model.jobDetail = &api.JobDetail{Job: api.JobDescriptor{ID: "job-1"}}
	model.showDetail = true

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	updated := res.(Model)
	s.False(updated.showDetail)

	model.showDetail = true
	res, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	updated = res.(Model)
	s.False(updated.showDetail)
}

func (s *ModelSuite) TestResetDetailViewResetsFields() {
	model := s.newReadyModel()
	model.graph = &dag.Graph{}
	model.focusedNodeID = "node"
	model.atomErr = errors.New("existing")
	model.loadingAtomID = "atom"
	model.showDetail = true
	model.detailLoading = true

	model.resetDetailView(errors.New("detail"), errors.New("dag"), true)
	s.Nil(model.graph)
	s.Empty(model.focusedNodeID)
	s.Empty(model.dagLayout)
	s.NotNil(model.detailErr)
	s.NotNil(model.dagErr)
	s.Empty(model.atomDetails)
	s.Empty(model.loadingAtomID)
	s.False(model.showDetail)
	s.False(model.detailLoading)
}

func (s *ModelSuite) TestResetDetailViewKeepsDetailVisibleWhenRequested() {
	model := s.newReadyModel()
	model.showDetail = true
	model.detailLoading = true
	model.resetDetailView(nil, nil, false)
	s.True(model.showDetail)
	s.True(model.detailLoading)
}

func (s *ModelSuite) TestCycleFocusedNodeTraversesLevels() {
	model := s.newReadyModel()
	graph, err := dag.FromJobDAG(&api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "task-a", Successors: []string{"task-b"}},
			{ID: "task-b"},
		},
	})
	s.Require().NoError(err)
	model.graph = graph

	model.cycleFocusedNode(1)
	s.Equal("task-a", model.focusedNodeID)

	model.cycleFocusedNode(1)
	s.Equal("task-b", model.focusedNodeID)

	model.cycleFocusedNode(-1)
	s.Equal("task-a", model.focusedNodeID)
}

func (s *ModelSuite) TestClearTriggeringJob() {
	model := s.newReadyModel()
	model.triggeringJobID = "job-1"
	model.clearTriggeringJob("job-2")
	s.Equal("job-1", model.triggeringJobID)
	model.clearTriggeringJob("job-1")
	s.Empty(model.triggeringJobID)
}

func (s *ModelSuite) TestSetActionStatus() {
	model := s.newReadyModel()
	model.setActionStatus("notice", errors.New("boom"))
	s.Equal("notice", model.actionNotice)
	s.Error(model.actionErr)
}

func (s *ModelSuite) TestTriggerSelectedJobSetsState() {
	model := s.newReadyModel()
	model.jobs.SetRows([]table.Row{{"alias", "-", "-", "-", "job-1", ""}})
	cmd := model.triggerSelectedJob()
	s.NotNil(cmd)
	s.Equal("job-1", model.triggeringJobID)
	s.Contains(model.actionNotice, "alias")
}

func (s *ModelSuite) TestTriggerCommandsUpdateActionStatus() {
	model := s.newReadyModel()
	model.jobs.SetRows([]table.Row{{"alias", "-", "-", "-", "job-1", ""}})
	model.triggeringJobID = "job-1"

	res, _ := model.Update(jobTriggeredMsg{jobID: "job-1", run: &api.Run{ID: "abcdefghijk"}})
	updated := res.(Model)
	s.Empty(updated.triggeringJobID)
	s.Contains(updated.actionNotice, "Run abcdefgh")
	s.NoError(updated.actionErr)

	res, _ = updated.Update(jobTriggerErrMsg{jobID: "job-1", err: errors.New("bad")})
	updated = res.(Model)
	s.Empty(updated.actionNotice)
	s.Error(updated.actionErr)
}

func (s *ModelSuite) TestActionStatusText() {
	model := s.newReadyModel()
	model.actionNotice = "pending"
	s.Equal("pending", model.actionStatusText())
	model.actionErr = errors.New("boom")
	s.Contains(model.actionStatusText(), "boom")
}

func (s *ModelSuite) mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	s.Require().NoError(err)
	return u
}

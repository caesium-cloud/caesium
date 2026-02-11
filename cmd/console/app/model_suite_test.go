package app

import (
	"errors"
	"net/url"
	"os"
	"strings"
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

	res, _ := model.Update(jobDetailLoadedMsg{detail: detail})
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

func (s *ModelSuite) TestClearPendingAction() {
	model := s.newReadyModel()
	model.actionPending = &actionRequest{kind: actionTrigger, jobID: "job-1"}
	req := model.clearPendingAction("job-2")
	s.Nil(req)
	s.NotNil(model.actionPending)
	req = model.clearPendingAction("job-1")
	s.NotNil(req)
	s.Nil(model.actionPending)
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
	s.Nil(cmd)
	s.NotNil(model.confirmAction)
	s.Equal("job-1", model.confirmAction.jobID)
	s.Equal(actionTrigger, model.confirmAction.kind)
	s.Contains(model.confirmAction.label, "alias")
}

func (s *ModelSuite) TestConfirmActionStartsTrigger() {
	cfg := &config.Config{BaseURL: s.mustParseURL("http://example.com"), HTTPTimeout: time.Second}
	model := New(api.New(cfg))
	model.state = statusReady
	model.active = sectionJobs
	model.jobs.SetRows([]table.Row{{"alias", "-", "-", "-", "job-1", ""}})
	model.triggerSelectedJob()
	s.NotNil(model.confirmAction)

	res, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated := res.(Model)
	s.NotNil(cmd)
	s.Nil(updated.confirmAction)
	s.NotNil(updated.actionPending)
	s.Equal(actionTrigger, updated.actionPending.kind)
	s.Contains(updated.actionNotice, "Triggering")
}

func (s *ModelSuite) TestConfirmActionCancelClearsRequest() {
	model := s.newReadyModel()
	model.confirmAction = &actionRequest{kind: actionTrigger, jobID: "job-1", label: "alias"}

	res, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	updated := res.(Model)
	s.Nil(cmd)
	s.Nil(updated.confirmAction)
}

func (s *ModelSuite) TestRunsModalRerunConfirmStartsAction() {
	cfg := &config.Config{BaseURL: s.mustParseURL("http://example.com"), HTTPTimeout: time.Second}
	model := New(api.New(cfg))
	model.state = statusReady
	model.active = sectionJobs
	model.showRunsModal = true
	model.runs = []api.Run{{ID: "run-1", JobID: "job-1"}}
	model.runsTable.SetRows(runsToRows(model.runs, ""))
	model.runsTable.SetCursor(0)

	res, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'R'}})
	s.Nil(cmd)
	updated := res.(Model)
	s.NotNil(updated.confirmAction)
	s.Equal(actionRerun, updated.confirmAction.kind)
	s.Equal("run-1", updated.confirmAction.runID)

	res, cmd = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = res.(Model)
	s.NotNil(cmd)
	s.Nil(updated.confirmAction)
	s.NotNil(updated.actionPending)
	s.Equal(actionRerun, updated.actionPending.kind)
	s.Contains(updated.actionNotice, "Re-running")
}

func (s *ModelSuite) TestTriggerCommandsUpdateActionStatus() {
	model := s.newReadyModel()
	model.jobs.SetRows([]table.Row{{"alias", "-", "-", "-", "job-1", ""}})
	model.actionPending = &actionRequest{kind: actionTrigger, jobID: "job-1"}

	res, _ := model.Update(jobTriggeredMsg{jobID: "job-1", run: &api.Run{ID: "abcdefghijk"}})
	updated := res.(Model)
	s.Nil(updated.actionPending)
	s.Contains(updated.actionNotice, "Run abcdefgh")
	s.NoError(updated.actionErr)

	updated.actionPending = &actionRequest{kind: actionTrigger, jobID: "job-1"}
	res, _ = updated.Update(jobTriggerErrMsg{jobID: "job-1", err: errors.New("bad")})
	updated = res.(Model)
	s.Contains(updated.actionNotice, "Trigger failed")
	s.Error(updated.actionErr)
}

func (s *ModelSuite) TestRerunCommandsUpdateActionStatus() {
	model := s.newReadyModel()
	model.actionPending = &actionRequest{kind: actionRerun, jobID: "job-1", runID: "source-123"}

	res, _ := model.Update(jobTriggeredMsg{jobID: "job-1", run: &api.Run{ID: "rerun-456"}})
	updated := res.(Model)
	s.Nil(updated.actionPending)
	s.Contains(updated.actionNotice, "Re-run from "+shortID("source-123")+" accepted as "+shortID("rerun-456"))
	s.NoError(updated.actionErr)

	updated.actionPending = &actionRequest{kind: actionRerun, jobID: "job-1", runID: "source-123"}
	res, _ = updated.Update(jobTriggerErrMsg{jobID: "job-1", err: errors.New("bad")})
	updated = res.(Model)
	s.Contains(updated.actionNotice, "Re-run failed")
	s.Error(updated.actionErr)
}

func (s *ModelSuite) TestActionStatusText() {
	model := s.newReadyModel()
	model.actionNotice = "pending"
	s.Equal("pending", model.actionStatusText())
	model.actionErr = errors.New("boom")
	s.Contains(model.actionStatusText(), "boom")
}

func (s *ModelSuite) TestRequestRerunSelectedRunOpensConfirm() {
	model := s.newReadyModel()
	model.runs = []api.Run{{ID: "run-1", JobID: "job-1"}}
	model.runsTable.SetRows(runsToRows(model.runs, ""))
	model.runsTable.SetCursor(0)

	cmd := model.requestRerunSelectedRun()
	s.Nil(cmd)
	s.NotNil(model.confirmAction)
	s.Equal(actionRerun, model.confirmAction.kind)
	s.Equal("job-1", model.confirmAction.jobID)
	s.Equal("run-1", model.confirmAction.runID)
}

func (s *ModelSuite) TestRequestRerunSelectedRunRejectsRunningRun() {
	model := s.newReadyModel()
	model.runs = []api.Run{{ID: "run-1", JobID: "job-1", Status: "running"}}
	model.runsTable.SetRows(runsToRows(model.runs, ""))
	model.runsTable.SetCursor(0)

	cmd := model.requestRerunSelectedRun()
	s.Nil(cmd)
	s.Nil(model.confirmAction)
	s.Contains(model.actionNotice, "Re-run unavailable")
	s.NoError(model.actionErr)
}

func (s *ModelSuite) TestFilteredLogContentAppliesQuery() {
	model := s.newReadyModel()
	model.logContent = "alpha\nbeta\ngamma\n"
	model.logFilter = "bet"
	s.Equal("beta", model.filteredLogContent())
}

func (s *ModelSuite) TestLogsModalFilterInputFlow() {
	model := s.newReadyModel()
	model.showLogsModal = true
	model.logContent = "alpha\nbeta\n"
	model.refreshLogsViewport()

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	updated := res.(Model)
	s.True(updated.logFilterInput)

	res, _ = updated.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}})
	updated = res.(Model)
	s.Equal("b", updated.logFilter)
	s.True(updated.logFilterInput)
	s.Equal("beta", strings.TrimSpace(updated.filteredLogContent()))

	res, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEnter})
	updated = res.(Model)
	s.False(updated.logFilterInput)
}

func (s *ModelSuite) TestExportFilteredLogsWritesFile() {
	model := s.newReadyModel()
	model.logContent = "alpha\nbeta\ngamma\n"
	model.logFilter = "bet"
	model.logRunID = "run-1"
	model.logTaskID = "task-1"

	cmd := model.exportFilteredLogs()
	s.Require().NotNil(cmd)
	msg := cmd()

	exported, ok := msg.(logsExportedMsg)
	s.Require().True(ok)
	s.T().Cleanup(func() { _ = os.Remove(exported.path) })

	data, err := os.ReadFile(exported.path)
	s.Require().NoError(err)
	content := string(data)
	s.Contains(content, "run_id=run-1")
	s.Contains(content, "task_id=task-1")
	s.Contains(content, "beta")
	s.NotContains(content, "alpha")
}

func (s *ModelSuite) TestHelpOverlayToggle() {
	model := s.newReadyModel()

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	updated := res.(Model)
	s.True(updated.showHelp)

	res, _ = updated.Update(tea.KeyMsg{Type: tea.KeyEsc})
	updated = res.(Model)
	s.False(updated.showHelp)
}

func (s *ModelSuite) TestThemeCycleHotkey() {
	model := s.newReadyModel()
	initial := model.themeName

	res, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'T'}})
	updated := res.(Model)
	s.NotEmpty(updated.themeName)
	s.NotEqual(initial, updated.themeName)
	s.Contains(updated.actionNotice, "Theme switched")
}

func (s *ModelSuite) TestHealthCheckUpdatesDiagnostics() {
	model := s.newReadyModel()
	checkedAt := time.Now()

	res, _ := model.Update(healthCheckedMsg{
		ok:        true,
		latency:   42 * time.Millisecond,
		checkedAt: checkedAt,
	})
	updated := res.(Model)
	s.True(updated.apiHealthy)
	s.Equal(42*time.Millisecond, updated.apiLatency)
	s.True(updated.apiCheckedAt.Equal(checkedAt))
	s.Contains(updated.actionNotice, "Health check passed")
}

func (s *ModelSuite) TestDiagnosticsStatusTextIncludesRetries() {
	model := s.newReadyModel()
	model.apiCheckedAt = time.Now()
	model.apiHealthy = true
	model.apiLatency = 15 * time.Millisecond
	model.lastLoadLatency = 90 * time.Millisecond
	model.lastLoadRetries = 2

	text := model.diagnosticsStatusText()
	s.Contains(text, "api:healthy")
	s.Contains(text, "retries:2")
}

func (s *ModelSuite) mustParseURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	s.Require().NoError(err)
	return u
}

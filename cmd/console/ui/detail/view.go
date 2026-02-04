package detail

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/caesium-cloud/caesium/cmd/console/ui/dag"
	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
)

var (
	accentColor       = lipgloss.Color("99")
	headerStyle       = lipgloss.NewStyle().Bold(true).Foreground(accentColor)
	sectionTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(accentColor).MarginTop(1)
	labelStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	valueStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("253"))
	errorStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	placeholderStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	blockStyle        = lipgloss.NewStyle().MarginTop(1)
)

// ViewModel captures the data required to render the job detail pane.
type ViewModel struct {
	Job           *api.JobDetail
	ActiveRun     *api.Run
	Graph         *dag.Graph
	GraphLayout   string
	GraphViewport *viewport.Model
	FocusPath     bool
	FocusedNode   string
	FocusedAtom   *api.Atom
	TaskStatus    map[string]api.RunTask
	DetailErr     error
	DetailPending bool
	GraphErr      error
	AtomErr       error
	AtomLoading   bool
	AtomLookup    map[string]api.Atom
	Spinner       string
	Labeler       dag.LabelFunc
	ViewportWidth int
}

// Render produces a formatted detail panel for the selected job.
func Render(vm ViewModel) string {
	if vm.ViewportWidth > 0 {
		blockStyle = blockStyle.MaxWidth(vm.ViewportWidth)
	}

	if vm.DetailErr != nil {
		return errorStyle.Render("Failed to load job details: " + vm.DetailErr.Error())
	}

	if vm.DetailPending {
		return placeholderStyle.Render("Loading job details…")
	}

	if vm.Job == nil {
		return placeholderStyle.Render("Select a job to view metadata, DAG layout, and run status.")
	}

	sections := []string{
		renderHeader(vm.Job),
		renderMetadata(vm.Job),
		renderRunSection(vm.Job, vm.ActiveRun, vm.Spinner),
		renderGraph(vm.Graph, vm.GraphErr, vm.GraphLayout, vm.GraphViewport, vm.FocusPath, vm.Labeler),
		renderNode(vm.Graph, vm.FocusedNode, vm.Labeler, vm.TaskStatus, vm.Spinner),
		renderAtom(vm.Graph, vm.FocusedNode, vm.FocusedAtom, vm.AtomErr, vm.AtomLoading, vm.AtomLookup),
	}

	output := make([]string, 0, len(sections))
	for _, block := range sections {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		output = append(output, blockStyle.Render(block))
	}

	return strings.TrimSpace(strings.Join(output, "\n"))
}

func renderGraph(graph *dag.Graph, graphErr error, layout string, vp *viewport.Model, focusPath bool, labeler dag.LabelFunc) string {
	switch {
	case graphErr != nil:
		return renderSection("DAG", errorStyle.Render("Failed to load DAG: "+graphErr.Error()))
	case graph == nil:
		return renderSection("DAG", placeholderStyle.Render("DAG layout unavailable"))
	default:
		view := strings.TrimSpace(layout)
		if view == "" {
			view = dag.Render(graph, dag.RenderOptions{
				FocusedID: "",
				Labeler:   labeler,
				FocusPath: focusPath,
			})
		}
		if strings.TrimSpace(view) == "" {
			return renderSection("DAG", placeholderStyle.Render("No DAG nodes registered for this job"))
		}
		if vp != nil && layout != "" {
			view = vp.View()
		}

		if meta := graphMeta(focusPath); meta != "" {
			view = lipgloss.JoinVertical(lipgloss.Left, meta, view)
		}

		return renderSection("DAG", view)
	}
}

func graphMeta(focusPath bool) string {
	if !focusPath {
		return ""
	}
	return valueStyle.Render("Path focus")
}

func renderHeader(detail *api.JobDetail) string {
	title := headerStyle.Render(detail.Job.Alias)
	sub := fmt.Sprintf("ID: %s", detail.Job.ID)
	if detail.Trigger != nil {
		sub += fmt.Sprintf("  •  Trigger: %s (%s)", detail.Trigger.Type, detail.Trigger.Alias)
	}

	return lipgloss.JoinVertical(lipgloss.Left, title, labelStyle.Render(sub))
}

func renderMetadata(detail *api.JobDetail) string {
	items := []string{
		formatKVPairs("Labels", detail.Job.Labels),
		formatKVPairs("Annotations", detail.Job.Annotations),
	}
	return renderSection("Metadata", strings.Join(items, "\n"))
}

func renderRunSection(detail *api.JobDetail, active *api.Run, spinnerFrame string) string {
	if detail == nil || (detail.LatestRun == nil && active == nil) {
		return renderSection("Latest Run", placeholderStyle.Render("No runs recorded"))
	}

	run := active
	if run == nil {
		run = detail.LatestRun
	}
	latest := detail.LatestRun
	title := "Latest Run"
	if latest != nil && run != nil && run.ID != latest.ID {
		title = "Selected Run"
	}
	status := statusBadge(run.Status, spinnerFrame, false)
	lines := []string{
		fmt.Sprintf("%s  •  Status: %s", labelStyle.Render(run.ID), valueStyle.Render(status)),
		fmt.Sprintf("Started: %s", run.StartedAt.Format(time.RFC3339)),
	}
	if run.CompletedAt != nil {
		lines = append(lines, fmt.Sprintf("Completed: %s", run.CompletedAt.Format(time.RFC3339)))
	}
	if strings.TrimSpace(run.Error) != "" {
		lines = append(lines, errorStyle.Render(run.Error))
	}
	if summary := taskSummary(run.Tasks, spinnerFrame); summary != "" {
		lines = append(lines, summary)
	}
	if latest != nil && run != nil && run.ID != latest.ID {
		lines = append(lines, fmt.Sprintf("Latest: %s", valueStyle.Render(shortID(latest.ID))))
	}

	return renderSection(title, strings.Join(lines, "\n"))
}

func renderNode(graph *dag.Graph, focused string, labeler dag.LabelFunc, tasks map[string]api.RunTask, spinnerFrame string) string {
	if graph == nil || focused == "" {
		return renderSection("Node", placeholderStyle.Render("Navigate the DAG to inspect node metadata."))
	}

	node, ok := graph.Node(focused)
	if !ok {
		return renderSection("Node", placeholderStyle.Render("Selected node not found in DAG."))
	}

	label := nodeLabel(node, labeler)
	lines := []string{
		fmt.Sprintf("ID: %s", valueStyle.Render(label)),
	}

	if task, ok := tasks[node.ID()]; ok {
		taskStatus := statusBadge(task.Status, spinnerFrame, true)
		if strings.TrimSpace(taskStatus) != "" {
			lines = append(lines, fmt.Sprintf("Status: %s", valueStyle.Render(taskStatus)))
		}
	}

	successors := node.Successors()
	if len(successors) > 0 {
		list := make([]string, 0, len(successors))
		for _, succ := range successors {
			list = append(list, nodeLabel(succ, labeler))
		}
		lines = append(lines, fmt.Sprintf("Successors: %s", valueStyle.Render(strings.Join(list, ", "))))
	}

	predecessors := node.Predecessors()
	if len(predecessors) > 0 {
		list := make([]string, 0, len(predecessors))
		for _, pred := range predecessors {
			list = append(list, nodeLabel(pred, labeler))
		}
		lines = append(lines, fmt.Sprintf("Predecessors: %s", valueStyle.Render(strings.Join(list, ", "))))
	}

	return renderSection("Node", strings.Join(lines, "\n"))
}

func renderAtom(graph *dag.Graph, focused string, atom *api.Atom, atomErr error, loading bool, lookup map[string]api.Atom) string {
	if graph == nil || focused == "" {
		return ""
	}
	node, ok := graph.Node(focused)
	if !ok || node.AtomID() == "" {
		return renderSection("Atom", placeholderStyle.Render("No atom metadata linked to this node."))
	}

	if atomErr != nil {
		return renderSection("Atom", errorStyle.Render("Failed to load atom metadata: "+atomErr.Error()))
	}

	if loading && atom == nil {
		return renderSection("Atom", placeholderStyle.Render("Loading atom metadata…"))
	}

	if atom == nil && lookup != nil {
		if cached, ok := lookup[node.AtomID()]; ok {
			atom = &cached
		}
	}

	if atom == nil {
		return renderSection("Atom", placeholderStyle.Render("Atom metadata unavailable."))
	}

	lines := []string{
		fmt.Sprintf("ID: %s", valueStyle.Render(atom.ID)),
		fmt.Sprintf("Engine: %s", valueStyle.Render(atom.Engine)),
		fmt.Sprintf("Image: %s", valueStyle.Render(atom.Image)),
	}
	if len(atom.Command) > 0 {
		lines = append(lines, fmt.Sprintf("Command: %s", valueStyle.Render(strings.Join(atom.Command, " "))))
	}
	if atom.ProvenanceSourceID != "" {
		lines = append(lines, fmt.Sprintf("Source: %s", valueStyle.Render(atom.ProvenanceSourceID)))
	}

	return renderSection("Atom", strings.Join(lines, "\n"))
}

func renderSection(title, body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return lipgloss.JoinVertical(
		lipgloss.Left,
		sectionTitleStyle.Render(title),
		body,
	)
}

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func formatKVPairs(label string, values map[string]string) string {
	if len(values) == 0 {
		return fmt.Sprintf("%s: %s", labelStyle.Render(label), placeholderStyle.Render("-"))
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, len(keys))
	for i, key := range keys {
		parts[i] = fmt.Sprintf("%s=%s", key, values[key])
	}

	return fmt.Sprintf("%s: %s", labelStyle.Render(label), valueStyle.Render(strings.Join(parts, ", ")))
}

func statusBadge(status, spinnerFrame string, compact bool) string {
	normalized := strings.ToLower(strings.TrimSpace(status))
	label := titleCase(normalized)
	switch normalized {
	case "running", "pending":
		if spinnerFrame != "" {
			return fmt.Sprintf("%s %s", spinnerFrame, label)
		}
		return label
	case "succeeded":
		if compact {
			return "✓"
		}
		return "✓ Succeeded"
	case "failed":
		if compact {
			return "✗"
		}
		return "✗ Failed"
	default:
		if label == "" {
			return "-"
		}
		if compact {
			return label
		}
		return strings.ToUpper(status)
	}
}

func taskSummary(tasks []api.RunTask, spinnerFrame string) string {
	if len(tasks) == 0 {
		return ""
	}
	var running, pending int
	for _, task := range tasks {
		switch strings.ToLower(strings.TrimSpace(task.Status)) {
		case "running":
			running++
		case "pending":
			pending++
		}
	}
	if running == 0 && pending == 0 {
		return ""
	}

	parts := make([]string, 0, 2)
	if running > 0 {
		label := fmt.Sprintf("%d running", running)
		if spinnerFrame != "" {
			label = fmt.Sprintf("%s %s", spinnerFrame, label)
		}
		parts = append(parts, label)
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}

	return fmt.Sprintf("Tasks: %s", strings.Join(parts, "  "))
}

func titleCase(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) == 1 {
		return strings.ToUpper(value)
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func nodeLabel(node *dag.Node, labeler dag.LabelFunc) string {
	if labeler != nil {
		if label := strings.TrimSpace(labeler(node)); label != "" {
			return label
		}
	}
	if node == nil {
		return ""
	}
	return node.ID()
}

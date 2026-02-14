package dag

import (
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/cmd/console/ui/status"
	"github.com/charmbracelet/lipgloss"
)

// LabelFunc formats the textual label for a node.
type LabelFunc func(*Node) string

// TaskInfo captures per-node status and timing for rendering.
type TaskInfo struct {
	Status       string
	Duration     string
	SpinnerFrame string
	Image        string
	Engine       string
	Command      []string
}

// StatusColors holds color values for each status type.
type StatusColors struct {
	Success string
	Error   string
	Running string
	Pending string
	Accent  string
}

// RenderOptions configures DAG rendering behaviour.
type RenderOptions struct {
	FocusedID  string
	Labeler    LabelFunc
	FocusPath  bool
	MaxWidth   int
	TaskStatus map[string]TaskInfo
	Colors     StatusColors
}

const (
	boxMinWidth = 16
	boxPadding  = 2
	boxGap      = 2
)

// Render returns a textual representation of the DAG highlighting the focused node.
func Render(g *Graph, opts RenderOptions) string {
	if g == nil {
		return ""
	}

	if opts.Labeler == nil {
		opts.Labeler = func(n *Node) string {
			if n == nil {
				return ""
			}
			return n.ID()
		}
	}

	return renderBoxDAG(g, opts)
}

func renderBoxDAG(g *Graph, opts RenderOptions) string {
	levels := g.Levels()
	if len(levels) == 0 {
		return ""
	}

	pathNodes := make(map[string]bool)
	if opts.FocusPath && opts.FocusedID != "" {
		pathNodes = collectPath(g, opts.FocusedID)
	}

	type renderedLevel struct {
		nodes  []*Node
		boxes  []string
		row    string
		width  int
		offset int
	}

	rendered := make([]renderedLevel, len(levels))
	graphWidth := 0

	for levelIdx, level := range levels {
		boxes := make([]string, len(level))
		for i, node := range level {
			boxes[i] = renderNodeBox(node, opts, pathNodes)
		}

		row := lipgloss.JoinHorizontal(lipgloss.Top, interleave(boxes, boxGap)...)
		width := lipgloss.Width(row)
		if width > graphWidth {
			graphWidth = width
		}

		rendered[levelIdx] = renderedLevel{
			nodes: level,
			boxes: boxes,
			row:   row,
			width: width,
		}
	}

	if graphWidth <= 0 {
		return ""
	}

	for i := range rendered {
		if graphWidth > rendered[i].width {
			rendered[i].offset = (graphWidth - rendered[i].width) / 2
		}
	}

	rows := make([]string, 0, len(levels)*2-1)
	for levelIdx := range rendered {
		level := rendered[levelIdx]
		row := level.row
		if level.offset > 0 {
			row = prefixMultiline(row, level.offset)
		}
		rows = append(rows, row)

		if levelIdx < len(rendered)-1 {
			next := rendered[levelIdx+1]
			connector := renderConnectors(
				level.nodes,
				next.nodes,
				level.boxes,
				next.boxes,
				level.offset,
				next.offset,
				graphWidth,
			)
			if connector != "" {
				rows = append(rows, connector)
			}
		}
	}

	rows = centerRows(rows, opts.MaxWidth)
	return strings.Join(rows, "\n")
}

func renderNodeBox(node *Node, opts RenderOptions, pathNodes map[string]bool) string {
	if node == nil {
		return ""
	}

	info := opts.TaskStatus[node.ID()]
	status := status.Normalize(info.Status)
	isFocused := opts.FocusedID != "" && node.ID() == opts.FocusedID
	isDimmed := opts.FocusPath && len(pathNodes) > 0 && !pathNodes[node.ID()]

	// Determine icon and border color
	icon, borderColor := statusIconAndColor(status, info, opts.Colors)

	// Line 1: status icon + label
	label := strings.TrimSpace(opts.Labeler(node))
	if label == "" {
		label = node.ID()
	}
	label = stripStatusPrefix(label)
	line1 := fmt.Sprintf("%s %s", icon, label)

	// Line 2: engine icon + command summary
	engIcon := engineIcon(info.Engine)
	cmdSummary := shortCommand(info.Command, 24)
	var line2 string
	if cmdSummary != "" {
		line2 = fmt.Sprintf(" %s %s", engIcon, cmdSummary)
	} else {
		line2 = fmt.Sprintf(" %s", engIcon)
	}

	// Line 3: duration or status text
	line3 := " " + durationLabel(status, info)

	// Calculate box width
	contentWidth := maxLen(line1, line2, line3)
	if contentWidth < boxMinWidth-boxPadding*2 {
		contentWidth = boxMinWidth - boxPadding*2
	}

	// Build the style
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(borderColor)).
		Padding(0, 1).
		Width(contentWidth + boxPadding)

	if isFocused {
		style = style.Bold(true)
		if opts.Colors.Accent != "" {
			style = style.BorderForeground(lipgloss.Color(opts.Colors.Accent))
		}
	}

	if isDimmed {
		style = style.Foreground(lipgloss.Color("240"))
	}

	content := lipgloss.JoinVertical(lipgloss.Left, line1, line2, line3)
	return style.Render(content)
}

func statusIconAndColor(taskStatus string, info TaskInfo, colors StatusColors) (string, string) {
	switch taskStatus {
	case status.Succeeded:
		color := colors.Success
		if color == "" {
			color = "42"
		}
		return "✓", color
	case status.Failed:
		color := colors.Error
		if color == "" {
			color = "196"
		}
		return "✗", color
	case status.Skipped:
		color := colors.Pending
		if color == "" {
			color = "240"
		}
		return "↷", color
	case status.Running:
		color := colors.Running
		if color == "" {
			color = "214"
		}
		icon := "⠋"
		if info.SpinnerFrame != "" {
			icon = info.SpinnerFrame
		}
		return icon, color
	default:
		color := colors.Pending
		if color == "" {
			color = "240"
		}
		return "·", color
	}
}

func durationLabel(taskStatus string, info TaskInfo) string {
	switch taskStatus {
	case status.Succeeded, status.Failed, status.Skipped:
		if info.Duration != "" {
			return info.Duration
		}
		return taskStatus
	case status.Running:
		if info.Duration != "" {
			return info.Duration + "…"
		}
		return status.Running
	default:
		return status.Pending
	}
}

func renderConnectors(
	parents []*Node,
	children []*Node,
	parentBoxes []string,
	childBoxes []string,
	parentOffset int,
	childOffset int,
	canvasWidth int,
) string {
	if len(parents) == 0 || len(children) == 0 || canvasWidth <= 0 {
		return ""
	}

	parentCenters := boxCenters(parentBoxes, parentOffset)
	childCenters := boxCenters(childBoxes, childOffset)

	type edge struct {
		parentIdx int
		childIdx  int
	}

	childOrder := make(map[string]int, len(children))
	for i, c := range children {
		childOrder[c.ID()] = i
	}

	var edges []edge
	for pi, parent := range parents {
		for _, succ := range parent.Successors() {
			if ci, ok := childOrder[succ.ID()]; ok {
				edges = append(edges, edge{parentIdx: pi, childIdx: ci})
			}
		}
	}

	if len(edges) == 0 {
		return ""
	}

	line1 := make([]rune, canvasWidth)
	line2 := make([]rune, canvasWidth)
	line3 := make([]rune, canvasWidth)
	for i := range line1 {
		line1[i] = ' '
		line2[i] = ' '
		line3[i] = ' '
	}

	parentCols := make(map[int]struct{}, len(parents))
	childCols := make(map[int]struct{}, len(children))
	minCol, maxCol := canvasWidth-1, 0
	for _, e := range edges {
		pc := clampCol(parentCenters[e.parentIdx], canvasWidth)
		cc := clampCol(childCenters[e.childIdx], canvasWidth)
		parentCols[pc] = struct{}{}
		childCols[cc] = struct{}{}

		if pc < minCol {
			minCol = pc
		}
		if cc < minCol {
			minCol = cc
		}
		if pc > maxCol {
			maxCol = pc
		}
		if cc > maxCol {
			maxCol = cc
		}
	}

	for col := range parentCols {
		line1[col] = '│'
	}
	for col := range childCols {
		line3[col] = '│'
	}

	if maxCol > minCol {
		for i := minCol; i <= maxCol; i++ {
			line2[i] = '─'
		}
		for col := range parentCols {
			line2[col] = mergeConnectorRune(line2[col], '┴')
		}
		for col := range childCols {
			line2[col] = mergeConnectorRune(line2[col], '┬')
		}
	} else {
		line2[minCol] = '│'
	}

	result := []string{
		trimRightRunes(line1),
		trimRightRunes(line2),
		trimRightRunes(line3),
	}
	return strings.Join(result, "\n")
}

func boxCenters(boxes []string, start int) []int {
	centers := make([]int, len(boxes))
	offset := start
	for i, box := range boxes {
		w := lipgloss.Width(box)
		centers[i] = offset + w/2
		offset += w + boxGap
	}
	return centers
}

func clampCol(col, width int) int {
	if width <= 0 {
		return 0
	}
	if col < 0 {
		return 0
	}
	if col >= width {
		return width - 1
	}
	return col
}

func mergeConnectorRune(current, junction rune) rune {
	switch current {
	case ' ':
		return junction
	case '─':
		return junction
	case '┴':
		if junction == '┬' {
			return '┼'
		}
		return '┴'
	case '┬':
		if junction == '┴' {
			return '┼'
		}
		return '┬'
	case '┼':
		return '┼'
	default:
		return junction
	}
}

func trimRightRunes(line []rune) string {
	end := len(line)
	for end > 0 && line[end-1] == ' ' {
		end--
	}
	return string(line[:end])
}

func interleave(items []string, gap int) []string {
	if len(items) <= 1 {
		return items
	}
	spacer := strings.Repeat(" ", gap)
	result := make([]string, 0, len(items)*2-1)
	for i, item := range items {
		if i > 0 {
			result = append(result, spacer)
		}
		result = append(result, item)
	}
	return result
}

func collectPath(g *Graph, focusedID string) map[string]bool {
	result := make(map[string]bool)
	if g == nil {
		return result
	}
	start, ok := g.Node(focusedID)
	if !ok || start == nil {
		return result
	}
	stack := []*Node{start}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if result[n.ID()] {
			continue
		}
		result[n.ID()] = true
		stack = append(stack, n.Successors()...)
		stack = append(stack, n.Predecessors()...)
	}
	return result
}

func engineIcon(engine string) string {
	switch strings.ToLower(strings.TrimSpace(engine)) {
	case "docker":
		return "🐳"
	case "kubernetes", "k8s":
		return "☸"
	case "podman":
		return "🦭"
	case "wasm", "wasmer", "wasmtime":
		return "🔮"
	default:
		if engine == "" {
			return "⚙"
		}
		return "⚙"
	}
}

func shortCommand(cmd []string, maxWidth int) string {
	if len(cmd) == 0 {
		return ""
	}

	// If the command is ["sh", "-c", "..."], extract the shell body
	if len(cmd) >= 3 {
		base := strings.TrimSpace(cmd[0])
		if (base == "sh" || base == "bash" || base == "/bin/sh" || base == "/bin/bash") && cmd[1] == "-c" {
			body := strings.Join(cmd[2:], " ")
			return truncateCommand(body, maxWidth)
		}
	}

	// Otherwise join and truncate
	full := strings.Join(cmd, " ")
	return truncateCommand(full, maxWidth)
}

func truncateCommand(s string, maxWidth int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Collapse whitespace
	fields := strings.Fields(s)
	s = strings.Join(fields, " ")
	if maxWidth <= 0 {
		maxWidth = 24
	}
	r := []rune(s)
	if len(r) <= maxWidth {
		return s
	}
	if maxWidth <= 3 {
		return string(r[:maxWidth])
	}
	return string(r[:maxWidth-1]) + "…"
}

func stripStatusPrefix(label string) string {
	prefixes := []string{"✓ ", "✗ ", "· "}
	for _, prefix := range prefixes {
		if strings.HasPrefix(label, prefix) {
			return label[len(prefix):]
		}
	}
	// Strip spinner frames (braille patterns are multi-byte)
	if len(label) > 0 {
		r := []rune(label)
		if len(r) >= 2 && r[0] >= 0x2800 && r[0] <= 0x28FF && r[1] == ' ' {
			return string(r[2:])
		}
	}
	return label
}

func maxLen(lines ...string) int {
	m := 0
	for _, line := range lines {
		w := lipgloss.Width(line)
		if w > m {
			m = w
		}
	}
	return m
}

func centerRows(rows []string, maxWidth int) []string {
	if len(rows) == 0 || maxWidth <= 0 {
		return rows
	}

	graphWidth := 0
	for _, row := range rows {
		if w := lipgloss.Width(row); w > graphWidth {
			graphWidth = w
		}
	}
	if graphWidth <= 0 || graphWidth >= maxWidth {
		return rows
	}

	padding := (maxWidth - graphWidth) / 2
	if padding <= 0 {
		return rows
	}

	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = prefixMultiline(row, padding)
	}
	return out
}

func prefixMultiline(s string, spaces int) string {
	if spaces <= 0 || s == "" {
		return s
	}
	prefix := strings.Repeat(" ", spaces)
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}

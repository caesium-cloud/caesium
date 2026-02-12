package dag

import (
	"fmt"
	"strings"

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

	var rows []string

	for levelIdx, level := range levels {
		// Render each node as a box
		boxes := make([]string, len(level))
		for i, node := range level {
			boxes[i] = renderNodeBox(node, opts, pathNodes)
		}

		// Join boxes side-by-side for this level
		row := lipgloss.JoinHorizontal(lipgloss.Top, interleave(boxes, boxGap)...)
		rows = append(rows, row)

		// Draw connectors between this level and the next
		if levelIdx < len(levels)-1 {
			connector := renderConnectors(level, levels[levelIdx+1], boxes, opts)
			if connector != "" {
				rows = append(rows, connector)
			}
		}
	}

	return strings.Join(rows, "\n")
}

func renderNodeBox(node *Node, opts RenderOptions, pathNodes map[string]bool) string {
	if node == nil {
		return ""
	}

	info := opts.TaskStatus[node.ID()]
	status := strings.ToLower(strings.TrimSpace(info.Status))
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

func statusIconAndColor(status string, info TaskInfo, colors StatusColors) (string, string) {
	switch status {
	case "succeeded":
		color := colors.Success
		if color == "" {
			color = "42"
		}
		return "✓", color
	case "failed":
		color := colors.Error
		if color == "" {
			color = "196"
		}
		return "✗", color
	case "running":
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

func durationLabel(status string, info TaskInfo) string {
	switch status {
	case "succeeded", "failed":
		if info.Duration != "" {
			return info.Duration
		}
		return status
	case "running":
		if info.Duration != "" {
			return info.Duration + "…"
		}
		return "running"
	default:
		return "pending"
	}
}

func renderConnectors(parents []*Node, children []*Node, parentBoxes []string, opts RenderOptions) string {
	if len(parents) == 0 || len(children) == 0 {
		return ""
	}

	// Calculate center position of each parent box
	parentCenters := make([]int, len(parentBoxes))
	offset := 0
	for i, box := range parentBoxes {
		w := lipgloss.Width(box)
		parentCenters[i] = offset + w/2
		offset += w + boxGap
	}
	totalWidth := offset - boxGap
	if totalWidth < 1 {
		totalWidth = 1
	}

	// For simple linear chains (1 parent -> 1 child), just draw a vertical line
	if len(parents) == 1 && len(children) == 1 {
		center := parentCenters[0]
		line := strings.Repeat(" ", center) + "│"
		return line
	}

	// Build a character grid for the connector row
	// Find which parents connect to which children
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
				edges = append(edges, edge{pi, ci})
			}
		}
	}

	if len(edges) == 0 {
		return ""
	}

	// Calculate child box centers (we need to render child boxes to know widths)
	pathNodes := make(map[string]bool)
	if opts.FocusPath && opts.FocusedID != "" {
		pathNodes = collectPath(nil, "") // empty - we just need width estimation
	}

	childBoxes := make([]string, len(children))
	for i, node := range children {
		childBoxes[i] = renderNodeBox(node, opts, pathNodes)
	}
	childCenters := make([]int, len(childBoxes))
	cOffset := 0
	// Center the child row relative to the parent row
	childTotalWidth := 0
	for i, box := range childBoxes {
		w := lipgloss.Width(box)
		if i > 0 {
			childTotalWidth += boxGap
		}
		childTotalWidth += w
	}
	childStart := 0
	if childTotalWidth < totalWidth {
		childStart = (totalWidth - childTotalWidth) / 2
	}
	cOffset = childStart
	for i, box := range childBoxes {
		w := lipgloss.Width(box)
		childCenters[i] = cOffset + w/2
		cOffset += w + boxGap
	}

	// Build connector lines
	gridWidth := max(totalWidth, cOffset) + 2
	if gridWidth < 1 {
		gridWidth = 1
	}

	// Line 1: vertical drops from parents
	line1 := make([]byte, gridWidth)
	for i := range line1 {
		line1[i] = ' '
	}
	for _, e := range edges {
		pos := parentCenters[e.parentIdx]
		if pos < gridWidth {
			line1[pos] = '|'
		}
	}

	// Line 2: horizontal bar connecting all branches
	line2 := make([]byte, gridWidth)
	for i := range line2 {
		line2[i] = ' '
	}

	// Find the range of columns that need horizontal lines
	minCol, maxCol := gridWidth, 0
	usedParents := make(map[int]bool)
	usedChildren := make(map[int]bool)
	for _, e := range edges {
		pc := parentCenters[e.parentIdx]
		cc := childCenters[e.childIdx]
		usedParents[e.parentIdx] = true
		usedChildren[e.childIdx] = true
		lo, hi := pc, cc
		if lo > hi {
			lo, hi = hi, lo
		}
		if lo < minCol {
			minCol = lo
		}
		if hi > maxCol {
			maxCol = hi
		}
	}

	// Only draw horizontal line if there's actual fan-out/fan-in
	if maxCol > minCol {
		for i := minCol; i <= maxCol && i < gridWidth; i++ {
			line2[i] = '-'
		}
		// Place junction markers
		for _, e := range edges {
			pc := parentCenters[e.parentIdx]
			if pc < gridWidth {
				line2[pc] = '+'
			}
			cc := childCenters[e.childIdx]
			if cc < gridWidth {
				line2[cc] = '+'
			}
		}
	} else {
		// Single vertical path
		if minCol < gridWidth {
			line2[minCol] = '|'
		}
	}

	// Line 3: vertical drops to children
	line3 := make([]byte, gridWidth)
	for i := range line3 {
		line3[i] = ' '
	}
	for _, e := range edges {
		pos := childCenters[e.childIdx]
		if pos < gridWidth {
			line3[pos] = '|'
		}
	}

	// Convert to proper box-drawing characters
	result := []string{
		convertConnectorLine(string(line1), "drop"),
		convertConnectorLine(string(line2), "horizontal"),
		convertConnectorLine(string(line3), "rise"),
	}

	return strings.Join(result, "\n")
}

func convertConnectorLine(line, kind string) string {
	r := []rune(line)
	for i, ch := range r {
		switch ch {
		case '|':
			r[i] = '│'
		case '-':
			r[i] = '─'
		case '+':
			// Determine junction type based on context
			hasLeft := i > 0 && (r[i-1] == '─' || r[i-1] == '+')
			hasRight := i < len(r)-1 && (line[i+1] == '-' || line[i+1] == '+')
			switch kind {
			case "horizontal":
				switch {
				case hasLeft && hasRight:
					r[i] = '┼'
				case hasLeft:
					r[i] = '┤'
				case hasRight:
					r[i] = '├'
				default:
					r[i] = '│'
				}
			}
		}
	}
	return strings.TrimRight(string(r), " ")
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

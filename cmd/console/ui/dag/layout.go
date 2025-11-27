package dag

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// LabelFunc formats the textual label for a node.
type LabelFunc func(*Node) string

// RenderOptions configures DAG rendering behaviour.
type RenderOptions struct {
	FocusedID string
	Labeler   LabelFunc
	FocusPath bool
	MaxWidth  int
}

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

	return renderVertical(g, opts)
}

func renderVertical(g *Graph, opts RenderOptions) string {
	roots := g.Roots()
	if len(roots) == 0 {
		return ""
	}

	pathNodes := make(map[string]bool)
	if opts.FocusPath && opts.FocusedID != "" {
		pathNodes = collectPath(g, opts.FocusedID)
	}

	var lines []string
	seen := make(map[string]bool, len(g.nodes))

	for idx, root := range roots {
		last := idx == len(roots)-1
		lines = append(lines, renderTree(root, "", last, opts, pathNodes, seen)...)
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func renderTree(node *Node, prefix string, last bool, opts RenderOptions, pathNodes map[string]bool, seen map[string]bool) []string {
	if node == nil {
		return nil
	}

	connector := "├─ "
	nextPrefix := prefix + "│  "
	if last {
		connector = "└─ "
		nextPrefix = prefix + "   "
	}

	label := strings.TrimSpace(opts.Labeler(node))
	if label == "" {
		label = node.ID()
	}
	label = annotateText(label, node.ID(), opts, pathNodes)

	line := fmt.Sprintf("%s%s%s", prefix, connector, label)

	if seen[node.ID()] {
		return []string{line + " (shared)"}
	}
	seen[node.ID()] = true

	children := node.Successors()
	if len(children) == 0 {
		return []string{line}
	}

	lines := []string{line}
	for idx, child := range children {
		lines = append(lines, renderTree(child, nextPrefix, idx == len(children)-1, opts, pathNodes, seen)...)
	}
	return lines
}

func collectPath(g *Graph, focusedID string) map[string]bool {
	result := make(map[string]bool)
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

func annotateText(text, id string, opts RenderOptions, pathNodes map[string]bool) string {
	focusStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("57")).Bold(true)
	switch {
	case opts.FocusedID != "" && id == opts.FocusedID:
		return focusStyle.Render(fmt.Sprintf("▶ %s", text))
	case opts.FocusPath && len(pathNodes) > 0 && !pathNodes[id]:
		return fmt.Sprintf("· %s", text)
	default:
		return fmt.Sprintf("• %s", text)
	}
}

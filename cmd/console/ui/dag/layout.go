package dag

import (
	"fmt"
	"strings"
)

// LabelFunc formats the textual label for a node.
type LabelFunc func(*Node) string

// Render returns a textual representation of the DAG highlighting the focused node.
func Render(g *Graph, focusedID string, labeler LabelFunc) string {
	if g == nil {
		return ""
	}

	if labeler == nil {
		labeler = func(n *Node) string {
			if n == nil {
				return ""
			}
			return n.ID()
		}
	}

	levels := g.Levels()
	if len(levels) == 0 {
		return ""
	}

	var lines []string
	seen := make(map[string]bool)

	for _, level := range levels {
		for _, node := range level {
			if node == nil || seen[node.ID()] {
				continue
			}
			seen[node.ID()] = true

			prefix := strings.Repeat("  ", node.Depth())
			indicator := "•"
			if node.ID() == focusedID {
				indicator = "▶"
			}
			label := strings.TrimSpace(labeler(node))
			if label == "" {
				label = node.ID()
			}
			lines = append(lines, fmt.Sprintf("%s%s %s", prefix, indicator, label))

			successors := node.Successors()
			for _, succ := range successors {
				sLabel := strings.TrimSpace(labeler(succ))
				if sLabel == "" {
					sLabel = succ.ID()
				}
				lines = append(lines, fmt.Sprintf("%s  ↳ %s", prefix, sLabel))
			}
			lines = append(lines, "")
		}
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

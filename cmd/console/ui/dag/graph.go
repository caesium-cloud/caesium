package dag

import (
	"fmt"
	"sort"

	"github.com/caesium-cloud/caesium/cmd/console/api"
)

// Graph represents a translated DAG structure with adjacency metadata.
type Graph struct {
	nodes  map[string]*Node
	roots  []*Node
	levels [][]*Node
}

// Node represents a DAG node with adjacency references.
type Node struct {
	id           string
	atomID       string
	successors   []*Node
	predecessors []*Node
	depth        int
	order        int
}

// FromJobDAG converts the API DAG payload into an internal graph structure.
func FromJobDAG(spec *api.JobDAG) (*Graph, error) {
	if spec == nil {
		return nil, fmt.Errorf("dag: nil input")
	}

	nodes := make(map[string]*Node, len(spec.Nodes))
	for _, raw := range spec.Nodes {
		if raw.ID == "" {
			return nil, fmt.Errorf("dag: node missing id")
		}
		if _, exists := nodes[raw.ID]; exists {
			return nil, fmt.Errorf("dag: duplicate node %s", raw.ID)
		}
		nodes[raw.ID] = &Node{
			id:     raw.ID,
			atomID: raw.AtomID,
		}
	}

	if len(nodes) == 0 {
		return &Graph{
			nodes:  nodes,
			roots:  nil,
			levels: nil,
		}, nil
	}

	indegree := make(map[string]int, len(nodes))

	for _, raw := range spec.Nodes {
		node := nodes[raw.ID]
		unique := make(map[string]struct{}, len(raw.Successors))
		successors := make([]string, 0, len(raw.Successors))
		for _, targetID := range raw.Successors {
			if targetID == "" {
				continue
			}
			if _, seen := unique[targetID]; seen {
				continue
			}
			if _, ok := nodes[targetID]; !ok {
				continue
			}
			unique[targetID] = struct{}{}
			successors = append(successors, targetID)
		}
		if len(successors) > 1 {
			sort.Strings(successors)
		}

		for _, targetID := range successors {
			target := nodes[targetID]
			node.successors = append(node.successors, target)
			target.predecessors = append(target.predecessors, node)
			indegree[targetID]++
		}
	}

	roots := make([]*Node, 0)
	for id, node := range nodes {
		if indegree[id] == 0 {
			roots = append(roots, node)
		}
	}
	sortNodes(roots)

	queue := make([]*Node, len(roots))
	copy(queue, roots)

	remaining := make(map[string]int, len(nodes))
	for id := range nodes {
		remaining[id] = indegree[id]
	}

	for _, node := range queue {
		node.depth = 0
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, succ := range current.successors {
			if current.depth+1 > succ.depth {
				succ.depth = current.depth + 1
			}
			remaining[succ.id]--
			if remaining[succ.id] == 0 {
				queue = insertSorted(queue, succ)
			}
		}
	}

	for id, count := range remaining {
		if count > 0 {
			return nil, fmt.Errorf("dag: cycle detected at %s", id)
		}
	}

	maxDepth := 0
	for _, node := range nodes {
		if node.depth > maxDepth {
			maxDepth = node.depth
		}
		if len(node.successors) > 1 {
			sortNodes(node.successors)
		}
		if len(node.predecessors) > 1 {
			sortNodes(node.predecessors)
		}
	}

	levels := make([][]*Node, maxDepth+1)
	for _, node := range nodes {
		d := node.depth
		levels[d] = append(levels[d], node)
	}
	for _, level := range levels {
		sortNodes(level)
		for order, node := range level {
			node.order = order
		}
	}

	return &Graph{
		nodes:  nodes,
		roots:  roots,
		levels: levels,
	}, nil
}

// Node returns the node with the given identifier.
func (g *Graph) Node(id string) (*Node, bool) {
	if g == nil {
		return nil, false
	}
	node, ok := g.nodes[id]
	return node, ok
}

// Roots returns the root nodes in lexical order.
func (g *Graph) Roots() []*Node {
	if g == nil || len(g.roots) == 0 {
		return nil
	}
	out := make([]*Node, len(g.roots))
	copy(out, g.roots)
	return out
}

// Levels returns nodes grouped by depth, preserving stable ordering.
func (g *Graph) Levels() [][]*Node {
	if g == nil || len(g.levels) == 0 {
		return nil
	}
	out := make([][]*Node, len(g.levels))
	for i, level := range g.levels {
		row := make([]*Node, len(level))
		copy(row, level)
		out[i] = row
	}
	return out
}

// NodeCount returns the total number of nodes in the graph.
func (g *Graph) NodeCount() int {
	if g == nil {
		return 0
	}
	return len(g.nodes)
}

// AllNodes returns all nodes in level order.
func (g *Graph) AllNodes() []*Node {
	if g == nil {
		return nil
	}
	var out []*Node
	for _, level := range g.levels {
		out = append(out, level...)
	}
	return out
}

// First returns the first root node, if present.
func (g *Graph) First() *Node {
	if g == nil || len(g.roots) == 0 {
		return nil
	}
	return g.roots[0]
}

// ID returns the node identifier.
func (n *Node) ID() string {
	return n.id
}

// AtomID returns the associated atom identifier.
func (n *Node) AtomID() string {
	return n.atomID
}

// Successors returns the successor nodes in lexical order.
func (n *Node) Successors() []*Node {
	if n == nil {
		return nil
	}
	out := make([]*Node, len(n.successors))
	copy(out, n.successors)
	return out
}

// Predecessors returns the predecessor nodes in lexical order.
func (n *Node) Predecessors() []*Node {
	if n == nil {
		return nil
	}
	out := make([]*Node, len(n.predecessors))
	copy(out, n.predecessors)
	return out
}

// Depth returns the assigned depth of the node.
func (n *Node) Depth() int {
	if n == nil {
		return 0
	}
	return n.depth
}

// Order returns the order of the node within its depth level.
func (n *Node) Order() int {
	if n == nil {
		return 0
	}
	return n.order
}

func sortNodes(nodes []*Node) {
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].id < nodes[j].id
	})
}

func insertSorted(queue []*Node, node *Node) []*Node {
	index := sort.Search(len(queue), func(i int) bool {
		return queue[i].id >= node.id
	})
	queue = append(queue, nil)
	copy(queue[index+1:], queue[index:])
	queue[index] = node
	return queue
}

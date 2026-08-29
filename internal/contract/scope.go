package contract

import (
	"strings"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// AliasSet returns the set of job aliases declared by a definition batch. It is
// the canonical "which jobs is this request about" set shared by apply
// enforcement and by the lint/check read surfaces that scope findings.
func AliasSet(defs []schema.Definition) map[string]struct{} {
	aliases := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		alias := strings.TrimSpace(def.Metadata.Alias)
		if alias != "" {
			aliases[alias] = struct{}{}
		}
	}
	return aliases
}

// EdgeInAliasScope reports whether a contract edge touches one of the given job
// aliases as its producer or consumer endpoint. Every derived edge (declared,
// inferred, and evidence) is job -> job, so an edge is "about" a linted job
// exactly when one of its endpoints is that job — which also pulls in the
// linted job's direct producers and direct consumers on the server.
//
// A nil alias set means "no scope requested" and matches everything, mirroring
// contractFindingInIncomingScope in enforce.go. An empty (non-nil) set scopes
// to nothing.
func EdgeInAliasScope(edge Edge, aliases map[string]struct{}) bool {
	if aliases == nil {
		return true
	}
	return nodeInAliasScope(edge.From, aliases) || nodeInAliasScope(edge.To, aliases)
}

// ScopeGraphToAliases narrows a derived contract graph to the edges that touch
// one of the given job aliases, dropping unrelated producer/consumer pairs that
// only exist because graph derivation unions the incoming batch with every job
// already persisted on the server.
//
// Nodes are retained when a kept edge references them, and the alias set's own
// job nodes are always retained so a scoped graph still names the subject jobs
// even when they have no contract edges at all.
func ScopeGraphToAliases(graph Graph, aliases map[string]struct{}) Graph {
	if aliases == nil {
		return graph
	}

	keep := make(map[string]struct{}, len(graph.Nodes))
	for alias := range aliases {
		keep[JobNodeID(alias)] = struct{}{}
	}

	edges := make([]Edge, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		if !EdgeInAliasScope(edge, aliases) {
			continue
		}
		edges = append(edges, edge)
		keep[edge.From] = struct{}{}
		keep[edge.To] = struct{}{}
		if edge.Dataset != nil {
			keep[datasetNodeID(*edge.Dataset)] = struct{}{}
		}
	}

	nodes := make([]Node, 0, len(keep))
	for _, node := range graph.Nodes {
		if _, ok := keep[node.ID]; ok {
			nodes = append(nodes, node)
		}
	}

	return Graph{Nodes: nodes, Edges: edges}
}

func nodeInAliasScope(nodeID string, aliases map[string]struct{}) bool {
	alias, ok := jobAliasFromNodeID(nodeID)
	if !ok {
		return false
	}
	_, scoped := aliases[alias]
	return scoped
}

// jobAliasFromNodeID inverts JobNodeID. Dataset node IDs (and anything else
// that is not a job node) report false rather than an empty alias.
func jobAliasFromNodeID(nodeID string) (string, bool) {
	trimmed := strings.TrimSpace(nodeID)
	if !strings.HasPrefix(trimmed, "job:") {
		return "", false
	}
	alias := strings.TrimSpace(strings.TrimPrefix(trimmed, "job:"))
	if alias == "" {
		return "", false
	}
	return alias, true
}

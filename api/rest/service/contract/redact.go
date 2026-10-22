package contract

import (
	"context"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/authz"
	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"gorm.io/gorm"
)

// ConfinedGraph is Graph for a namespace-confined caller (identity-and-access
// design §8.3, plan item B5): the contract graph is derived over every job,
// in one read-only snapshot, and then redacted for r's caller (RedactGraph).
// The dataset filter is applied to the redacted graph, so a dataset the
// caller can only see through a collapsed edge answers exactly as a dataset
// nobody declares or observes.
//
// It returns the response and, separately, the graph the findings metric is
// recorded from: the unredacted graph under the same filter, exactly what an
// unrestricted request records.
func (s *Service) ConfinedGraph(r *authz.Redactor, dataset string) (response, metrics *internalcontract.Graph, err error) {
	if !Enabled() {
		return nil, nil, ErrDisabled
	}
	filter, err := parseDatasetFilter(dataset)
	if err != nil {
		return nil, nil, err
	}
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	var whole internalcontract.Graph
	if err := jobdiff.ReadSnapshot(ctx, s.db, func(tx *gorm.DB) error {
		var deriveErr error
		whole, deriveErr = internalcontract.NewGORMDeriver(tx).DeriveGraph(ctx, nil)
		return deriveErr
	}); err != nil {
		return nil, nil, err
	}
	recorded := whole
	redacted := RedactGraph(r, whole)
	if filter != nil {
		recorded = filterGraphByDataset(whole, *filter)
		redacted = filterGraphByDataset(redacted, *filter)
	}
	return &redacted, &recorded, nil
}

// RedactGraph collapses everything a confined caller may not read:
//
//   - a job node is visible when the job that owns it is in a readable
//     namespace; any other job node (another namespace's, or one no single job
//     owns, such as an inferred edge's unknown producer) becomes
//     {id: <opaque>, kind: "external"};
//   - an edge is shown whole only when both endpoints are visible and, for an
//     evidence edge, every run it was observed from persisted a readable
//     namespace. An inferred edge is derived from a trigger, so one whose
//     endpoint shares its trigger with a job the caller cannot read is
//     withheld (the jobs themselves stay visible);
//   - every other edge keeps only its endpoints, {id, from, to}: dataset,
//     schemas, findings, verdict, class and last-seen time are dropped, and
//     edges that collapse to the same endpoints are one edge;
//   - a dataset node is shown only when an edge shown whole carries it.
//
// Collapsed nodes and edges keep the topology (an edge between two foreign
// jobs stays, between two placeholders). They follow the visible ones, sorted
// by their opaque ids: the derived order is by alias, which would otherwise
// place a placeholder next to its alias's neighbours.
func RedactGraph(r *authz.Redactor, graph internalcontract.Graph) internalcontract.Graph {
	if r == nil {
		return graph
	}
	visible := make(map[string]bool, len(graph.Nodes))
	triggerShared := make(map[string]bool, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if node.Kind != internalcontract.NodeKindJob {
			continue
		}
		owner, owned := node.OwnerNamespace()
		visible[node.ID] = owned && r.Visible(owner)
		if namespaces := node.TriggerNamespaces(); len(namespaces) > 0 && !r.Visible(namespaces...) {
			triggerShared[node.ID] = true
		}
	}
	placeholder := func(nodeID string) string {
		return r.ExternalID("job\x00" + nodeID)
	}
	endpoint := func(nodeID string) string {
		if visible[nodeID] {
			return nodeID
		}
		return placeholder(nodeID)
	}

	out := internalcontract.Graph{Nodes: []internalcontract.Node{}, Edges: []internalcontract.Edge{}}
	carried := map[internalcontract.DatasetRef]struct{}{}
	var collapsed []internalcontract.Edge
	collapsedSeen := map[string]struct{}{}
	for _, edge := range graph.Edges {
		// Every derived edge merged into this one must be readable, not only
		// the endpoints it shows: colliding edge ids merge another pair's
		// schemas and findings into it.
		whole, sharedTrigger := true, false
		for _, contributor := range edge.Contributors() {
			from, to := contributor[0], contributor[1]
			whole = whole && visible[from] && visible[to]
			sharedTrigger = sharedTrigger || triggerShared[from] || triggerShared[to]
		}
		whole = whole && visible[edge.From] && visible[edge.To]
		if whole && edge.Class == internalcontract.EdgeClassEvidence {
			whole = r.Visible(edge.RunNamespaces()...)
		}
		if whole && edge.Class == internalcontract.EdgeClassInferred && sharedTrigger {
			continue
		}
		if whole {
			out.Edges = append(out.Edges, edge)
			if edge.Dataset != nil {
				carried[trimmedDataset(*edge.Dataset)] = struct{}{}
			}
			continue
		}
		from, to := endpoint(edge.From), endpoint(edge.To)
		id := r.ExternalID("edge\x00" + from + "\x00" + to)
		if _, dup := collapsedSeen[id]; dup {
			continue
		}
		collapsedSeen[id] = struct{}{}
		collapsed = append(collapsed, internalcontract.Edge{ID: id, From: from, To: to})
	}

	var placeholders []internalcontract.Node
	placed := map[string]struct{}{}
	for _, node := range graph.Nodes {
		switch node.Kind {
		case internalcontract.NodeKindJob:
			if visible[node.ID] {
				out.Nodes = append(out.Nodes, node)
			} else {
				id := placeholder(node.ID)
				placed[id] = struct{}{}
				placeholders = append(placeholders, internalcontract.Node{ID: id, Kind: internalcontract.NodeKindExternal})
			}
		case internalcontract.NodeKindDataset:
			if node.Dataset == nil {
				continue
			}
			if _, ok := carried[trimmedDataset(*node.Dataset)]; ok {
				out.Nodes = append(out.Nodes, node)
			}
		}
	}
	// Every collapsed endpoint is a placeholder node, even one no node of the
	// derived graph stood for.
	for _, edge := range collapsed {
		for _, id := range []string{edge.From, edge.To} {
			if _, ok := placed[id]; visible[id] || ok {
				continue
			}
			placed[id] = struct{}{}
			placeholders = append(placeholders, internalcontract.Node{ID: id, Kind: internalcontract.NodeKindExternal})
		}
	}
	slices.SortFunc(placeholders, func(a, b internalcontract.Node) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(collapsed, func(a, b internalcontract.Edge) int { return strings.Compare(a.ID, b.ID) })
	out.Nodes = append(out.Nodes, placeholders...)
	out.Edges = append(out.Edges, collapsed...)
	return out
}

func trimmedDataset(dataset internalcontract.DatasetRef) internalcontract.DatasetRef {
	return internalcontract.DatasetRef{Namespace: strings.TrimSpace(dataset.Namespace), Name: strings.TrimSpace(dataset.Name)}
}

package contract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/pkg/db"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"gorm.io/gorm"
)

const enforcementEnv = "CAESIUM_CONTRACT_ENFORCEMENT"

var (
	ErrDisabled             = errors.New("contract enforcement is disabled")
	ErrInvalidDatasetFilter = errors.New("dataset filter must be in namespace/name form")
)

type Service struct {
	ctx context.Context
	db  *gorm.DB
}

type Finding struct {
	EdgeID    string                       `json:"edgeId,omitempty"`
	EdgeClass internalcontract.EdgeClass   `json:"edgeClass,omitempty"`
	From      string                       `json:"from,omitempty"`
	To        string                       `json:"to,omitempty"`
	Dataset   *internalcontract.DatasetRef `json:"dataset,omitempty"`
	Kind      schemacompat.FindingKind     `json:"kind,omitempty"`
	Path      string                       `json:"path,omitempty"`
	Key       string                       `json:"key,omitempty"`
	Detail    string                       `json:"detail,omitempty"`
	Verdict   schemacompat.Verdict         `json:"verdict"`
}

type FindingsSummary struct {
	Breaking []Finding `json:"breaking"`
	Warnings []Finding `json:"warnings"`
	Edges    int       `json:"edges"`
}

func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	cp := *s
	cp.db = conn
	return &cp
}

// EnforcementMode reads the W2-delta REST gate directly until C1's env accessor
// lands in pkg/env and W3 can unify callers on that typed field.
func EnforcementMode() string {
	return strings.ToLower(strings.TrimSpace(os.Getenv(enforcementEnv)))
}

func Enabled() bool {
	return EnforcementMode() != ""
}

func (s *Service) Graph(dataset string, incoming []schema.Definition) (*internalcontract.Graph, error) {
	if !Enabled() {
		return nil, ErrDisabled
	}
	filter, err := parseDatasetFilter(dataset)
	if err != nil {
		return nil, err
	}
	// Local fallback only — mutating s.ctx from a pointer receiver would be a
	// latent race across concurrent requests sharing the service value.
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	graph, err := internalcontract.NewGORMDeriver(s.db).DeriveGraph(ctx, incoming)
	if err != nil {
		return nil, err
	}
	if filter != nil {
		graph = filterGraphByDataset(graph, *filter)
	}
	return &graph, nil
}

func SummaryFromGraph(graph internalcontract.Graph) FindingsSummary {
	summary := FindingsSummary{
		Breaking: []Finding{},
		Warnings: []Finding{},
		Edges:    len(graph.Edges),
	}
	for _, finding := range FindingsFromGraph(graph) {
		if finding.Verdict == schemacompat.VerdictBreaking {
			summary.Breaking = append(summary.Breaking, finding)
			continue
		}
		summary.Warnings = append(summary.Warnings, finding)
	}
	return summary
}

func FindingsForAlias(graph internalcontract.Graph, alias string) []Finding {
	return FilterFindingsForAlias(FindingsFromGraph(graph), alias)
}

// FilterFindingsForAlias filters an already-materialized findings list, so
// callers iterating many aliases compute FindingsFromGraph once. The node ID
// comes from the graph package's canonical helper rather than a duplicated
// format string.
func FilterFindingsForAlias(all []Finding, alias string) []Finding {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil
	}
	nodeID := internalcontract.JobNodeID(alias)
	findings := make([]Finding, 0)
	for _, finding := range all {
		if finding.From == nodeID || finding.To == nodeID {
			findings = append(findings, finding)
		}
	}
	return findings
}

func FindingsFromGraph(graph internalcontract.Graph) []Finding {
	findings := make([]Finding, 0)
	for _, edge := range graph.Edges {
		for _, edgeFinding := range edge.Findings {
			findings = append(findings, Finding{
				EdgeID:    edge.ID,
				EdgeClass: edge.Class,
				From:      edge.From,
				To:        edge.To,
				Dataset:   cloneDataset(edge.Dataset),
				Kind:      edgeFinding.Kind,
				Path:      edgeFinding.Path,
				Key:       edgeFinding.Key,
				Detail:    edgeFinding.Detail,
				Verdict:   edgeFinding.Verdict,
			})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Verdict != findings[j].Verdict {
			return findings[i].Verdict < findings[j].Verdict
		}
		if findings[i].EdgeID != findings[j].EdgeID {
			return findings[i].EdgeID < findings[j].EdgeID
		}
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Detail < findings[j].Detail
	})
	return findings
}

func RecordFindings(graph internalcontract.Graph) {
	for _, finding := range FindingsFromGraph(graph) {
		metrics.ContractFindingsTotal.WithLabelValues(metricVerdict(finding.Verdict)).Inc()
	}
}

func parseDatasetFilter(raw string) (*internalcontract.DatasetRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 2 {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDatasetFilter, raw)
	}
	ref := internalcontract.DatasetRef{
		Namespace: strings.TrimSpace(parts[0]),
		Name:      strings.TrimSpace(parts[1]),
	}
	if ref.Namespace == "" || ref.Name == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidDatasetFilter, raw)
	}
	return &ref, nil
}

func filterGraphByDataset(graph internalcontract.Graph, filter internalcontract.DatasetRef) internalcontract.Graph {
	edges := make([]internalcontract.Edge, 0, len(graph.Edges))
	neededNodeIDs := map[string]struct{}{}
	for _, edge := range graph.Edges {
		if !datasetMatches(edge.Dataset, filter) {
			continue
		}
		edges = append(edges, edge)
		neededNodeIDs[edge.From] = struct{}{}
		neededNodeIDs[edge.To] = struct{}{}
	}

	nodes := make([]internalcontract.Node, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if _, ok := neededNodeIDs[node.ID]; ok {
			nodes = append(nodes, node)
			continue
		}
		if datasetMatches(node.Dataset, filter) {
			nodes = append(nodes, node)
		}
	}

	return internalcontract.Graph{Nodes: nodes, Edges: edges}
}

func datasetMatches(dataset *internalcontract.DatasetRef, filter internalcontract.DatasetRef) bool {
	if dataset == nil {
		return false
	}
	return strings.TrimSpace(dataset.Namespace) == filter.Namespace && strings.TrimSpace(dataset.Name) == filter.Name
}

func cloneDataset(dataset *internalcontract.DatasetRef) *internalcontract.DatasetRef {
	if dataset == nil {
		return nil
	}
	cp := *dataset
	return &cp
}

func metricVerdict(verdict schemacompat.Verdict) string {
	switch verdict {
	case schemacompat.VerdictBreaking, schemacompat.VerdictCompatible, schemacompat.VerdictUnknown:
		return string(verdict)
	default:
		return string(schemacompat.VerdictUnknown)
	}
}

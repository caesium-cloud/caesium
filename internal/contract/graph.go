// Package contract derives the cross-job contract graph from authoritative
// job, trigger, and lineage sources.
package contract

import (
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/google/uuid"
)

// NodeKind identifies the kind of graph node.
type NodeKind string

const (
	// NodeKindJob is a Caesium job node.
	NodeKindJob NodeKind = "job"
	// NodeKindDataset is an OpenLineage dataset node.
	NodeKindDataset NodeKind = "dataset"
)

// EdgeClass identifies how a contract graph edge was derived.
type EdgeClass string

const (
	// EdgeClassDeclared marks an edge declared through dataset produces/consumes blocks.
	EdgeClassDeclared EdgeClass = "declared"
	// EdgeClassInferred marks an edge inferred from lifecycle trigger chains and paramMapping paths.
	EdgeClassInferred EdgeClass = "inferred"
	// EdgeClassEvidence marks an edge observed from lineage input/output rows.
	EdgeClassEvidence EdgeClass = "evidence"
)

// DatasetRef identifies a dataset by its OpenLineage namespace/name pair.
type DatasetRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// Graph is the JSON-ready contract graph returned by read surfaces.
type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

// Node is a job or dataset graph node.
type Node struct {
	ID      string            `json:"id"`
	Kind    NodeKind          `json:"kind"`
	Alias   string            `json:"alias,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Dataset *DatasetRef       `json:"dataset,omitempty"`
}

// Edge is a derived relationship between two graph nodes.
type Edge struct {
	ID       string                 `json:"id"`
	From     string                 `json:"from"`
	To       string                 `json:"to"`
	Class    EdgeClass              `json:"class"`
	Verdict  schemacompat.Verdict   `json:"verdict,omitempty"`
	Findings []schemacompat.Finding `json:"findings,omitempty"`
	Dataset  *DatasetRef            `json:"dataset,omitempty"`
	LastSeen *time.Time             `json:"lastSeen,omitempty"`
}

// Job is the compact job definition surface needed to derive B1 graph edges.
type Job struct {
	ID      uuid.UUID
	Alias   string
	Labels  map[string]string
	Trigger Trigger
	Steps   []Step
}

// Trigger is the compact trigger surface needed for trigger-chain inference.
type Trigger struct {
	Type          string
	Configuration map[string]any
}

// Step is the compact step surface needed for output-schema path checks.
type Step struct {
	Name         string
	OutputSchema map[string]any
}

// EvidenceRecord is one distinct lineage-observed producer/dataset/consumer edge.
type EvidenceRecord struct {
	ProducerJobID    uuid.UUID
	ProducerJobAlias string
	ConsumerJobID    uuid.UUID
	ConsumerJobAlias string
	Dataset          DatasetRef
	LastSeen         time.Time
}

// DeriveInput is the pure input surface for graph derivation. Callers that
// already have the merged job world can use DeriveGraph directly.
type DeriveInput struct {
	Jobs     []Job
	Evidence []EvidenceRecord
}

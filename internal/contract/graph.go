// Package contract derives the cross-job contract graph from authoritative
// job, trigger, and lineage sources.
package contract

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
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
	// NodeKindExternal is a node collapsed for a namespace-confined caller
	// (identity-and-access B5): it carries only an opaque id.
	NodeKindExternal NodeKind = NodeKind(authz.ExternalKind)
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

	// owner is the namespace of the job row this node was derived from (its
	// current namespace); "" when no single job owns it (an inferred edge's
	// unknown producer, or two derivation sources that disagree). Never
	// serialised; the REST boundary redacts on it (identity-and-access B5).
	owner string
	// ownerConflict pins a node whose derivation sources disagreed unowned.
	ownerConflict bool
	// triggerNamespaces are the namespaces of every live job sharing this
	// job's trigger (its own included). Kept apart from owner: sharing a
	// trigger only withholds what is derived from the trigger.
	triggerNamespaces []string
}

// OwnerNamespace reports the namespace of the job that owns a job node, and
// false when no single job does (such a node is foreign to every confined
// caller).
func (n Node) OwnerNamespace() (string, bool) {
	return n.owner, n.owner != ""
}

// TriggerNamespaces returns the namespaces of the live jobs sharing a job
// node's trigger.
func (n Node) TriggerNamespaces() []string {
	return append([]string(nil), n.triggerNamespaces...)
}

// Edge is a derived relationship between two graph nodes.
type Edge struct {
	ID                     string                 `json:"id"`
	From                   string                 `json:"from"`
	To                     string                 `json:"to"`
	Class                  EdgeClass              `json:"class,omitempty"`
	Verdict                schemacompat.Verdict   `json:"verdict,omitempty"`
	Findings               []schemacompat.Finding `json:"findings,omitempty"`
	Dataset                *DatasetRef            `json:"dataset,omitempty"`
	ProducerSchema         map[string]any         `json:"producerSchema,omitempty"`
	PreviousProducerSchema map[string]any         `json:"previousProducerSchema,omitempty"`
	ConsumerSchema         map[string]any         `json:"consumerSchema,omitempty"`
	LastSeen               *time.Time             `json:"lastSeen,omitempty"`

	// runNamespaces are the persisted namespaces of the runs whose lineage an
	// evidence edge was observed from (producer and consumer runs). Never
	// serialised (identity-and-access B5).
	runNamespaces []string
	// contributors are the (from, to) node ids of every derived edge merged
	// into this one. The edge id is a plain concatenation, so two edges
	// between different jobs can share it (a ':' or '/' in an alias or a
	// dataset name) and mergeEdges folds the second's schemas and findings
	// into the first: redaction must check every contributor, not just the
	// endpoints shown (identity-and-access B5).
	contributors [][2]string
}

// Contributors returns the (from, to) node ids of every derived edge merged
// into e; an edge built by hand reports its own endpoints.
func (e Edge) Contributors() [][2]string {
	if len(e.contributors) == 0 {
		return [][2]string{{e.From, e.To}}
	}
	return append([][2]string(nil), e.contributors...)
}

// RunNamespaces returns the persisted namespaces of the runs an evidence edge
// was observed from.
func (e Edge) RunNamespaces() []string {
	return append([]string(nil), e.runNamespaces...)
}

// Job is the compact job definition surface needed to derive contract graph edges.
type Job struct {
	ID    uuid.UUID
	Alias string
	// Namespace is the job's namespace (current, for a persisted job; the
	// requested one for an incoming definition). "" means unknown.
	Namespace string
	// TriggerID is the persisted job's trigger row (zero for an incoming
	// definition).
	TriggerID uuid.UUID
	Labels    map[string]string
	Trigger   Trigger
	Steps     []Step
	Incoming  bool
}

// Trigger is the compact trigger surface needed for trigger-chain inference.
type Trigger struct {
	Type          string
	Configuration map[string]any
}

// Step is the compact step surface needed for output-schema and dataset checks.
type Step struct {
	Name         string
	OutputSchema map[string]any
	Produces     []ProducedDataset
	Consumes     []ConsumedDataset
}

// ProducedDataset is the compact produces declaration used by the contract graph.
type ProducedDataset struct {
	Name       string
	Schema     map[string]any
	SchemaFrom string
	Version    int
}

// ConsumedDataset is the compact consumes declaration used by the contract graph.
type ConsumedDataset struct {
	Name   string
	Schema map[string]any
}

// EvidenceRecord is one distinct lineage-observed producer/dataset/consumer edge.
type EvidenceRecord struct {
	ProducerJobID    uuid.UUID
	ProducerJobAlias string
	ConsumerJobID    uuid.UUID
	ConsumerJobAlias string
	// Producer/ConsumerNamespace are the jobs' current namespaces and
	// Producer/ConsumerRunNamespace the persisted namespaces of the runs the
	// lineage rows came from.
	ProducerNamespace    string
	ConsumerNamespace    string
	ProducerRunNamespace string
	ConsumerRunNamespace string
	Dataset              DatasetRef
	LastSeen             time.Time
}

// ProducerSchemaRecord captures a persisted producer's resolved dataset schema
// before an incoming definition replaces it.
type ProducerSchemaRecord struct {
	JobAlias    string
	StepName    string
	Dataset     DatasetRef
	Schema      map[string]any
	SchemaKnown bool
}

// DeriveInput is the pure input surface for graph derivation. Callers that
// already have the merged job world can use DeriveGraph directly.
type DeriveInput struct {
	Jobs                    []Job
	Evidence                []EvidenceRecord
	PreviousProducerSchemas []ProducerSchemaRecord
}

package lineage

import (
	"encoding/json"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
)

// RedactedImpactResult is an impact cone shaped for one namespace-confined
// reader (identity-and-access design §8.3, plan item B5). Downstream holds each
// node the reader may read exactly as an unrestricted caller sees it (an
// ImpactNode), and every other node as an ExternalImpactNode.
type RedactedImpactResult struct {
	RootNamespace string `json:"root_namespace"`
	RootName      string `json:"root_name"`
	Downstream    []any  `json:"downstream"`
}

// ExternalImpactNode is a downstream node collapsed for a confined reader:
// alias, namespace, dataset, job id, producing step, provenance and last-seen
// time removed. Depth is kept: it is the only structure an impact list has,
// and the collapsed topology already implies it.
type ExternalImpactNode struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Depth int    `json:"depth"`
}

// RedactImpactNodes keeps each node whose producing job's current namespace
// and whose run's persisted namespace are both readable, and collapses every
// other node. Placeholders are de-duplicated by (id, depth): repeat runs of one
// foreign producer are one node, not a run count.
func RedactImpactNodes(r *authz.Redactor, nodes []ImpactNode) []any {
	return redactNodes(r, nodes, func(node ImpactNode) bool {
		return r.Visible(node.JobNamespace, node.RunNamespace)
	})
}

func redactNodes(r *authz.Redactor, nodes []ImpactNode, visible func(ImpactNode) bool) []any {
	out := make([]any, 0, len(nodes))
	seen := map[ExternalImpactNode]struct{}{}
	for _, node := range nodes {
		if visible(node) {
			out = append(out, node)
			continue
		}
		placeholder := ExternalImpactNode{
			ID:    r.ExternalID("impact\x00" + node.DatasetNamespace + "\x00" + node.DatasetName + "\x00" + node.JobID.String()),
			Kind:  authz.ExternalKind,
			Depth: node.Depth,
		}
		if _, dup := seen[placeholder]; dup {
			continue
		}
		seen[placeholder] = struct{}{}
		out = append(out, placeholder)
	}
	return out
}

// storedImpact is the persisted form of an ImpactResult (DatasetHold.Impact).
//
// ImpactNode never serialises its owners, so that a REST body cannot name a
// namespace, but a cone stored for later needs them: a node decoded without
// them has blank namespaces, and a blank namespace reads as "default" to
// authz.NamespaceAllowed, which would show every foreign node to a reader in
// the default namespace.
type storedImpact struct {
	RootNamespace string             `json:"root_namespace"`
	RootName      string             `json:"root_name"`
	Downstream    []storedImpactNode `json:"downstream"`
}

type storedImpactNode struct {
	ImpactNode
	JobNamespace string `json:"job_namespace"`
	RunNamespace string `json:"run_namespace"`
}

// EncodeStoredImpact marshals a cone for persistence, owners included. The
// result is a superset of the ImpactResult wire shape.
func EncodeStoredImpact(result *ImpactResult) ([]byte, error) {
	stored := storedImpact{
		RootNamespace: result.RootNamespace,
		RootName:      result.RootName,
		Downstream:    make([]storedImpactNode, 0, len(result.Downstream)),
	}
	for _, node := range result.Downstream {
		stored.Downstream = append(stored.Downstream, storedImpactNode{
			ImpactNode:   node,
			JobNamespace: models.NamespaceOrDefault(node.JobNamespace),
			RunNamespace: models.NamespaceOrDefault(node.RunNamespace),
		})
	}
	return json.Marshal(stored)
}

// RedactStoredImpact decodes a cone written by EncodeStoredImpact and redacts
// it for r's reader. EncodeStoredImpact never writes a blank owner, so a node
// with one predates the owners being recorded: it is foreign whatever the
// reader's namespace. A nil r is an unrestricted reader.
func RedactStoredImpact(r *authz.Redactor, raw []byte) (*RedactedImpactResult, error) {
	var stored storedImpact
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("lineage: decode stored impact: %w", err)
	}
	nodes := make([]ImpactNode, 0, len(stored.Downstream))
	for _, node := range stored.Downstream {
		node.ImpactNode.JobNamespace = node.JobNamespace
		node.ImpactNode.RunNamespace = node.RunNamespace
		nodes = append(nodes, node.ImpactNode)
	}
	return &RedactedImpactResult{
		RootNamespace: stored.RootNamespace,
		RootName:      stored.RootName,
		Downstream: redactNodes(r, nodes, func(node ImpactNode) bool {
			if r == nil {
				return true
			}
			return node.JobNamespace != "" && node.RunNamespace != "" && r.Visible(node.JobNamespace, node.RunNamespace)
		}),
	}, nil
}

package jobdef

import (
	contractenforce "github.com/caesium-cloud/caesium/internal/contract"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

type ApplyRequest struct {
	Definitions   []schema.Definition   `json:"definitions"`
	Force         bool                  `json:"force,omitempty"`
	Prune         bool                  `json:"prune,omitempty"`
	Provenance    *ApplyProvenance      `json:"provenance,omitempty"`
	AllowBreaking *AllowBreakingRequest `json:"allow_breaking,omitempty"`
}

// ApplyProvenance lets a non-git-sync apply (e.g. a CI/CD pipeline) record the
// source it came from. SECURITY/TRUST BOUNDARY: unlike git-sync provenance —
// which the server sets authoritatively from its own configuration — these
// values are CLIENT-ASSERTED and NOT authenticated. SourceID is advisory
// ownership only (an Operator who knows a git-sync SourceID could assert it; no
// worse than the existing --force override, which the same RoleOperator already
// has), and Commit is an unverified label that flows into dag_snapshot.git_commit
// (so `caesium blame` attribution for an API-applied job reflects the asserted
// commit, not a server-verified one). Treat blame as a tamper-evident audit
// record only when provenance came from git-sync, not an API client.
type ApplyProvenance struct {
	SourceID string `json:"source_id,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Ref      string `json:"ref,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Path     string `json:"path,omitempty"`
}

type ApplyResponse struct {
	Applied          int                               `json:"applied"`
	Pruned           int                               `json:"pruned,omitempty"`
	ContractWarnings []contractenforce.ContractWarning `json:"contract_warnings,omitempty"`
}

// AllowBreakingRequest requests a bounded contract-break acknowledgement for
// one declared dataset or inferred producer.output.<key> subject.
type AllowBreakingRequest struct {
	Dataset string `json:"dataset"`
	Reason  string `json:"reason,omitempty"`
}

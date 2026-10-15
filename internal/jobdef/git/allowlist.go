package git

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
)

// ManifestViolation is one manifest a namespace-restricted source refused.
type ManifestViolation struct {
	Path            string `json:"path"`
	Alias           string `json:"alias"`
	Namespace       string `json:"namespace"`
	StoredNamespace string `json:"stored_namespace,omitempty"`
	Reason          string `json:"reason"` // jobdef.NamespaceReason*
}

// SyncReport summarises one sync of a source.
type SyncReport struct {
	SourceID     string
	URL          string // credentials, query and fragment removed
	Ref          string
	Commit       string
	Namespaces   []string // nil = unrestricted
	Applied      int
	Violations   []ManifestViolation
	Pruned       int
	PruneSkipped []jobdef.PruneSkip
}

// applyHookForTest, when set, runs just before a restricted source's batch
// transaction so a package test can move a row after the stored-namespace
// pre-filter. Always nil in production.
var applyHookForTest func()

// applyAtomically applies a restricted source's allowed manifests in one
// transaction: all of them or none. A manifest the in-transaction guard
// refuses (its row moved after the pre-filter) is skipped and reported, and
// the batch commits only if the stored graph it leaves is still valid. The
// trade-off: the database writer is held for the whole batch, once per new
// commit and only for allowlisted sources; unrestricted sources keep the
// per-manifest transactions.
func (s *Source) applyAtomically(ctx context.Context, importer *jobdef.Importer, plans []applyFilePlan, report *SyncReport) error {
	if len(plans) == 0 {
		return nil
	}
	if applyHookForTest != nil {
		applyHookForTest()
	}
	batch := make([]jobdef.BatchPlan, 0, len(plans))
	for _, plan := range plans {
		batch = append(batch, jobdef.BatchPlan{Def: plan.def, Opts: plan.opts})
	}
	result, err := importer.ApplyBatchWithOptions(ctx, batch)
	refused := make([]string, 0, len(result.Refused))
	for _, refusal := range result.Refused {
		nsErr := refusal.Err
		s.reportViolation(report, plans[refusal.Index].path, nsErr.Alias, nsErr.RequestedNamespace, nsErr.StoredNamespace, nsErr.Reason)
		refused = append(refused, nsErr.Alias)
	}
	if err != nil {
		if len(refused) > 0 && errors.Is(err, jobdef.ErrBatchInvalid) {
			return fmt.Errorf("git sync %s commit %s: namespace refusal of %q leaves the batch invalid, so nothing from this commit was applied: %w (fix the repository or move the job back into the allowlist)",
				report.SourceID, report.Commit, refused, err)
		}
		return err
	}
	report.Applied = result.Applied
	return nil
}

// rejectDuplicateAliases runs over every collected manifest, before any
// namespace filtering, so a refused copy cannot hide a duplicate. It is fatal,
// as ValidateBatch makes a duplicate for an unrestricted source.
func rejectDuplicateAliases(plans []applyFilePlan) error {
	seen := make(map[string]string, len(plans))
	for _, plan := range plans {
		alias := strings.TrimSpace(plan.def.Metadata.Alias)
		if alias == "" {
			continue // Validate reports it.
		}
		if first, ok := seen[alias]; ok {
			return fmt.Errorf("%w: %s (%s and %s)", jobdef.ErrDuplicateJob, alias, first, plan.path)
		}
		seen[alias] = plan.path
	}
	return nil
}

// allowedPlans drops every manifest the apply guard would refuse, on its
// requested namespace or on its stored row's, so that neither its proposal nor
// its validation can fail the sync and ValidateBatch sees the stored row that
// stays in place.
func (s *Source) allowedPlans(ctx context.Context, importer *jobdef.Importer, plans []applyFilePlan, report *SyncReport) ([]applyFilePlan, error) {
	requested := make([]applyFilePlan, 0, len(plans))
	aliases := make([]string, 0, len(plans))
	for _, plan := range plans {
		ns := models.NamespaceOrDefault(plan.def.Metadata.Namespace)
		if !slices.Contains(s.Namespaces, ns) {
			s.reportViolation(report, plan.path, plan.def.Metadata.Alias, ns, "", jobdef.NamespaceReasonRequested)
			continue
		}
		requested = append(requested, plan)
		aliases = append(aliases, plan.def.Metadata.Alias)
	}
	stored, err := importer.StoredNamespaces(ctx, aliases)
	if err != nil {
		return nil, err
	}
	allowed := make([]applyFilePlan, 0, len(requested))
	for _, plan := range requested {
		if ns, ok := stored[plan.def.Metadata.Alias]; ok && !slices.Contains(s.Namespaces, ns) {
			s.reportViolation(report, plan.path, plan.def.Metadata.Alias,
				models.NamespaceOrDefault(plan.def.Metadata.Namespace), ns, jobdef.NamespaceReasonStored)
			continue
		}
		allowed = append(allowed, plan)
	}
	return allowed, nil
}

// skippedBatchError makes a batch validation failure actionable when manifests
// were skipped for their namespace: their stored rows stay live and take part
// in the validation, so a conflict with them (say a trigger cycle through a job
// moved out of the allowlist) fails the sync. Without skips the error is
// returned unchanged.
func skippedBatchError(report *SyncReport, err error) error {
	if len(report.Violations) == 0 {
		return err
	}
	aliases := make([]string, 0, len(report.Violations))
	for _, violation := range report.Violations {
		aliases = append(aliases, violation.Alias)
	}
	return fmt.Errorf("git sync %s commit %s: batch invalid while namespace-skipped manifests %q keep their stored jobs: %w (fix the repository or move the job back into the allowlist)",
		report.SourceID, report.Commit, aliases, err)
}

func (s *Source) reportViolation(report *SyncReport, path, alias, namespace, storedNamespace, reason string) {
	if reason != jobdef.NamespaceReasonStored {
		storedNamespace = ""
	}
	report.Violations = append(report.Violations, ManifestViolation{
		Path: path, Alias: alias, Namespace: namespace, StoredNamespace: storedNamespace, Reason: reason,
	})
	log.Warn("git sync skipped a manifest outside the source namespace allowlist",
		"source_id", report.SourceID, "url", report.URL, "commit", report.Commit, "path", path,
		"alias", alias, "namespace", namespace, "stored_namespace", storedNamespace, "reason", reason,
		"allowed", s.Namespaces)
}

// auditSync records the sync as system:git-sync. Best effort: a failed write
// is logged and never changes the sync result.
func (s *Source) auditSync(importer *jobdef.Importer, report SyncReport, syncErr error) {
	violations := report.Violations
	if violations == nil {
		violations = []ManifestViolation{}
	}
	skipped := report.PruneSkipped
	if skipped == nil {
		skipped = []jobdef.PruneSkip{}
	}
	metadata := map[string]any{
		"url": report.URL, "ref": report.Ref, "commit": report.Commit, "source_id": report.SourceID,
		"namespaces": report.Namespaces, "applied": report.Applied, "violations": violations,
		"pruned": report.Pruned, "prune_skipped": skipped,
	}
	outcome := auth.OutcomeSuccess
	switch {
	case syncErr != nil:
		outcome = auth.OutcomeError
		metadata["error"] = syncErr.Error()
	case len(violations) > 0 || len(skipped) > 0:
		outcome = auth.OutcomeDenied
	}
	resourceID := report.SourceID
	if resourceID == "" {
		resourceID = report.URL
	}
	if err := auth.NewAuditLogger(importer.DB()).Log(auth.AuditEntry{
		Actor: auth.ActorGitSync, Action: auth.ActionJobdefGitSync, ResourceType: "git_source",
		ResourceID: resourceID, Outcome: outcome, Metadata: metadata,
	}); err != nil {
		log.Warn("git sync audit write failed", "source_id", report.SourceID, "url", report.URL, "error", err)
	}
	log.Info("git sync finished", "source_id", report.SourceID, "url", report.URL, "commit", report.Commit,
		"outcome", outcome, "applied", report.Applied, "violations", len(violations),
		"pruned", report.Pruned, "prune_skipped", len(skipped))
}

// redactSourceURL drops credentials, query and fragment from a URL source.
// Paths and scp-style addresses (git@host:org/repo.git) come back unchanged.
func redactSourceURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		return raw
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}

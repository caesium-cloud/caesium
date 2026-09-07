package incident

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// playbook_resolve.go is the ONE place an incident's effective policy is
// derived. It lives here rather than beside either caller because there are two,
// and they must not disagree:
//
//   - the action executor ENFORCES the resolved playbook on every proposal;
//   - the triage bundle BRIEFS the agent with it.
//
// When those came from different code the agent was shown the deployment default
// profile's document while its proposals were judged by the job's — so an agent
// could reason correctly from its brief and still be denied, or worse, believe
// it was constrained when it was not.

// ResolvePlaybook returns the effective policy for an incident.
//
// Resolution, in order:
//
//  1. incident → job. The job's persisted `metadata.remediation` block
//     (models.Job.Remediation) is the job's policy.
//  2. That block names the AgentProfile whose playbook is the base; a job that
//     declares a block but no profile uses the deployment default. The block's
//     `autonomy` sub-block then resolves over that base (Playbook.Override).
//  3. A job with no remediation block at all falls back to the deployment
//     default profile: that is what "default profile" means, and the job has
//     expressed no policy to override it.
//
// Every failure fails CLOSED, with the severity matched to what was lost:
//
//   - the incident or job cannot be read → the zero Playbook (unconfigured:
//     tier 3 to a human, tier 2 denied, tier 0/1 autonomous);
//   - the job DECLARED a policy whose profile cannot be loaded → DenyAllPlaybook,
//     which permits no autonomous action at all. Falling back to the deployment
//     default there would substitute a policy the job explicitly replaced, in the
//     widening direction.
func ResolvePlaybook(ctx context.Context, db *gorm.DB, incidentID uuid.UUID, defaultProfile string) Playbook {
	var inc models.Incident
	if err := db.WithContext(ctx).Select("id", "job_id").First(&inc, "id = ?", incidentID).Error; err != nil {
		log.Warn("incident: could not load incident for playbook; failing closed",
			"incident_id", incidentID, "error", err)
		return Playbook{}
	}

	var job models.Job
	if err := db.WithContext(ctx).Select("id", "alias", "remediation").First(&job, "id = ?", inc.JobID).Error; err != nil {
		log.Warn("incident: could not load job for playbook; failing closed",
			"incident_id", incidentID, "job_id", inc.JobID, "error", err)
		return Playbook{}
	}

	if len(job.Remediation) == 0 {
		return defaultProfilePlaybook(ctx, db, incidentID, defaultProfile)
	}

	var block schema.MetadataRemediation
	if err := json.Unmarshal(job.Remediation, &block); err != nil {
		log.Warn("incident: could not decode job remediation policy; denying autonomous actions",
			"incident_id", incidentID, "job_id", inc.JobID, "error", err)
		return DenyAllPlaybook()
	}

	base, ok := profilePlaybook(ctx, db, incidentID, block.Profile, defaultProfile)
	if !ok {
		return DenyAllPlaybook()
	}

	// The job's own autonomy block is a valid playbook document (both are
	// pkg/jobdef.RemediationAutonomy's shape), so the shared decoder reads it.
	return base.Override(DecodePlaybook(job.Remediation))
}

// ResolveProfile returns the AgentProfile an incident's job names, falling back
// to the deployment default. It is what supplies the session IMAGE and limits —
// distinct from ResolvePlaybook, which supplies the enforced policy — and is
// best-effort: a nil profile means no session can be launched, not that the
// policy is unknown.
func ResolveProfile(ctx context.Context, db *gorm.DB, incidentID uuid.UUID, defaultProfile string) *models.AgentProfile {
	name := strings.TrimSpace(defaultProfile)

	var inc models.Incident
	if err := db.WithContext(ctx).Select("id", "job_id").First(&inc, "id = ?", incidentID).Error; err == nil {
		var job models.Job
		if err := db.WithContext(ctx).Select("id", "remediation").First(&job, "id = ?", inc.JobID).Error; err == nil && len(job.Remediation) > 0 {
			var block schema.MetadataRemediation
			if err := json.Unmarshal(job.Remediation, &block); err == nil && strings.TrimSpace(block.Profile) != "" {
				name = strings.TrimSpace(block.Profile)
			}
		}
	}
	if name == "" {
		return nil
	}

	var profile models.AgentProfile
	if err := db.WithContext(ctx).First(&profile, "name = ?", name).Error; err != nil {
		return nil
	}
	return &profile
}

// profilePlaybook loads a named AgentProfile's playbook. An empty name means the
// job deferred to the deployment default. The bool reports whether resolution
// SUCCEEDED — false is a hard failure the caller must fail closed on, and is
// deliberately distinct from "resolved to an unconfigured (zero) playbook".
func profilePlaybook(ctx context.Context, db *gorm.DB, incidentID uuid.UUID, name, defaultProfile string) (Playbook, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return defaultProfilePlaybook(ctx, db, incidentID, defaultProfile), true
	}
	var profile models.AgentProfile
	if err := db.WithContext(ctx).First(&profile, "name = ?", name).Error; err != nil {
		log.Warn("incident: could not load job-declared agent profile for playbook",
			"incident_id", incidentID, "profile", name, "error", err)
		return Playbook{}, false
	}
	return DecodePlaybook(profile.Playbook), true
}

// defaultProfilePlaybook resolves the deployment default profile's playbook. An
// unset or unreadable default yields the zero Playbook — never a permissive one.
func defaultProfilePlaybook(ctx context.Context, db *gorm.DB, incidentID uuid.UUID, defaultProfile string) Playbook {
	name := strings.TrimSpace(defaultProfile)
	if name == "" {
		return Playbook{}
	}
	var profile models.AgentProfile
	if err := db.WithContext(ctx).First(&profile, "name = ?", name).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Warn("incident: could not load default profile for playbook",
				"incident_id", incidentID, "profile", name, "error", err)
		}
		return Playbook{}
	}
	return DecodePlaybook(profile.Playbook)
}

// Document re-encodes a resolved Playbook into the stored document shape, so the
// triage bundle can show the agent EXACTLY the policy that will be enforced on
// its proposals rather than the raw profile document it was derived from.
//
// A nil Allow (unconfigured) is omitted; a configured-but-empty one is rendered
// as `[]`, because "grants nothing" and "not configured" are different policies
// and the agent must be able to tell them apart.
func (pb Playbook) Document() json.RawMessage {
	autonomy := map[string]any{}
	if pb.Allow != nil {
		autonomy["allow"] = sortedTrueKeys(pb.Allow)
	}
	if pb.RequireApproval != nil {
		autonomy["requireApproval"] = sortedTrueKeys(pb.RequireApproval)
	}
	if pb.ParamOverrides != nil {
		autonomy["paramOverrides"] = pb.ParamOverrides
	}
	encoded, err := json.Marshal(map[string]any{"autonomy": autonomy})
	if err != nil {
		return nil
	}
	return encoded
}

// sortedTrueKeys returns the set's members in a stable order, so a bundle served
// twice is byte-identical and a diff of two bundles is meaningful.
func sortedTrueKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k, v := range set {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

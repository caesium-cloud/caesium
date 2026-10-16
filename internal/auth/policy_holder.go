package auth

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// policyResourceType is the audit resource type of access policy reloads.
const policyResourceType = "access_policy"

// KnownUserGroups returns the policy loader's source for the unmatched-group
// lint warning: every group stored on any user. A row whose groups do not
// decode is skipped, as it grants nothing (resolveUserGrants).
func KnownUserGroups(db *gorm.DB) policy.KnownGroupsFunc {
	if db == nil {
		return nil
	}
	return func(ctx context.Context) (map[string]struct{}, error) {
		var rows []datatypes.JSON
		if err := db.WithContext(ctx).Model(&models.User{}).Pluck("groups", &rows).Error; err != nil {
			return nil, fmt.Errorf("read user groups: %w", err)
		}
		known := make(map[string]struct{})
		for _, raw := range rows {
			if len(raw) == 0 {
				continue
			}
			var groups []string
			if err := json.Unmarshal(raw, &groups); err != nil {
				continue
			}
			for _, group := range groups {
				known[group] = struct{}{}
			}
		}
		return known, nil
	}
}

// PolicyReloadAuditor adapts the policy holder's reload events to audit rows:
// policy.reloaded on success (with the new and previous revision hashes) and
// policy.reload_failed with the validation error. A failed audit write is
// logged and never affects the reload.
func PolicyReloadAuditor(auditor *AuditLogger) func(policy.ReloadEvent) {
	return func(event policy.ReloadEvent) {
		if auditor == nil {
			return
		}
		if err := auditor.Log(PolicyReloadAuditEntry(event)); err != nil {
			log.Warn("failed to write audit log", "action", policyReloadAction(event), "error", err)
		}
	}
}

// PolicyReloadAuditEntry returns the audit row for one reload event.
func PolicyReloadAuditEntry(event policy.ReloadEvent) AuditEntry {
	actor := event.Actor
	if actor == "" {
		actor = "unknown"
	}
	metadata := map[string]any{
		"trigger":           event.Trigger,
		"path":              event.Path,
		"revision":          event.Revision,
		"previous_revision": event.PreviousRevision,
	}
	outcome := OutcomeSuccess
	if event.OK {
		metadata["changed"] = event.Changed
	} else {
		outcome = OutcomeError
		metadata["error"] = event.Error
	}
	return AuditEntry{
		Actor:        actor,
		Action:       policyReloadAction(event),
		ResourceType: policyResourceType,
		ResourceID:   event.Revision,
		SourceIP:     event.SourceIP,
		Outcome:      outcome,
		Metadata:     metadata,
		// A policy reload is a cluster decision (A3's audit namespace rule).
		Namespace: models.ClusterNamespace,
	}
}

func policyReloadAction(event policy.ReloadEvent) string {
	if event.OK {
		return ActionPolicyReloaded
	}
	return ActionPolicyReloadFailed
}

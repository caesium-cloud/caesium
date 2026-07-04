package jobdef

import (
	"context"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gorm.io/gorm"
)

// ValidateAgentProfileRefs verifies that every server-side resource a
// metadata.remediation block references exists: the AgentProfile named by
// remediation.profile and the NotificationChannel named by
// remediation.escalation.channel. This is the server-side half of the lint
// split docs/design-agent-in-the-loop.md documents: pkg/jobdef's offline
// validation (which runs with conn == nil, e.g. `caesium job lint`) can only
// check the references are well-formed and emits a scope note, because these
// are server-side state. This function closes that gap and is invoked both
// from POST /v1/jobdefs/lint and from Importer.ValidateBatch inside the apply
// transaction, mirroring ValidateDatasetGraph's conn-may-be-nil posture.
//
// The built-in models.DefaultTriageOnlyProfileName is treated as always
// resolvable regardless of DB state: it is the shipped default jobs are
// advertised to reference, seeded lazily/idempotently — so a job referencing
// it must never be rejected merely because the row has not been materialized
// on this particular server yet.
func ValidateAgentProfileRefs(ctx context.Context, conn *gorm.DB, defs []schema.Definition) error {
	if conn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	profiles := make(map[string]struct{})
	channels := make(map[string]struct{})
	for i := range defs {
		r := defs[i].Metadata.Remediation
		if r == nil {
			continue
		}
		if profile := strings.TrimSpace(r.Profile); profile != "" && profile != models.DefaultTriageOnlyProfileName {
			profiles[profile] = struct{}{}
		}
		if r.Escalation != nil {
			if channel := strings.TrimSpace(r.Escalation.Channel); channel != "" {
				channels[channel] = struct{}{}
			}
		}
	}

	missing, err := findMissingNames(ctx, conn, &models.AgentProfile{}, profiles)
	if err != nil {
		return err
	}
	if missing != "" {
		return fmt.Errorf("metadata.remediation.profile %q does not reference an existing AgentProfile", missing)
	}

	missing, err = findMissingNames(ctx, conn, &models.NotificationChannel{}, channels)
	if err != nil {
		return err
	}
	if missing != "" {
		return fmt.Errorf("metadata.remediation.escalation.channel %q does not reference an existing notification channel", missing)
	}
	return nil
}

// findMissingNames returns the first name in want that has no matching row
// (by the name column) in the given model's table, or "" if all resolve.
func findMissingNames(ctx context.Context, conn *gorm.DB, model interface{}, want map[string]struct{}) (string, error) {
	if len(want) == 0 {
		return "", nil
	}

	lookup := make([]string, 0, len(want))
	for name := range want {
		lookup = append(lookup, name)
	}

	var names []string
	if err := conn.WithContext(ctx).Model(model).Where("name IN ?", lookup).Pluck("name", &names).Error; err != nil {
		return "", fmt.Errorf("validate remediation references: %w", err)
	}
	found := make(map[string]struct{}, len(names))
	for _, name := range names {
		found[name] = struct{}{}
	}

	for name := range want {
		if _, ok := found[name]; !ok {
			return name, nil
		}
	}
	return "", nil
}

package jobdef

import (
	"context"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"gorm.io/gorm"
)

// ValidateAgentProfileRefs verifies that every metadata.remediation.profile
// reference in the incoming definitions names an AgentProfile that exists.
// This is the server-side half of the lint split
// docs/design-agent-in-the-loop.md documents: pkg/jobdef's offline validation
// (which runs with conn == nil, e.g. `caesium job lint`) can only check the
// reference is well-formed and emits a scope note, because AgentProfile is
// server-side state. This function closes that gap and is invoked both from
// POST /v1/jobdefs/lint and from Importer.ValidateBatch inside the apply
// transaction, mirroring ValidateDatasetGraph's conn-may-be-nil posture.
func ValidateAgentProfileRefs(ctx context.Context, conn *gorm.DB, defs []schema.Definition) error {
	if conn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	names := make(map[string]struct{})
	for i := range defs {
		r := defs[i].Metadata.Remediation
		if r == nil {
			continue
		}
		profile := strings.TrimSpace(r.Profile)
		if profile == "" {
			continue
		}
		names[profile] = struct{}{}
	}
	if len(names) == 0 {
		return nil
	}

	lookup := make([]string, 0, len(names))
	for name := range names {
		lookup = append(lookup, name)
	}

	var rows []models.AgentProfile
	if err := conn.WithContext(ctx).Where("name IN ?", lookup).Find(&rows).Error; err != nil {
		return fmt.Errorf("validate agent profile references: %w", err)
	}
	found := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		found[row.Name] = struct{}{}
	}

	for name := range names {
		if _, ok := found[name]; !ok {
			return fmt.Errorf("metadata.remediation.profile %q does not reference an existing AgentProfile", name)
		}
	}
	return nil
}

package jobdef

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func remediationDefinition(alias, profile string) schema.Definition {
	return schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata: schema.Metadata{
			Alias: alias,
			Remediation: &schema.MetadataRemediation{
				Profile: profile,
				Classes: []string{schema.RemediationClassUnknown},
			},
		},
		Trigger: schema.Trigger{
			Type:          schema.TriggerCron,
			Configuration: map[string]any{"expression": "0 * * * *"},
		},
		Steps: []schema.Step{{Name: "extract", Image: "busybox:1.36.1"}},
	}
}

func TestValidateAgentProfileRefs_NilConnSkipsVerification(t *testing.T) {
	t.Parallel()

	defs := []schema.Definition{remediationDefinition("j", "does-not-exist")}
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), nil, defs))
}

func TestValidateAgentProfileRefs_NoRemediationBlockIsANoOp(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	defs := []schema.Definition{{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: "plain"},
		Trigger: schema.Trigger{
			Type:          schema.TriggerCron,
			Configuration: map[string]any{"expression": "0 * * * *"},
		},
		Steps: []schema.Step{{Name: "extract", Image: "busybox:1.36.1"}},
	}}
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, defs))
}

func TestValidateAgentProfileRefs_RejectsUnknownProfile(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	defs := []schema.Definition{remediationDefinition("j", "ghost-profile")}

	err := ValidateAgentProfileRefs(context.Background(), db, defs)
	require.Error(t, err)
	require.Contains(t, err.Error(), `"ghost-profile"`)
	require.Contains(t, err.Error(), "does not reference an existing AgentProfile")
}

func TestValidateAgentProfileRefs_AcceptsExistingProfile(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	require.NoError(t, db.Create(&models.AgentProfile{
		ID:    uuid.New(),
		Name:  "default-triage",
		Image: "caesiumcloud/triage-agent:latest",
	}).Error)

	defs := []schema.Definition{remediationDefinition("j", "default-triage")}
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, defs))
}

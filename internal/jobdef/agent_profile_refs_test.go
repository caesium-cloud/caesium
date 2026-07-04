package jobdef

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
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

// TestValidateAgentProfileRefs_TriageOnlyAlwaysValid proves the shipped
// default profile resolves even against a DB where no row has been seeded, so
// lint/apply of a job referencing it never fails on a fresh server.
func TestValidateAgentProfileRefs_TriageOnlyAlwaysValid(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	defs := []schema.Definition{remediationDefinition("j", models.DefaultTriageOnlyProfileName)}
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, defs))
}

func remediationDefinitionWithEscalation(alias, profile, channel string) schema.Definition {
	def := remediationDefinition(alias, profile)
	def.Metadata.Remediation.Escalation = &schema.RemediationEscalation{
		Channel: channel,
		After:   "15m",
	}
	return def
}

func TestValidateAgentProfileRefs_RejectsUnknownEscalationChannel(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	require.NoError(t, db.Create(&models.AgentProfile{
		ID:    uuid.New(),
		Name:  "default-triage",
		Image: "caesiumcloud/triage-agent:latest",
	}).Error)

	defs := []schema.Definition{remediationDefinitionWithEscalation("j", "default-triage", "ghost-channel")}
	err := ValidateAgentProfileRefs(context.Background(), db, defs)
	require.Error(t, err)
	require.Contains(t, err.Error(), `"ghost-channel"`)
	require.Contains(t, err.Error(), "does not reference an existing notification channel")
}

func TestValidateAgentProfileRefs_AcceptsExistingEscalationChannel(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	require.NoError(t, db.Create(&models.AgentProfile{
		ID:    uuid.New(),
		Name:  "default-triage",
		Image: "caesiumcloud/triage-agent:latest",
	}).Error)
	require.NoError(t, db.Create(&models.NotificationChannel{
		ID:     uuid.New(),
		Name:   "data-oncall",
		Type:   models.ChannelTypeWebhook,
		Config: datatypes.JSON(`{"url":"https://example.com/hook"}`),
	}).Error)

	defs := []schema.Definition{remediationDefinitionWithEscalation("j", "default-triage", "data-oncall")}
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, defs))
}

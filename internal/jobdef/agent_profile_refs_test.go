package jobdef

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
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

// --- tenancy namespace visibility (identity-and-access D3) ---

func sharedNSDefinition(alias, namespace, profile, channel string) schema.Definition {
	def := remediationDefinition(alias, profile)
	def.Metadata.Namespace = namespace
	if channel != "" {
		def.Metadata.Remediation.Escalation = &schema.RemediationEscalation{Channel: channel, After: "15m"}
	}
	return def
}

func sharedNSProfile(t *testing.T, db *gorm.DB, name, namespace string) {
	t.Helper()
	require.NoError(t, db.Create(&models.AgentProfile{ID: uuid.New(), Namespace: namespace, Name: name, Image: "caesiumcloud/triage-agent:latest"}).Error)
}

func sharedNSChannel(t *testing.T, db *gorm.DB, name, namespace string) {
	t.Helper()
	require.NoError(t, db.Create(&models.NotificationChannel{ID: uuid.New(), Namespace: namespace, Name: name, Type: models.ChannelTypeWebhook, Config: datatypes.JSON(`{"url":"https://example.com/hook"}`)}).Error)
}

func TestValidateAgentProfileRefs_RejectsCrossNamespaceProfile(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, "fin-profile", "finance")

	err := ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("mkt-job", "marketing", "fin-profile", "")})
	require.Error(t, err)
	require.Contains(t, err.Error(), `"fin-profile"`)
	require.Contains(t, err.Error(), `"finance"`)
	require.Contains(t, err.Error(), `"marketing"`)
	require.Contains(t, err.Error(), `"mkt-job"`)
	require.Contains(t, err.Error(), "metadata.remediation.profile")
}

func TestValidateAgentProfileRefs_AcceptsSharedProfile(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, "shared-profile", models.ClusterNamespace)

	for _, namespace := range []string{"marketing", "finance", "default", ""} {
		require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", namespace, "shared-profile", "")}), namespace)
	}
}

func TestValidateAgentProfileRefs_AcceptsSameNamespaceProfile(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, "fin-profile", "finance")
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("fin-job", "finance", "fin-profile", "")}))
}

// POST /v1/jobdefs/lint validates copies, so a definition may reach the check
// with an empty namespace: it must be treated as default.
func TestValidateAgentProfileRefs_UnnormalizedJobNamespaceIsDefault(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, "default-profile", "")
	sharedNSProfile(t, db, "fin-profile", "finance")

	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", "", "default-profile", "")}))
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", "  ", "default-profile", "")}))

	err := ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", "", "fin-profile", "")})
	require.Error(t, err)
	require.Contains(t, err.Error(), `(namespace "default")`)

	// A default-namespace object is not visible from another namespace.
	err = ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", "marketing", "default-profile", "")})
	require.Error(t, err)
	require.Contains(t, err.Error(), `AgentProfile in namespace "default"`)
}

func TestValidateAgentProfileRefs_RejectsCrossNamespaceEscalationChannel(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, "shared-profile", models.ClusterNamespace)
	sharedNSChannel(t, db, "fin-oncall", "finance")

	err := ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("mkt-job", "marketing", "shared-profile", "fin-oncall")})
	require.Error(t, err)
	require.Contains(t, err.Error(), `"fin-oncall"`)
	require.Contains(t, err.Error(), `"finance"`)
	require.Contains(t, err.Error(), `"marketing"`)
	require.Contains(t, err.Error(), "metadata.remediation.escalation.channel")
	require.Contains(t, err.Error(), "notification channel in namespace")
}

func TestValidateAgentProfileRefs_AcceptsSharedEscalationChannel(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, "mkt-profile", "marketing")
	sharedNSChannel(t, db, "shared-oncall", models.ClusterNamespace)
	sharedNSChannel(t, db, "mkt-oncall", "marketing")

	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{
		sharedNSDefinition("a", "marketing", "mkt-profile", "shared-oncall"),
		sharedNSDefinition("b", "marketing", "mkt-profile", "mkt-oncall"),
	}))
}

// The built-in triage-only profile is cluster-shared; once a row exists it is
// checked like any other profile, so a row moved out of "*" is not silently
// usable from every namespace.
func TestValidateAgentProfileRefs_TriageOnlyRowInForeignNamespaceRejected(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, models.DefaultTriageOnlyProfileName, "finance")

	err := ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", "marketing", models.DefaultTriageOnlyProfileName, "")})
	require.Error(t, err)
	require.Contains(t, err.Error(), `"`+models.DefaultTriageOnlyProfileName+`"`)
	require.Contains(t, err.Error(), `"finance"`)
	require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", "finance", models.DefaultTriageOnlyProfileName, "")}))
}

func TestValidateAgentProfileRefs_TriageOnlySharedRowAccepted(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, models.DefaultTriageOnlyProfileName, models.ClusterNamespace)

	for _, namespace := range []string{"marketing", "default", ""} {
		require.NoError(t, ValidateAgentProfileRefs(context.Background(), db, []schema.Definition{sharedNSDefinition("j", namespace, models.DefaultTriageOnlyProfileName, "")}), namespace)
	}
}

// Violations are reported in definition order, profile before channel, so the
// error is deterministic (the previous map-iteration lookup was not).
func TestValidateAgentProfileRefs_ReportsFirstViolationInDefinitionOrder(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	sharedNSProfile(t, db, "shared-profile", models.ClusterNamespace)
	sharedNSProfile(t, db, "fin-profile", "finance")
	sharedNSChannel(t, db, "fin-oncall", "finance")

	defs := []schema.Definition{
		sharedNSDefinition("ok", "marketing", "shared-profile", ""),
		sharedNSDefinition("second", "marketing", "shared-profile", "fin-oncall"),
		sharedNSDefinition("third", "marketing", "fin-profile", ""),
		sharedNSDefinition("fourth", "marketing", "ghost-profile", ""),
	}
	for range 20 {
		err := ValidateAgentProfileRefs(context.Background(), db, defs)
		require.Error(t, err)
		require.Contains(t, err.Error(), `job "second"`)
		require.Contains(t, err.Error(), `"fin-oncall"`)
	}

	both := []schema.Definition{sharedNSDefinition("both", "marketing", "fin-profile", "fin-oncall")}
	err := ValidateAgentProfileRefs(context.Background(), db, both)
	require.Error(t, err)
	require.Contains(t, err.Error(), "metadata.remediation.profile", "profile is checked before channel")

	missingFirst := []schema.Definition{
		sharedNSDefinition("missing", "marketing", "ghost-profile", ""),
		sharedNSDefinition("foreign", "marketing", "fin-profile", ""),
	}
	err = ValidateAgentProfileRefs(context.Background(), db, missingFirst)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not reference an existing AgentProfile")
}

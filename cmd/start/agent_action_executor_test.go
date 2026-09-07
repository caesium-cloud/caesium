package start

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// The tier-2 action these tests pivot on. Tier 2 is the interesting case: it is
// autonomous ONLY under an explicit allowlist entry, so "which playbook governs"
// is the whole difference between a human deciding and a container acting.
const tier2Action = "pause_job"

// permissivePlaybook allows the tier-2 action outright.
const permissivePlaybook = `{"autonomy":{"allow":["pause_job","retry_from_failure"]}}`

// restrictivePlaybook allows only a tier-1 retry — never the tier-2 pause.
const restrictivePlaybook = `{"autonomy":{"allow":["retry_from_failure"]}}`

func mkProfile(t *testing.T, db *gorm.DB, name, playbook string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.AgentProfile{
		ID:        uuid.New(),
		Name:      name,
		Image:     "caesium/triage:latest",
		Engine:    models.AtomEngineDocker,
		Playbook:  datatypes.JSON(playbook),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
}

// mkIncidentForJob creates a job (optionally carrying a persisted remediation
// block) and an incident against it, returning the incident id.
func mkIncidentForJob(t *testing.T, db *gorm.DB, alias, remediation string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()

	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     alias,
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if remediation != "" {
		job.Remediation = datatypes.JSON(remediation)
	}
	require.NoError(t, db.Create(job).Error)

	incidentID := uuid.New()
	require.NoError(t, db.Create(&models.Incident{
		ID:        incidentID,
		JobID:     job.ID,
		Class:     "unknown",
		Status:    models.IncidentStatusOpen,
		DedupeKey: alias + ":unknown",
		OpenedAt:  now,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return incidentID
}

func newPlaybookTest(t *testing.T, defaultProfile string) (*gorm.DB, *agentActionExecutor) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	t.Setenv("CAESIUM_AGENT_DEFAULT_PROFILE", defaultProfile)
	require.NoError(t, env.Process())
	t.Cleanup(func() { _ = env.Process() })

	return db, newAgentActionExecutor(db, nil)
}

// TestJobRestrictedPolicyDeniesWhatTheDefaultProfileAllows is the security
// regression this file exists for. The deployment default permits the tier-2
// action; the JOB's own remediation policy does not. Resolving the playbook from
// the default — which is what this did before — let the agent pause a job whose
// declared policy forbids it, with no human in the loop.
func TestJobRestrictedPolicyDeniesWhatTheDefaultProfileAllows(t *testing.T) {
	db, exec := newPlaybookTest(t, "permissive-default")
	mkProfile(t, db, "permissive-default", permissivePlaybook)
	mkProfile(t, db, "locked-down", restrictivePlaybook)

	// Control: a job with no policy of its own inherits the deployment default,
	// which allows the tier-2 action.
	unscoped := mkIncidentForJob(t, db, "unscoped-job", "")
	require.True(t, exec.playbook(context.Background(), unscoped).Allow[tier2Action],
		"a job with no policy is governed by the deployment default")

	// The job under test names a restrictive profile.
	scoped := mkIncidentForJob(t, db, "scoped-job", `{"profile":"locked-down"}`)
	pb := exec.playbook(context.Background(), scoped)
	require.False(t, pb.Allow[tier2Action],
		"the job's own policy must govern, not the permissive deployment default")
	require.True(t, pb.Allow["retry_from_failure"],
		"the job's policy still allows what it declared")
}

// TestJobAutonomyBlockNarrowsItsProfile: a job may name a permissive profile and
// still narrow it with its own autonomy block. Neither document can grant what
// the other withholds.
func TestJobAutonomyBlockNarrowsItsProfile(t *testing.T) {
	db, exec := newPlaybookTest(t, "")
	mkProfile(t, db, "permissive-default", permissivePlaybook)

	incidentID := mkIncidentForJob(t, db, "narrowing-job",
		`{"profile":"permissive-default","autonomy":{"allow":["retry_from_failure"]}}`)

	pb := exec.playbook(context.Background(), incidentID)
	require.False(t, pb.Allow[tier2Action],
		"the job's autonomy block must narrow its profile's wider allowlist")
	require.True(t, pb.Allow["retry_from_failure"])
}

// TestJobRequireApprovalIsUnioned: a job may force an otherwise-autonomous
// action through the approval gate even when its profile allows it.
func TestJobRequireApprovalIsUnioned(t *testing.T) {
	db, exec := newPlaybookTest(t, "")
	mkProfile(t, db, "permissive-default", permissivePlaybook)

	incidentID := mkIncidentForJob(t, db, "gated-job",
		`{"profile":"permissive-default","autonomy":{"requireApproval":["retry_from_failure"]}}`)

	pb := exec.playbook(context.Background(), incidentID)
	require.True(t, pb.RequireApproval["retry_from_failure"],
		"a job may force an action through a human even if its profile allows it")
}

// TestUnresolvableJobPolicyDeniesEverything: when a job DECLARES a policy whose
// profile cannot be loaded, falling back to the deployment default would enforce
// a policy the job explicitly replaced — in the widening direction. Denying is
// the only answer that does not substitute a different policy.
func TestUnresolvableJobPolicyDeniesEverything(t *testing.T) {
	db, exec := newPlaybookTest(t, "permissive-default")
	mkProfile(t, db, "permissive-default", permissivePlaybook)

	incidentID := mkIncidentForJob(t, db, "dangling-job", `{"profile":"no-such-profile"}`)

	pb := exec.playbook(context.Background(), incidentID)
	// A NON-NIL Allow map is what makes this a denial rather than the unconfigured
	// default: nil means "not configured" (tier 0/1 autonomous), while a
	// configured-but-empty allowlist grants nothing at any tier.
	require.NotNil(t, pb.Allow, "denial must be expressed as a configured allowlist, not an unconfigured one")
	require.Empty(t, pb.Allow, "the deny-all allowlist names no action")
	require.False(t, pb.Allow[tier2Action])
	require.False(t, pb.Allow["retry_from_failure"],
		"an unresolvable declared policy must permit nothing autonomously, not even tier 1")
	require.Equal(t, incident.DenyAllPlaybook(), pb)
}

// TestUnknownIncidentFailsClosed: an incident that cannot be read yields the
// unconfigured zero playbook — tier 3 to a human, tier 2 needing an explicit
// allow — never the deployment default's allowlist.
func TestUnknownIncidentFailsClosed(t *testing.T) {
	db, exec := newPlaybookTest(t, "permissive-default")
	mkProfile(t, db, "permissive-default", permissivePlaybook)

	pb := exec.playbook(context.Background(), uuid.New())
	require.Empty(t, pb.Allow, "an unreadable incident must not inherit the default's allowlist")
	require.False(t, pb.Allow[tier2Action])
}

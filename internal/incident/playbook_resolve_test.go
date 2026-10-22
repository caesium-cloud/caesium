package incident

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Runtime profile resolution is namespace-aware (identity-and-access D4): an
// incident resolves only a profile in its own persisted namespace or "*", and
// refuses any other exactly as a missing one.

func seedNamespacedProfile(t *testing.T, db *gorm.DB, name, namespace string, allow ...string) {
	t.Helper()
	playbook, err := json.Marshal(map[string]any{"autonomy": map[string]any{"allow": allow}})
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.AgentProfile{
		ID:        uuid.New(),
		Namespace: namespace,
		Name:      name,
		Image:     "caesium/triage:latest",
		Engine:    models.AtomEngineDocker,
		Playbook:  datatypes.JSON(playbook),
	}).Error)
}

// withPolicyFile serves a policy file for the test: the namespace rule
// applies only while one is loaded (design §§1/15).
func withPolicyFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n  finance: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
}

func setJobProfile(t *testing.T, db *gorm.DB, jobID uuid.UUID, profile string) {
	t.Helper()
	block, err := json.Marshal(map[string]any{"profile": profile})
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", jobID).Update("remediation", datatypes.JSON(block)).Error)
}

func TestResolvePlaybookRefusesProfilesOutsideTheIncidentNamespace(t *testing.T) {
	withPolicyFile(t)
	db, store, _, _ := newExecutorTest(t)
	seedNamespacedProfile(t, db, "mkt-profile", "marketing", ActionTypePauseJob)
	seedNamespacedProfile(t, db, "fin-profile", "finance", ActionTypePauseJob)
	seedNamespacedProfile(t, db, "shared-profile", models.ClusterNamespace, ActionTypePauseJob)

	inc, _, _ := seedJobIncident(t, db, store, "gate", "")
	// The incident and its job live in marketing; the job may still name
	// another namespace's profile (applied before lint refused it). A job that
	// moved out is covered by TestResolvePlaybookMovedJobIgnoresRemediation.
	require.NoError(t, db.Model(&models.Incident{}).Where("id = ?", inc.ID).Update("namespace", "marketing").Error)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).Update("namespace", "marketing").Error)

	for _, tc := range []struct {
		profile string
		allowed bool
	}{
		{"mkt-profile", true},
		{"shared-profile", true},
		{"fin-profile", false},
	} {
		setJobProfile(t, db, inc.JobID, tc.profile)
		resolved := ResolvePlaybook(context.Background(), db, inc.ID, "")
		if tc.allowed {
			require.Equal(t, decisionExecute, resolved.decide(ActionTypePauseJob, TierGated), "%s resolves for a marketing incident", tc.profile)
			continue
		}
		require.Equal(t, DenyAllPlaybook(), resolved, "a job-declared profile outside the incident's namespace fails closed exactly as a missing one")
		require.Equal(t, decisionDeny, resolved.decide(ActionTypeEscalate, TierAutonomous))
	}
	// The same refusal as a profile that does not exist.
	setJobProfile(t, db, inc.JobID, "no-such-profile")
	require.Equal(t, DenyAllPlaybook(), ResolvePlaybook(context.Background(), db, inc.ID, ""))
}

func TestResolvePlaybookRefusesADefaultProfileOutsideTheIncidentNamespace(t *testing.T) {
	withPolicyFile(t)
	db, store, _, _ := newExecutorTest(t)
	seedNamespacedProfile(t, db, "corp-default", "finance", ActionTypePauseJob)
	seedNamespacedProfile(t, db, "shared-default", models.ClusterNamespace, ActionTypePauseJob)
	seedNamespacedProfile(t, db, "mkt-default", "marketing", ActionTypePauseJob)

	inc, _, _ := seedJobIncident(t, db, store, "gate", "")
	require.NoError(t, db.Model(&models.Incident{}).Where("id = ?", inc.ID).Update("namespace", "marketing").Error)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).Update("namespace", "marketing").Error)

	// No remediation block: the deployment default applies, but only when it
	// is visible from the incident. A namespace that created the configured
	// default name cannot author other namespaces' playbooks.
	refused := ResolvePlaybook(context.Background(), db, inc.ID, "corp-default")
	require.NotNil(t, refused.Allow, "a refused default fails closed: a configured, empty allow-list, not the unconfigured zero playbook")
	require.Empty(t, refused.Allow)
	require.Equal(t, decisionDeny, refused.decide(ActionTypeRetryFromFailure, TierAutonomous),
		"the zero playbook would run tier 1 autonomously, wider than the refused default")
	// A default that does not exist at all is still unconfigured, as before.
	require.Equal(t, Playbook{}, ResolvePlaybook(context.Background(), db, inc.ID, "no-such-default"))
	require.Equal(t, decisionExecute, ResolvePlaybook(context.Background(), db, inc.ID, "shared-default").decide(ActionTypePauseJob, TierGated))
	require.Equal(t, decisionExecute, ResolvePlaybook(context.Background(), db, inc.ID, "mkt-default").decide(ActionTypePauseJob, TierGated))

	// A job block naming no profile defers to the same default.
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).Update("remediation", datatypes.JSON(`{"classes":["unknown"]}`)).Error)
	foreign := ResolvePlaybook(context.Background(), db, inc.ID, "corp-default")
	require.Equal(t, decisionDeny, foreign.decide(ActionTypePauseJob, TierGated))
	require.Equal(t, decisionDeny, foreign.decide(ActionTypeRetryFromFailure, TierAutonomous),
		"the refused default denies tier 1 under a job block too")
	require.Equal(t, decisionExecute, ResolvePlaybook(context.Background(), db, inc.ID, "mkt-default").decide(ActionTypePauseJob, TierGated))

	// The job's own autonomy block must not widen a refused default: the
	// refusal fails resolution before any job override is applied.
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).
		Update("remediation", datatypes.JSON(`{"classes":["unknown"],"autonomy":{"allow":["retry_from_failure","pause_job"]}}`)).Error)
	overridden := ResolvePlaybook(context.Background(), db, inc.ID, "corp-default")
	require.Equal(t, DenyAllPlaybook(), overridden)
	require.Equal(t, decisionDeny, overridden.decide(ActionTypeRetryFromFailure, TierAutonomous))
	require.Equal(t, decisionExecute, ResolvePlaybook(context.Background(), db, inc.ID, "mkt-default").decide(ActionTypeRetryFromFailure, TierAutonomous),
		"a visible default still takes the job's override")
}

// TestProfileResolutionIgnoresNamespacesWithoutAPolicyFile: with no policy
// file (design §§1/15) profiles resolve by name alone, as before D4.
func TestProfileResolutionIgnoresNamespacesWithoutAPolicyFile(t *testing.T) {
	policy.SetProcessHolder(nil)
	db, store, _, _ := newExecutorTest(t)
	seedNamespacedProfile(t, db, "fin-profile", "finance", ActionTypePauseJob)

	inc, _, _ := seedJobIncident(t, db, store, "gate", "")
	require.NoError(t, db.Model(&models.Incident{}).Where("id = ?", inc.ID).Update("namespace", "marketing").Error)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).Update("namespace", "marketing").Error)

	require.Equal(t, decisionExecute, ResolvePlaybook(context.Background(), db, inc.ID, "fin-profile").decide(ActionTypePauseJob, TierGated),
		"the deployment default resolves by name")
	setJobProfile(t, db, inc.JobID, "fin-profile")
	require.Equal(t, decisionExecute, ResolvePlaybook(context.Background(), db, inc.ID, "").decide(ActionTypePauseJob, TierGated),
		"a job-declared profile resolves by name")
	require.Equal(t, "fin-profile", ResolveProfile(context.Background(), db, inc.ID, "").Name)
	require.Equal(t, "fin-profile", ResolveProfile(context.Background(), db, uuid.New(), "fin-profile").Name)
}

func TestResolveProfileRefusesProfilesOutsideTheIncidentNamespace(t *testing.T) {
	withPolicyFile(t)
	db, store, _, _ := newExecutorTest(t)
	seedNamespacedProfile(t, db, "mkt-profile", "marketing")
	seedNamespacedProfile(t, db, "fin-profile", "finance")
	seedNamespacedProfile(t, db, "shared-profile", models.ClusterNamespace)

	inc, _, _ := seedJobIncident(t, db, store, "gate", "")
	require.NoError(t, db.Model(&models.Incident{}).Where("id = ?", inc.ID).Update("namespace", "marketing").Error)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).Update("namespace", "marketing").Error)

	setJobProfile(t, db, inc.JobID, "fin-profile")
	require.Nil(t, ResolveProfile(context.Background(), db, inc.ID, ""), "a foreign job-declared profile is not used")
	setJobProfile(t, db, inc.JobID, "mkt-profile")
	require.Equal(t, "mkt-profile", ResolveProfile(context.Background(), db, inc.ID, "").Name)
	setJobProfile(t, db, inc.JobID, "shared-profile")
	require.Equal(t, "shared-profile", ResolveProfile(context.Background(), db, inc.ID, "").Name)

	// An incident that cannot be read resolves only a "*" default.
	unknown := uuid.New()
	require.Nil(t, ResolveProfile(context.Background(), db, unknown, "mkt-profile"))
	require.Equal(t, "shared-profile", ResolveProfile(context.Background(), db, unknown, "shared-profile").Name)
}

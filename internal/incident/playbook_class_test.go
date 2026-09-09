package incident

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestDecodePlaybookReadsPerClass is the regression at its narrowest: the
// decoder used to drop `perClass` on the floor, so a job that narrowed autonomy
// per failure class was enforced as if the block named no classes at all —
// fail-open (issue #416).
func TestDecodePlaybookReadsPerClass(t *testing.T) {
	pb := DecodePlaybook([]byte(`{"autonomy":{
		"allow":["retry_from_failure","pause_job"],
		"perClass":{"schema_violation":{"allow":["retry_from_failure"],"requireApproval":["retry_from_failure"]}}
	}}`))

	require.NotNil(t, pb.PerClass, "perClass must decode, not be silently discarded")
	constraint, ok := pb.PerClass["schema_violation"]
	require.True(t, ok)
	require.Equal(t, map[string]bool{ActionTypeRetryFromFailure: true}, constraint.Allow)
	require.Equal(t, map[string]bool{ActionTypeRetryFromFailure: true}, constraint.RequireApproval)

	// Absent means absent: no perClass key must not decode to an empty map, or
	// "declared no narrowing" and "declared narrowing for no class" would look
	// the same to Override.
	require.Nil(t, DecodePlaybook([]byte(`{"autonomy":{"allow":[]}}`)).PerClass)
}

// TestPlaybookForClassNarrowsOnlyTheMatchingClass covers the two halves of the
// feature: the incident's own class is narrowed, every other class inherits.
func TestPlaybookForClassNarrowsOnlyTheMatchingClass(t *testing.T) {
	pb := DecodePlaybook([]byte(`{"autonomy":{
		"allow":["retry_from_failure","pause_job"],
		"perClass":{"schema_violation":{"allow":["retry_from_failure"]}}
	}}`))

	narrowed := pb.ForClass(string(ClassSchemaViolation))
	require.Equal(t, decisionExecute, narrowed.decide(ActionTypeRetryFromFailure, TierAutonomous))
	require.Equal(t, decisionDeny, narrowed.decide(ActionTypePauseJob, TierGated),
		"the class block omits pause_job, so this class may not run it")

	inherited := pb.ForClass(string(ClassTransientInfra))
	require.Equal(t, decisionExecute, inherited.decide(ActionTypePauseJob, TierGated),
		"a class with no entry falls back to the base policy untouched")

	require.Nil(t, narrowed.PerClass, "ForClass consumes the constraint so decide() sees one policy")
	require.Nil(t, inherited.PerClass)

	// Idempotent: ResolvePlaybook narrows, then Execute narrows again.
	require.Equal(t, narrowed, narrowed.ForClass(string(ClassSchemaViolation)))
}

// TestPlaybookForClassCannotWiden is the property that makes per-class safe to
// honour at all: it is a constraint, never a grant, at every field.
func TestPlaybookForClassCannotWiden(t *testing.T) {
	t.Run("allow intersects rather than replaces", func(t *testing.T) {
		pb := DecodePlaybook([]byte(`{"autonomy":{
			"allow":["notify"],
			"perClass":{"oom":{"allow":["notify","pause_job"]}}
		}}`))
		narrowed := pb.ForClass(string(ClassOOM))
		require.Equal(t, decisionExecute, narrowed.decide(ActionTypeNotify, TierAutonomous))
		require.Equal(t, decisionDeny, narrowed.decide(ActionTypePauseJob, TierGated),
			"a class block may not grant an action the surrounding policy withholds")
	})

	t.Run("an unconfigured base is intersected against the tier default", func(t *testing.T) {
		// The trap: nil Allow means "tier defaults", which DENY tier 2. Adopting
		// the class list wholesale would grant pause_job here.
		pb := DecodePlaybook([]byte(`{"autonomy":{
			"perClass":{"quota":{"allow":["pause_job","notify"]}}
		}}`))
		require.Nil(t, pb.Allow)

		narrowed := pb.ForClass(string(ClassQuota))
		require.Equal(t, decisionDeny, narrowed.decide(ActionTypePauseJob, TierGated),
			"a class block cannot promote a tier-2 action under an unconfigured base")
		require.Equal(t, decisionExecute, narrowed.decide(ActionTypeNotify, TierAutonomous))
		require.Equal(t, decisionDeny, narrowed.decide(ActionTypeRetryFromFailure, TierAutonomous),
			"and the intersection still removes the tier-1 actions it omits")
	})

	t.Run("an unknown action type in a class allow is dropped", func(t *testing.T) {
		pb := Playbook{PerClass: map[string]Playbook{
			"oom": {Allow: map[string]bool{"definitely_not_a_real_action": true}},
		}}
		require.Empty(t, pb.ForClass(string(ClassOOM)).Allow)
	})

	t.Run("requireApproval unions", func(t *testing.T) {
		pb := DecodePlaybook([]byte(`{"autonomy":{
			"requireApproval":["pause_job"],
			"perClass":{"auth_failure":{"requireApproval":["retry_from_failure"]}}
		}}`))
		narrowed := pb.ForClass(string(ClassAuthFailure))
		require.Equal(t, decisionApprove, narrowed.decide(ActionTypeRetryFromFailure, TierAutonomous),
			"the class gate applies")
		require.Equal(t, decisionApprove, narrowed.decide(ActionTypePauseJob, TierGated),
			"and the base gate survives — a class block can never drop one")
	})

	t.Run("param overrides take the most restrictive", func(t *testing.T) {
		pb := DecodePlaybook([]byte(`{"autonomy":{
			"allow":["rerun_with_params"],
			"paramOverrides":{"mode":["full","incremental"],"region":["us","eu"]},
			"perClass":{"sla_risk":{"paramOverrides":{"mode":["incremental","dry-run"]}}}
		}}`))
		narrowed := pb.ForClass(string(ClassSLARisk))

		require.NoError(t, validateParamOverrides(map[string]string{"mode": "incremental"}, narrowed.ParamOverrides))
		require.Error(t, validateParamOverrides(map[string]string{"mode": "full"}, narrowed.ParamOverrides),
			"a value the class block omits is no longer whitelisted")
		require.Error(t, validateParamOverrides(map[string]string{"mode": "dry-run"}, narrowed.ParamOverrides),
			"nor may the class block add a value the base never permitted")
		require.Error(t, validateParamOverrides(map[string]string{"region": "us"}, narrowed.ParamOverrides),
			"a key the class whitelist omits is denied, not inherited")
	})

	t.Run("an emptied value intersection denies the key rather than reopening it", func(t *testing.T) {
		// An empty allowed-value list reads as "any value" in
		// validateParamOverrides, so the key must be DROPPED, not emptied.
		pb := DecodePlaybook([]byte(`{"autonomy":{
			"paramOverrides":{"mode":["full"]},
			"perClass":{"oom":{"paramOverrides":{"mode":["incremental"]}}}
		}}`))
		narrowed := pb.ForClass(string(ClassOOM))
		require.NotContains(t, narrowed.ParamOverrides, "mode")
		require.Error(t, validateParamOverrides(map[string]string{"mode": "full"}, narrowed.ParamOverrides))
		require.Error(t, validateParamOverrides(map[string]string{"mode": "incremental"}, narrowed.ParamOverrides))
	})

	t.Run("an unconfigured base whitelist is not opened by a class block", func(t *testing.T) {
		pb := DecodePlaybook([]byte(`{"autonomy":{
			"perClass":{"oom":{"paramOverrides":{"mode":["full"]}}}
		}}`))
		narrowed := pb.ForClass(string(ClassOOM))
		require.Nil(t, narrowed.ParamOverrides, "nil denies every key; a constraint cannot grant one")
		require.Error(t, validateParamOverrides(map[string]string{"mode": "full"}, narrowed.ParamOverrides))
	})
}

// TestPlaybookOverrideCombinesPerClass: the job block may widen its own base
// allow-list (an authored policy), but it may NOT use a per-class block to erase
// a narrowing the operator's profile declared. Both constraints survive and are
// applied together.
func TestPlaybookOverrideCombinesPerClass(t *testing.T) {
	profile := DecodePlaybook([]byte(`{"autonomy":{
		"allow":["retry_from_failure","pause_job","notify"],
		"perClass":{"auth_failure":{"requireApproval":["retry_from_failure"]}}
	}}`))
	job := DecodePlaybook([]byte(`{"autonomy":{
		"perClass":{
			"auth_failure":{"allow":["notify"]},
			"oom":{"allow":["retry_from_failure"]}
		}
	}}`))

	resolved := profile.Override(job)
	authFailure := resolved.ForClass(string(ClassAuthFailure))
	require.Equal(t, decisionApprove, authFailure.decide(ActionTypeRetryFromFailure, TierAutonomous),
		"the profile's per-class gate survives the job's per-class block")
	require.Equal(t, decisionDeny, authFailure.decide(ActionTypePauseJob, TierGated),
		"and the job's per-class allow still narrows")
	require.Equal(t, decisionExecute, authFailure.decide(ActionTypeNotify, TierAutonomous))

	oom := resolved.ForClass(string(ClassOOM))
	require.Equal(t, decisionExecute, oom.decide(ActionTypeRetryFromFailure, TierAutonomous),
		"a class only the job constrains still narrows")
	require.Equal(t, decisionDeny, oom.decide(ActionTypeNotify, TierAutonomous))

	require.Nil(t, Playbook{}.Override(Playbook{}).PerClass,
		"no per-class on either side stays nil")
}

// TestPlaybookDocumentRendersPerClass: Document promises the WHOLE policy. A
// resolved playbook has none left, but an unresolved one must not have its
// narrowing silently flattened away.
func TestPlaybookDocumentRendersPerClass(t *testing.T) {
	pb := DecodePlaybook([]byte(`{"autonomy":{
		"allow":["notify"],
		"perClass":{"oom":{"allow":["notify"],"requireApproval":["notify"]}}
	}}`))
	require.JSONEq(t,
		`{"autonomy":{"allow":["notify"],"perClass":{"oom":{"allow":["notify"],"requireApproval":["notify"]}}}}`,
		string(pb.Document()))
	require.Equal(t, pb, DecodePlaybook(pb.Document()), "the rendered document decodes back to the same policy")

	require.NotContains(t, string(pb.ForClass(string(ClassOOM)).Document()), "perClass",
		"a resolved playbook has no constraint left to advertise")
}

// TestExecuteAppliesPerClassNarrowing drives the executor: the same proposal,
// the same job policy, two incidents of different classes. This is the wiring
// the decoder gap made unreachable — decide() never saw the class.
func TestExecuteAppliesPerClassNarrowing(t *testing.T) {
	_, store, ops, exec := newExecutorTest(t)

	// seedIncident opens a transient_infra incident.
	inc, _ := seedIncident(t, store)

	pb := DecodePlaybook([]byte(`{"autonomy":{
		"allow":["retry_from_failure","pause_job"],
		"perClass":{"transient_infra":{"allow":["retry_from_failure"],"requireApproval":["retry_from_failure"]}}
	}}`))
	require.Equal(t, string(ClassTransientInfra), inc.Class)

	// retry_from_failure is allowed by the base, but the class block gates it.
	gated, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypeRetryFromFailure,
		Playbook:   pb,
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusProposed, gated.Status,
		"a class-gated action must park for a human, not execute")
	require.Empty(t, ops.retryFromFailure, "and must not have run")

	// pause_job is allowed by the base but omitted by the class allow-list.
	denied, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: inc.ID,
		Type:       ActionTypePauseJob,
		Playbook:   pb,
	})
	require.ErrorIs(t, err, ErrActionNotPermitted)
	require.Equal(t, models.AgentActionStatusRejected, denied.Status)
	require.Empty(t, ops.setPaused)

	// A different class is untouched by the block and runs under the base.
	otherRun := uuid.New()
	other, outcome, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  inc.JobID,
		RunID:                  &otherRun,
		TaskName:               "load",
		Class:                  ClassOOM,
		RemediationTargetRunID: &otherRun,
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)

	ran, err := exec.Execute(context.Background(), ActionRequest{
		IncidentID: other.ID,
		Type:       ActionTypeRetryFromFailure,
		Playbook:   pb,
	})
	require.NoError(t, err)
	require.Equal(t, models.AgentActionStatusExecuted, ran.Status,
		"a non-matching class falls back to the base policy")
	require.Equal(t, []uuid.UUID{otherRun}, ops.retryFromFailure)
}

// TestResolvePlaybookAppliesPerClass proves the resolver — the single place the
// enforced policy and the agent's brief both come from — narrows by the
// incident's class, so the two cannot disagree about it.
func TestResolvePlaybookAppliesPerClass(t *testing.T) {
	db, store, _, _ := newExecutorTest(t)

	require.NoError(t, db.Create(&models.AgentProfile{
		ID:     uuid.New(),
		Name:   "perclass-profile",
		Image:  "caesium/triage:latest",
		Engine: models.AtomEngineDocker,
		Playbook: datatypes.JSON(`{"autonomy":{
			"allow":["retry_from_failure","pause_job"]
		}}`),
	}).Error)

	// seedJobIncident opens an `unknown` incident against a real job row.
	inc, _, _ := seedJobIncident(t, db, store, "gate", "")
	require.Equal(t, string(ClassUnknown), inc.Class)

	block, err := json.Marshal(map[string]any{
		"profile": "perclass-profile",
		"classes": []string{"unknown"},
		"autonomy": map[string]any{
			"perClass": map[string]any{
				"unknown": map[string]any{"requireApproval": []string{ActionTypeRetryFromFailure}},
			},
		},
	})
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).
		Update("remediation", datatypes.JSON(block)).Error)

	resolved := ResolvePlaybook(context.Background(), db, inc.ID, "")
	require.Nil(t, resolved.PerClass, "the resolver hands the executor an already-narrowed policy")
	require.Equal(t, decisionApprove, resolved.decide(ActionTypeRetryFromFailure, TierAutonomous),
		"the job's per-class gate for THIS incident's class must be enforced")
	require.Equal(t, decisionExecute, resolved.decide(ActionTypePauseJob, TierGated),
		"everything the class block does not mention is inherited from the profile")

	// An incident of a class the block does not name resolves to the base.
	otherRun := uuid.New()
	other, _, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  inc.JobID,
		RunID:                  &otherRun,
		TaskName:               "load",
		Class:                  ClassOOM,
		RemediationTargetRunID: &otherRun,
	})
	require.NoError(t, err)

	fallback := ResolvePlaybook(context.Background(), db, other.ID, "")
	require.Equal(t, decisionExecute, fallback.decide(ActionTypeRetryFromFailure, TierAutonomous),
		"a non-matching class is enforced under the unnarrowed policy")
}

// TestPersistedEmptyPerClassAllowStillDenies is the reviewer's P1 reproduction
// (PR #452), driven apply → persisted remediation → resolved class policy
// rather than over hand-written JSON.
//
// `perClass.auth_failure.allow: []` is a CONFIGURED deny-all list, and
// pkg/jobdef.Parse preserves it as an empty non-nil slice. But the importer
// persists the block by marshalling it into models.Job.Remediation, and
// `json:"allow,omitempty"` drops an empty slice — so the stored class was `{}`,
// DecodePlaybook rebuilt `Allow == nil` ("no constraint"), and ForClass
// inherited the surrounding permissions. A job allowing pause_job globally
// would autonomously pause on auth_failure despite an explicit empty class
// allowlist: the fail-open direction, reached through serialization rather than
// through the merge rule.
func TestPersistedEmptyPerClassAllowStillDenies(t *testing.T) {
	db, store, _, _ := newExecutorTest(t)

	const manifest = `
apiVersion: v1
kind: Job
metadata:
  alias: perclass-empty-allow
  remediation:
    profile: perclass-empty-profile
    classes: [auth_failure, unknown]
    autonomy:
      allow: [pause_job, notify]
      perClass:
        auth_failure:
          allow: []
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: busybox:1.36.1
`
	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err)

	// Parsing preserves the distinction the policy model rests on…
	authFailure := def.Metadata.Remediation.Autonomy.PerClass["auth_failure"]
	require.NotNil(t, authFailure.Allow, "parse must keep `allow: []` as a configured empty list")
	require.Empty(t, authFailure.Allow)

	// …and persistence must too. This is the exact call the importer makes
	// (internal/jobdef.marshalOptionalJSON → json.Marshal), so a tag that drops
	// an empty slice here is a policy silently widened on the way to the column.
	persisted, err := json.Marshal(def.Metadata.Remediation)
	require.NoError(t, err)

	var reloaded schema.MetadataRemediation
	require.NoError(t, json.Unmarshal(persisted, &reloaded))
	require.NotNil(t, reloaded.Autonomy.PerClass["auth_failure"].Allow,
		"an explicitly empty per-class allow-list must survive persistence: %s", persisted)

	require.NoError(t, db.Create(&models.AgentProfile{
		ID:     uuid.New(),
		Name:   "perclass-empty-profile",
		Image:  "caesium/triage:latest",
		Engine: models.AtomEngineDocker,
	}).Error)

	inc, _, _ := seedJobIncident(t, db, store, "extract", "")
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", inc.JobID).
		Update("remediation", datatypes.JSON(persisted)).Error)

	// The enforcement half, through the real resolver.
	authRun := uuid.New()
	authIncident, _, err := store.OpenOrAppend(context.Background(), OpenParams{
		JobID:                  inc.JobID,
		RunID:                  &authRun,
		TaskName:               "extract-auth",
		Class:                  ClassAuthFailure,
		RemediationTargetRunID: &authRun,
	})
	require.NoError(t, err)

	resolved := ResolvePlaybook(context.Background(), db, authIncident.ID, "")
	require.Equal(t, decisionDeny, resolved.decide(ActionTypePauseJob, TierGated),
		"an explicit `allow: []` for this class must deny what the job allows globally")
	require.Equal(t, decisionDeny, resolved.decide(ActionTypeNotify, TierAutonomous),
		"`allow: []` grants nothing, including tier 1")

	// A class the block does not name still gets the job's own allow-list, so
	// the fix narrows the named class only.
	unknown := ResolvePlaybook(context.Background(), db, inc.ID, "")
	require.Equal(t, decisionExecute, unknown.decide(ActionTypePauseJob, TierGated),
		"an unnamed class must keep the surrounding policy")
}

// TestPersistedEmptyAutonomyConstraintsSurvive covers the same serialization
// hazard at the other two per-class fields and at the TOP-LEVEL autonomy block,
// where `allow: []` has always been the difference between the shipped
// triage-only posture ("grants nothing") and "unconfigured" (tier 0/1
// autonomous). The reviewer asked whether the omission affected the outer block
// too; it did, and this pins all of it.
func TestPersistedEmptyAutonomyConstraintsSurvive(t *testing.T) {
	const manifest = `
apiVersion: v1
kind: Job
metadata:
  alias: empty-constraints
  remediation:
    profile: triage-only
    classes: [auth_failure]
    autonomy:
      allow: []
      requireApproval: []
      perClass:
        auth_failure:
          allow: []
          requireApproval: []
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: busybox:1.36.1
`
	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err)

	persisted, err := json.Marshal(def.Metadata.Remediation)
	require.NoError(t, err)

	var reloaded schema.MetadataRemediation
	require.NoError(t, json.Unmarshal(persisted, &reloaded))

	require.NotNil(t, reloaded.Autonomy.Allow, "autonomy.allow: [] must survive: %s", persisted)
	require.Empty(t, reloaded.Autonomy.Allow)
	require.NotNil(t, reloaded.Autonomy.RequireApproval, "autonomy.requireApproval: [] must survive: %s", persisted)

	class := reloaded.Autonomy.PerClass["auth_failure"]
	require.NotNil(t, class.Allow, "perClass allow: [] must survive: %s", persisted)
	require.NotNil(t, class.RequireApproval, "perClass requireApproval: [] must survive: %s", persisted)

	// And the enforcement input rebuilt from those bytes is the configured,
	// deny-everything policy the author wrote — not the unconfigured one.
	pb := DecodePlaybook(persisted)
	require.NotNil(t, pb.Allow, "a persisted `allow: []` must decode as CONFIGURED, not unconfigured")
	require.Equal(t, decisionDeny, pb.ForClass(string(ClassAuthFailure)).decide(ActionTypeNotify, TierAutonomous))
	require.Equal(t, decisionDeny, pb.ForClass(string(ClassOOM)).decide(ActionTypeNotify, TierAutonomous))

	// An ABSENT block must still decode as unconfigured — the fix must preserve
	// the distinction, not collapse it the other way.
	absent, err := json.Marshal(&schema.MetadataRemediation{
		Profile:  "triage-only",
		Classes:  []string{"unknown"},
		Autonomy: &schema.RemediationAutonomy{},
	})
	require.NoError(t, err)
	require.NotContains(t, string(absent), "allow",
		"an unset field must stay ABSENT from the document, not become null: %s", absent)
	require.Nil(t, DecodePlaybook(absent).Allow)
}

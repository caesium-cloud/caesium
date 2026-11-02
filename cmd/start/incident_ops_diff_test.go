package start

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	"github.com/caesium-cloud/caesium/internal/incident"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

const patchedJob = `apiVersion: v1
kind: Job
metadata:
  alias: orders-daily
  namespace: marketing
trigger:
  type: cron
  configuration:
    cron: "0 6 * * *"
steps:
  - name: load
    image: busybox:1.36.1
    command: ["true"]
    env:
      REGION: eu
      STORE: secret://env/STORE_URL
      LITERAL: current-literal-value
    mounts:
      - type: bind
        source: /data
        target: /in
    next: [publish]
  - name: publish
    image: busybox:1.36.1
    command: ["true"]
`

type previewRig struct {
	db    *gorm.DB
	job   models.Job
	inc   *models.Incident
	ops   *incidentActionOps
	exec  *incident.Executor
	store *incident.Store
}

// newPreviewRig applies patchedJob through the importer and opens an incident on
// it, so a proposal runs through the real Executor and the real ActionOps.
func newPreviewRig(t *testing.T, manifest string) *previewRig {
	t.Helper()
	withAuthMode(t, "api-key")
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err)
	applied, err := internaljobdef.NewImporter(db).Apply(context.Background(), def)
	require.NoError(t, err)

	store := incident.NewStore(db)
	runID := uuid.New()
	inc, _, err := store.OpenOrAppend(context.Background(), incident.OpenParams{
		JobID:                  applied.ID,
		RunID:                  &runID,
		TaskName:               "load",
		Class:                  incident.ClassTransientInfra,
		Namespace:              def.Metadata.Namespace,
		RemediationTargetRunID: &runID,
	})
	require.NoError(t, err)
	ops := newIncidentActionOps(db, nil, nil)
	return &previewRig{db: db, job: *applied, inc: inc, ops: ops, exec: incident.NewExecutor(store, ops), store: store}
}

func patchParams(t *testing.T, manifest string) incident.ActionParams {
	t.Helper()
	def, err := schema.Parse([]byte(manifest))
	require.NoError(t, err)
	encoded, err := json.Marshal(def)
	require.NoError(t, err)
	return incident.ActionParams{Definition: encoded}
}

func (r *previewRig) preview(t *testing.T, manifest string) map[string]any {
	t.Helper()
	action, err := r.exec.Execute(context.Background(), incident.ActionRequest{
		IncidentID: r.inc.ID,
		Type:       incident.ActionTypeApplyJobdefPatch,
		Params:     patchParams(t, manifest),
	})
	require.NoError(t, err)
	var row models.AgentAction
	require.NoError(t, r.db.First(&row, "id = ?", action.ID).Error)
	var preview map[string]any
	require.NoError(t, json.Unmarshal(row.Result, &preview), "the proposal recorded no preview")
	return preview
}

// TestPreviewShowsEveryEdit is Greptile's finding through the real render: an
// edit that the reduced JobSpec omits reads as itself, not as "no changes", on
// the route the job takes, with or without provenance.
func TestPreviewShowsEveryEdit(t *testing.T) {
	for _, route := range []string{"direct", "escalate"} {
		for name, tc := range map[string]struct {
			edit     func(string) string
			expected []string
		}{
			"env only": {
				edit:     func(m string) string { return strings.Replace(m, "REGION: eu", "REGION: us", 1) },
				expected: []string{"-", "REGION: eu", "+", "REGION: us"},
			},
			"mount only": {
				edit:     func(m string) string { return strings.Replace(m, "target: /in", "target: /elsewhere", 1) },
				expected: []string{"target: /in", "target: /elsewhere"},
			},
			"mixed": {
				edit: func(m string) string {
					edited := strings.Replace(m, `command: ["true"]`, `command: ["sh", "-c", "exit 0"]`, 1)
					return strings.Replace(edited, "target: /in", "target: /elsewhere", 1)
				},
				expected: []string{"exit 0", "target: /in", "target: /elsewhere"},
			},
			"edge": {
				edit:     func(m string) string { return strings.Replace(m, "    next: [publish]\n", "", 1) },
				expected: []string{"next:", "publish"},
			},
		} {
			t.Run(route+"/"+name, func(t *testing.T) {
				rig := newPreviewRig(t, patchedJob)
				if route == "escalate" {
					require.NoError(t, rig.db.Model(&models.Job{}).Where("id = ?", rig.job.ID).
						Update("provenance_source_id", "orders").Error)
				}
				preview := rig.preview(t, tc.edit(patchedJob))
				require.Equal(t, route, preview["route"])
				text := preview["diff"].(string)
				require.NotContains(t, text, "no changes")
				require.Contains(t, text, "--- current\n+++ proposed\n@@ ")
				for _, expected := range tc.expected {
					require.Contains(t, text, expected)
				}
			})
		}
	}
}

// "No changes" is reserved for a proposal identical to the job as stored.
func TestPreviewSaysNoChangesOnlyForAnIdenticalDefinition(t *testing.T) {
	rig := newPreviewRig(t, patchedJob)
	require.Contains(t, rig.preview(t, patchedJob)["diff"], "no changes: the proposed definition is identical")
}

// An exported definition reads back as itself for every shipped example, so a
// proposal built from what the server holds shows the agent's edit, not noise.
func TestRenderReportsNoChangesForTheShippedExamples(t *testing.T) {
	paths, err := filepath.Glob("../../docs/examples/*.job.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	checked := 0
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		def, err := schema.Parse(raw)
		if err != nil {
			continue // a multi-document or fixture example
		}
		t.Run(filepath.Base(path), func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			defer testutil.CloseDB(db)
			applied, err := internaljobdef.NewImporter(db).Apply(context.Background(), def)
			if err != nil {
				t.Skipf("the example does not apply alone: %v", err)
			}
			exported, err := internaljobdef.NewExporter(db).Export(context.Background(), applied.ID)
			require.NoError(t, err)
			encoded, err := json.Marshal(exported)
			require.NoError(t, err)

			rendered, err := newIncidentActionOps(db, nil, nil).ApplyJobdefPatch(context.Background(), applied.ID, models.NamespaceOrDefault(def.Metadata.Namespace), encoded, true)
			if err != nil {
				t.Skipf("the example does not render alone: %v", err)
			}
			var result struct {
				Empty bool   `json:"empty"`
				Diff  string `json:"diff"`
			}
			require.NoError(t, json.Unmarshal(rendered, &result))
			require.True(t, result.Empty, "diff:\n%s", result.Diff)
			checked++
		})
	}
	require.NotZero(t, checked)
}

// A diff that cannot be produced leaves `diff` out and `empty` false, which the
// executor shows as the whole proposal. A job that became unreadable is the case
// a stored definition can really reach.
func TestRenderClaimsNothingWhenTheCurrentDefinitionCannotBeRead(t *testing.T) {
	rig := newPreviewRig(t, patchedJob)
	require.NoError(t, rig.db.Exec(`UPDATE triggers SET configuration = 'not json' WHERE id = ?`, rig.job.TriggerID).Error)

	preview := rig.preview(t, strings.Replace(patchedJob, "REGION: eu", "REGION: us", 1))
	text := preview["diff"].(string)
	require.NotContains(t, text, "no changes")
	require.Contains(t, text, "no diff could be produced")
	require.Contains(t, text, "REGION: us", "the whole proposal shows instead")
}

// The current definition reaches the incident's timeline, so it is read as a
// reader confined to the incident's namespace: a trigger shared with another
// namespace's job keeps its configuration, webhook secret included.
func TestRenderRedactsATriggerSharedOutsideTheNamespace(t *testing.T) {
	webhook := strings.NewReplacer(
		"type: cron", "type: http",
		`cron: "0 6 * * *"`, "path: /hooks/shared\n    secret: finance-webhook-secret",
	).Replace(patchedJob)
	rig := newPreviewRig(t, webhook)
	require.NoError(t, rig.db.Create(&models.Job{
		ID: uuid.New(), Alias: "fin", Namespace: "finance", TriggerID: rig.job.TriggerID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error)

	proposed := strings.Replace(strings.ReplaceAll(webhook, "finance-webhook-secret", "marketing-proposal"), "REGION: eu", "REGION: us", 1)
	preview := rig.preview(t, proposed)
	require.NotContains(t, preview["diff"], "finance-webhook-secret")
	require.Contains(t, preview["diff"], jobdiff.RedactedTriggerConfiguration["redacted"])
}

// TestAgentDoesNotReceiveThePreview: the preview quotes the current definition,
// which an agent token cannot read, so the answer to a proposal withholds it. The
// row keeps it for the human.
func TestAgentDoesNotReceiveThePreview(t *testing.T) {
	rig := newPreviewRig(t, patchedJob)
	proposed := strings.Replace(patchedJob, "      LITERAL: current-literal-value\n", "", 1)
	params, err := json.Marshal(patchParams(t, proposed))
	require.NoError(t, err)

	result, err := (&agentActionExecutor{exec: rig.exec, db: rig.db}).ExecuteAgentAction(context.Background(), agentsvc.ActionRequest{
		IncidentID: rig.inc.ID,
		Type:       incident.ActionTypeApplyJobdefPatch,
		Params:     params,
	})
	require.NoError(t, err)
	require.Equal(t, "awaiting_approval", result.Disposition)
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "current-literal-value")
	require.Empty(t, result.Action.Result)

	var row models.AgentAction
	require.NoError(t, rig.db.First(&row, "id = ?", result.Action.ID).Error)
	require.Contains(t, string(row.Result), "current-literal-value", "the human still reads it")
}

// A refused action keeps its result: the agent must be able to read why.
func TestAgentStillReadsARefusal(t *testing.T) {
	refused := &models.AgentAction{Status: models.AgentActionStatusRejected, Result: []byte(`{"reason":"no"}`)}
	require.Equal(t, refused, withoutPreview(refused))
}

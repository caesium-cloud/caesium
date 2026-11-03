package diff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const projectionPlainLiteral = "literal-one"

func TestProjectionRoundTripMatchesApplyShape(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	def := projectionDefinition()
	insertDiffDefinition(t, db, def)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)
	desired := FromDefinition(&def)
	diff := Compare(map[string]JobSpec{def.Metadata.Alias: desired}, actual)
	require.True(t, diff.Empty(), "unchanged persisted job must diff empty, got %+v", diff)

	got := actual[def.Metadata.Alias]
	require.Equal(t, "warn", got.SchemaValidation)
	require.Equal(t, 5*time.Minute, got.TaskTimeout)
	require.Equal(t, time.Hour, got.RunTimeout)
	require.Equal(t, schema.PriorityHigh, got.Priority)
	require.Equal(t, 2, got.Steps[0].Retries)
	require.Equal(t, schema.TriggerRuleOneSuccess, got.Steps[1].TriggerRule)
	require.Equal(t, schema.StepTypeTask, got.Steps[0].Type)
	require.Equal(t, schema.EngineDocker, got.Steps[0].Engine)
	require.True(t, got.Steps[0].ReplaySafe, "job replaySafe applies to every step")
	require.Equal(t, []EdgeSpec{{From: "extract", To: "load"}, {From: "load", To: "publish"}}, got.Edges)
	require.Equal(t, "6h", declarationFreshness(t, got, "warehouse/orders"))
	require.Equal(t, "team-queue", got.Steps[2].Runtime.Kubernetes.QueueName)
	require.Equal(t, "/work", got.Steps[0].Runtime.WorkDir)
	require.Equal(t, "CAESIUM_PARTITION", got.Steps[2].FanOut.Env)
	require.Equal(t, schema.FanOutOnEmptySkip, got.Steps[2].FanOut.OnEmpty)
	require.Equal(t, schema.FanOutFailureFailFast, got.Steps[2].FanOut.FailurePolicy)
	require.Equal(t, map[string]any{"mode": "incremental"}, got.Trigger.Configuration["defaultParams"])

	raw := mustSpecJSON(t, got)
	require.NotContains(t, raw, projectionPlainLiteral)
	require.Contains(t, raw, "secret://env/TOKEN")
	require.Equal(t, EnvValue{Digest: projectionDigest(projectionPlainLiteral), Redacted: true}, got.Steps[0].Runtime.Env["PLAIN"])
	require.Equal(t, EnvValue{Secret: "secret://env/TOKEN"}, got.Steps[0].Runtime.Env["TOKEN"])
	require.NotContains(t, diffString(diff), projectionPlainLiteral)
}

func TestProjectionReportsSavedFieldChanges(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	base := projectionDefinition()
	insertDiffDefinition(t, db, base)
	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	cases := []struct {
		name    string
		mutate  func(*schema.Definition)
		want    string
		absent  string
		present string
	}{
		{
			name: "schemaValidation",
			mutate: func(def *schema.Definition) {
				def.Metadata.SchemaValidation = schema.SchemaValidationFail
			},
			want: "SchemaValidation",
		},
		{
			name: "retries",
			mutate: func(def *schema.Definition) {
				def.Steps[0].Retries = 9
			},
			want: "Retries",
		},
		{
			name: "triggerRule",
			mutate: func(def *schema.Definition) {
				def.Steps[1].TriggerRule = schema.TriggerRuleAllDone
			},
			want: "TriggerRule",
		},
		{
			name: "edges",
			mutate: func(def *schema.Definition) {
				def.Steps[0].Next = []string{"publish"}
				def.Steps[1].Next = nil
			},
			want: "Edges",
		},
		{
			name: "taskTimeout",
			mutate: func(def *schema.Definition) {
				def.Metadata.TaskTimeout = 10 * time.Minute
			},
			want: "TaskTimeout",
		},
		{
			name: "env",
			mutate: func(def *schema.Definition) {
				def.Steps[0].Env = map[string]string{
					"PLAIN": "literal-two",
					"TOKEN": "secret://env/TOKEN_V2",
				}
			},
			want:    "PLAIN",
			absent:  "literal-two",
			present: "secret://env/TOKEN_V2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := base
			mutated.Steps = append([]schema.Step(nil), base.Steps...)
			tc.mutate(&mutated)
			desired := FromDefinition(&mutated)
			diff := Compare(map[string]JobSpec{base.Metadata.Alias: desired}, actual)
			require.Len(t, diff.Updates, 1)
			require.Equal(t, base.Metadata.Alias, diff.Updates[0].Alias)
			require.Contains(t, diff.Updates[0].Diff, tc.want)
			rendered := diff.Updates[0].Diff + mustSpecJSON(t, desired)
			require.NotContains(t, rendered, projectionPlainLiteral)
			if tc.absent != "" {
				require.NotContains(t, rendered, tc.absent)
			}
			if tc.present != "" {
				require.Contains(t, rendered, tc.present)
			}
		})
	}
}

func TestEdgesNormalizeToTheSameSet(t *testing.T) {
	implicit := projectionDefinition()
	implicit.Steps[0].Next = nil
	implicit.Steps[1].Next = nil
	explicit := projectionDefinition()

	require.True(t, Compare(
		map[string]JobSpec{"job": FromDefinition(&implicit)},
		map[string]JobSpec{"job": FromDefinition(&explicit)},
	).Empty(), "sequential steps must match explicit next edges")

	join := schema.Definition{
		Metadata: schema.Metadata{Alias: "join"},
		Trigger:  schema.Trigger{Type: schema.TriggerCron, Configuration: map[string]any{"cron": "0 * * * *"}},
		Steps: []schema.Step{
			{Name: "a", Image: "alpine:3.23", Engine: schema.EngineDocker},
			{Name: "b", Image: "alpine:3.23", Engine: schema.EngineDocker},
			{Name: "join", Image: "alpine:3.23", Engine: schema.EngineDocker, DependsOn: []string{"a", "b"}},
		},
	}
	flipped := join
	flipped.Steps = append([]schema.Step(nil), join.Steps...)
	flipped.Steps[2].DependsOn = []string{"b", "a"}
	require.True(t, Compare(
		map[string]JobSpec{"join": FromDefinition(&join)},
		map[string]JobSpec{"join": FromDefinition(&flipped)},
	).Empty(), "dependsOn order is not a change")

	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)
	insertDiffDefinition(t, db, explicit)
	var edges []models.TaskEdge
	require.NoError(t, db.Find(&edges).Error)
	require.NotEmpty(t, edges)
	dup := edges[0]
	dup.ID = uuid.New()
	require.NoError(t, db.Create(&dup).Error)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)
	require.True(t, Compare(
		map[string]JobSpec{explicit.Metadata.Alias: FromDefinition(&explicit)},
		actual,
	).Empty(), "duplicate edge rows collapse to one set entry")
}

func TestProjectionErrorStaysVisible(t *testing.T) {
	def := &schema.Definition{
		Metadata: schema.Metadata{Alias: "bad-edges", Namespace: "marketing"},
		Steps: []schema.Step{
			{Name: "a", Image: "alpine:3.23", Engine: schema.EngineDocker},
			{Name: "a", Image: "alpine:3.23", Engine: schema.EngineDocker},
		},
	}
	spec := FromDefinition(def)
	require.Equal(t, "bad-edges", spec.Alias)
	require.Equal(t, "marketing", spec.Namespace)
	require.Contains(t, spec.Annotations["caesium.dev/diff-projection-error"], "duplicate step name")
	diff := Compare(
		map[string]JobSpec{spec.Alias: spec},
		map[string]JobSpec{spec.Alias: {}},
	)
	require.NotEmpty(t, diff.Updates)
	require.Contains(t, diff.Updates[0].Diff, "caesium.dev/diff-projection-error")
}

func projectionDefinition() schema.Definition {
	return schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata: schema.Metadata{
			Alias:            "projection-job",
			Namespace:        "marketing",
			Labels:           map[string]string{"team": "data"},
			Annotations:      map[string]string{"owner": "etl"},
			MaxParallelTasks: 3,
			TaskTimeout:      5 * time.Minute,
			RunTimeout:       time.Hour,
			Priority:         schema.PriorityHigh,
			Concurrency:      &schema.Concurrency{MaxRuns: 2, Strategy: schema.ConcurrencyStrategyReplace},
			RateLimits:       []schema.RateLimit{{Resource: "warehouse-api", Limit: 60, Window: "1m"}},
			SLA:              &schema.SLAConfig{Duration: 30 * time.Minute, CompletedBy: "06:00"},
			SchemaValidation: schema.SchemaValidationWarn,
			ReplaySafe:       true,
			Cache:            map[string]any{"enabled": true, "ttl": "12h", "digestTTL": 0},
		},
		Trigger: schema.Trigger{
			Type:          schema.TriggerCron,
			Configuration: map[string]any{"cron": "15 3 * * *", "timezone": "UTC"},
			DefaultParams: map[string]string{"mode": "incremental"},
		},
		Callbacks: []schema.Callback{{
			Type:          schema.CallbackNotification,
			Configuration: map[string]any{"url": "https://example.invalid/hook"},
		}},
		Steps: []schema.Step{
			{
				Name:    "extract",
				Image:   "alpine:3.23",
				Command: []string{"sh", "-c", "echo extract"},
				Retries: 2,
				Next:    []string{"load"},
				Spec: container.Spec{
					Env: map[string]string{
						"PLAIN": projectionPlainLiteral,
						"TOKEN": "secret://env/TOKEN",
					},
					WorkDir: "/work",
				},
				Datasets: &schema.StepDatasets{
					Produces: []schema.ProducedDataset{{
						Name:      "warehouse/orders",
						Freshness: "6h",
					}},
				},
			},
			{
				Name:         "load",
				Engine:       schema.EngineDocker,
				Image:        "alpine:3.23",
				Command:      []string{"sh", "-c", "echo load"},
				TriggerRule:  schema.TriggerRuleOneSuccess,
				RetryDelay:   30 * time.Second,
				RetryBackoff: true,
				NodeSelector: map[string]string{"disk": "ssd"},
				Next:         []string{"publish"},
				RateLimit:    &schema.StepRateLimit{Resource: "warehouse-api", Units: 1},
			},
			{
				Name:               "publish",
				Engine:             schema.EngineKubernetes,
				Image:              "alpine:3.23",
				Command:            []string{"sh", "-c", "echo publish"},
				ServiceAccountName: "runner",
				Kueue:              &schema.Kueue{QueueName: "team-queue"},
				FanOut:             &schema.FanOut{From: "load", MaxPartitions: 4},
				OutputSchema: map[string]any{
					"type":     "object",
					"required": []any{"row_count"},
				},
			},
		},
	}
}

func declarationFreshness(t *testing.T, spec JobSpec, name string) string {
	t.Helper()
	for _, decl := range spec.Declarations {
		if decl.Name == name && decl.Direction == models.DatasetDirectionProduces {
			return decl.Freshness
		}
	}
	t.Fatalf("missing produced declaration %s", name)
	return ""
}

func mustSpecJSON(t *testing.T, spec JobSpec) string {
	t.Helper()
	raw, err := json.Marshal(spec)
	require.NoError(t, err)
	return string(raw)
}

func diffString(diff Diff) string {
	out := ""
	for _, update := range diff.Updates {
		out += update.Diff
	}
	return out
}

func projectionDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

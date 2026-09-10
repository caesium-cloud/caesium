package jobdef

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/ptr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
)

// richJob exercises every manifest surface the exporter has to reconstruct:
// the full metadata block (scheduling, SLA, cache, datasets, remediation), a
// trigger with defaultParams, callbacks, single- and multi-engine volumes, and
// steps carrying env/workdir/mounts/volumeMounts/nodeSelector/retries/
// triggerRule/rateLimit/fanOut/cache/schemas/datasets/workload identity/kueue
// and an explicit branch fan-out.
const richJob = `
apiVersion: v1
kind: Job
metadata:
  alias: export-round-trip
  labels:
    team: data
    tier: gold
  annotations:
    owner: platform-oncall
  maxParallelTasks: 3
  taskTimeout: 5m
  runTimeout: 1h
  priority: high
  concurrency:
    maxRuns: 2
    strategy: replace
  rateLimits:
    - resource: warehouse-api
      limit: 60
      window: 1m
  sla:
    duration: 30m
    completedBy: "06:00"
  schemaValidation: warn
  cache:
    enabled: true
    ttl: 12h
  onUpstreamHold: run
  datasets:
    skipWhenFresh: false
    sources:
      - name: vendor-drop
        expectedEvery: 24h
        external: true
        arrival:
          event:
            type: s3.object.created
            filter:
              bucket: vendor-drops
          watermark: $.detail.time
  remediation:
    profile: export-agent
    classes: [transient_infra, auth_failure]
    maxAttempts: 2
    autonomy:
      allow: [auto_retry_backoff, notify]
      paramOverrides:
        mode: [full, incremental]
      perClass:
        auth_failure:
          allow: [notify]
      requireApproval: [apply_jobdef_patch]
    escalation:
      channel: data-oncall
      after: 90m
trigger:
  type: cron
  configuration:
    cron: "15 3 * * *"
    timezone: UTC
  defaultParams:
    mode: incremental
callbacks:
  - type: notification
    configuration:
      url: https://example.invalid/hook
volumes:
  - name: shared
    accessMode: ReadWriteMany
    sources:
      docker:
        volume: caesium-export-shared
      kubernetes:
        pvc: caesium-export-shared-rwx
  - name: scratch
    source:
      tmpfs:
        sizeBytes: 1048576
        mode: 511
steps:
  - name: extract
    engine: docker
    image: busybox:1.36.1
    command: ["sh", "-c", "echo extract"]
    workdir: /work
    env:
      MODE: incremental
      TOKEN: "secret://env/EXPORT_TOKEN"
    mounts:
      - type: bind
        source: /tmp/export-src
        target: /src
        readOnly: true
    volumeMounts:
      - volume: shared
        path: /shared
      - volume: scratch
        path: /scratch
    nodeSelector:
      zone: us-east-1
    retries: 2
    retryDelay: 30s
    retryBackoff: true
    replaySafe: true
    rateLimit:
      resource: warehouse-api
      units: 2
    outputSchema:
      type: object
      required: [partitions]
    datasets:
      consumes:
        - vendor-drop
      produces:
        - name: raw-extract
          schemaFrom: output
          version: 2
          freshness: 6h
          maxStaleness: 12h
          watermark:
            key: as_of
          assertions:
            rowCount:
              min: 1
            custom:
              - metric: dupeRate
                max: 0.2
          onViolation: hold
          release: manual
    next: [fanned]

  - name: fanned
    engine: docker
    image: busybox:1.36.1
    command: ["sh", "-c", "echo fanned"]
    dependsOn: [extract]
    triggerRule: all_done
    cache: false
    fanOut:
      from: extract
      env: PARTITION
      maxPartitions: 16
      maxParallel: 4
      onEmpty: fail
      failurePolicy: continue
    inputSchema:
      extract:
        type: object
    next: [decide]

  - name: decide
    type: branch
    engine: docker
    image: busybox:1.36.1
    command: ["sh", "-c", "echo '##caesium::branch publish'"]
    dependsOn: [fanned]
    next: [publish, archive]

  - name: publish
    engine: kubernetes
    image: busybox:1.36.1
    command: ["sh", "-c", "echo publish"]
    dependsOn: [decide]
    serviceAccountName: caesium-publisher
    podAnnotations:
      iam.gke.io/gcp-service-account: publisher@example.invalid
    automountServiceAccountToken: true
    kueue:
      queueName: batch-queue
    volumeMounts:
      - volume: shared
        path: /shared
        readOnly: true
        subPath: reports
    cache:
      ttl: 1h

  - name: archive
    engine: docker
    image: busybox:1.36.1
    command: ["sh", "-c", "echo archive"]
    dependsOn: [decide]
    triggerRule: one_success
`

type ExporterTestSuite struct {
	suite.Suite
	db       *gorm.DB
	importer *Importer
	exporter *Exporter
}

func TestExporterSuite(t *testing.T) {
	suite.Run(t, new(ExporterTestSuite))
}

func (s *ExporterTestSuite) SetupTest() {
	// The rich manifest declares dataset assertions and metadata.onUpstreamHold,
	// both of which pkg/jobdef gates on the data circuit breaker's master flag.
	s.T().Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
	s.db = testutil.OpenTestDB(s.T())
	s.importer = NewImporter(s.db)
	s.exporter = NewExporter(s.db)
}

func (s *ExporterTestSuite) TearDownTest() {
	testutil.CloseDB(s.db)
}

// applyAndExport parses a manifest, applies it through the real importer, then
// exports it back and re-parses the YAML the exporter produced. Re-parsing is
// the offline equivalent of `caesium job lint`: schema.Parse validates.
func (s *ExporterTestSuite) applyAndExport(manifest string) (*schema.Definition, *schema.Definition, string) {
	s.T().Helper()

	def, err := schema.Parse([]byte(manifest))
	s.Require().NoError(err)

	jobModel, err := s.importer.Apply(context.Background(), def)
	s.Require().NoError(err)

	exported, err := s.exporter.Export(context.Background(), jobModel.ID)
	s.Require().NoError(err)

	encoded, err := yaml.Marshal(exported)
	s.Require().NoError(err)

	reparsed, err := schema.Parse(encoded)
	s.Require().NoError(err, "exported manifest must pass validation:\n%s", string(encoded))

	return def, reparsed, string(encoded)
}

func (s *ExporterTestSuite) TestExportedManifestMatchesTheAppliedOne() {
	original, exported, encoded := s.applyAndExport(richJob)

	// The acceptance property: diffing the exported manifest against the state
	// it was exported from reports nothing. This is exactly what `caesium job
	// diff` / POST /v1/jobdefs/diff compute.
	result := jobdiff.Compare(
		map[string]jobdiff.JobSpec{original.Metadata.Alias: jobdiff.FromDefinition(exported)},
		map[string]jobdiff.JobSpec{original.Metadata.Alias: jobdiff.FromDefinition(original)},
	)
	s.True(result.Empty(), "exported manifest must diff clean, got: %+v", result)

	// Derived runtime state must never leak back into the authoring manifest —
	// not in YAML, and not in the JSON the endpoint's ?format=json arm
	// serialises (container.Spec's yaml:"-" fields still carry json tags).
	s.NotContains(encoded, "resolvedVolumeMounts")
	s.assertNoRuntimeStateInStepJSON(exported)

	s.Equal(schema.APIVersionV1, exported.APIVersion)
	s.Equal(schema.KindJob, exported.Kind)

	md := exported.Metadata
	s.Equal("export-round-trip", md.Alias)
	s.Equal(map[string]string{"team": "data", "tier": "gold"}, md.Labels)
	s.Equal(map[string]string{"owner": "platform-oncall"}, md.Annotations)
	s.Equal(3, md.MaxParallelTasks)
	s.Equal(5*time.Minute, md.TaskTimeout)
	s.Equal(time.Hour, md.RunTimeout)
	s.Equal(schema.PriorityHigh, md.Priority)
	s.Require().NotNil(md.Concurrency)
	s.Equal(schema.Concurrency{MaxRuns: 2, Strategy: schema.ConcurrencyStrategyReplace}, *md.Concurrency)
	s.Equal([]schema.RateLimit{{Resource: "warehouse-api", Limit: 60, Window: "1m"}}, md.RateLimits)
	s.Require().NotNil(md.SLA)
	s.Equal(schema.SLAConfig{Duration: 30 * time.Minute, CompletedBy: "06:00"}, *md.SLA)
	s.Equal(schema.SchemaValidationWarn, md.SchemaValidation)
	s.Equal(map[string]any{"enabled": true, "ttl": "12h"}, md.Cache)
	s.Equal(schema.OnUpstreamHoldRun, md.OnUpstreamHold)

	s.Require().NotNil(md.Datasets)
	s.Require().NotNil(md.Datasets.SkipWhenFresh)
	s.False(*md.Datasets.SkipWhenFresh)
	s.Require().Len(md.Datasets.Sources, 1)
	s.Equal(schema.SourceDataset{
		Name:          "vendor-drop",
		ExpectedEvery: "24h",
		External:      true,
		Arrival: &schema.Arrival{
			Event:     &schema.ArrivalEvent{Type: "s3.object.created", Filter: map[string]string{"bucket": "vendor-drops"}},
			Watermark: "$.detail.time",
		},
	}, md.Datasets.Sources[0])

	s.Require().NotNil(md.Remediation)
	s.Equal(original.Metadata.Remediation, md.Remediation)

	s.Equal("cron", exported.Trigger.Type)
	s.Equal(map[string]any{"cron": "15 3 * * *", "timezone": "UTC"}, exported.Trigger.Configuration)
	s.Equal(map[string]string{"mode": "incremental"}, exported.Trigger.DefaultParams)

	s.Require().Len(exported.Callbacks, 1)
	s.Equal(schema.CallbackNotification, exported.Callbacks[0].Type)
	s.Equal(map[string]any{"url": "https://example.invalid/hook"}, exported.Callbacks[0].Configuration)

	s.assertVolumes(exported.Volumes)
	s.assertSteps(exported.Steps)
}

// assertNoRuntimeStateInStepJSON pins that a step serialised as JSON carries no
// resolvedVolumeMounts / kubernetes block. Those are container.Spec fields the
// runtime derives; re-applying a manifest that carried them would be lossy.
func (s *ExporterTestSuite) assertNoRuntimeStateInStepJSON(def *schema.Definition) {
	s.T().Helper()
	for i := range def.Steps {
		encoded, err := json.Marshal(def.Steps[i])
		s.Require().NoError(err)
		var fields map[string]json.RawMessage
		s.Require().NoError(json.Unmarshal(encoded, &fields))
		s.NotContains(fields, "resolvedVolumeMounts")
		s.NotContains(fields, "kubernetes")
	}
}

func (s *ExporterTestSuite) assertVolumes(volumes []schema.Volume) {
	s.T().Helper()
	s.Require().Len(volumes, 2)

	// `shared` is mounted by a docker step and a kubernetes step, so both
	// per-engine sources survive as a `sources` map.
	s.Equal("shared", volumes[0].Name)
	s.Nil(volumes[0].Source)
	s.Equal(map[string]schema.VolumeSource{
		schema.EngineDocker:     {Volume: "caesium-export-shared"},
		schema.EngineKubernetes: {PVC: "caesium-export-shared-rwx"},
	}, volumes[0].Sources)

	// `scratch` is mounted by one engine only, so it collapses back to a single
	// `source`.
	s.Equal("scratch", volumes[1].Name)
	s.Empty(volumes[1].Sources)
	s.Require().NotNil(volumes[1].Source)
	s.Require().NotNil(volumes[1].Source.Tmpfs)
	s.Equal(int64(1048576), volumes[1].Source.Tmpfs.SizeBytes)
	s.Require().NotNil(volumes[1].Source.Tmpfs.Mode)
	s.Equal(511, *volumes[1].Source.Tmpfs.Mode)
}

func (s *ExporterTestSuite) assertSteps(steps []schema.Step) {
	s.T().Helper()
	s.Require().Len(steps, 5)

	byName := make(map[string]*schema.Step, len(steps))
	names := make([]string, 0, len(steps))
	for i := range steps {
		byName[steps[i].Name] = &steps[i]
		names = append(names, steps[i].Name)
	}
	s.Equal([]string{"extract", "fanned", "decide", "publish", "archive"}, names)

	extract := byName["extract"]
	s.Equal(schema.EngineDocker, extract.Engine)
	s.Equal(schema.StepTypeTask, extract.Type)
	s.Equal("busybox:1.36.1", extract.Image)
	s.Equal([]string{"sh", "-c", "echo extract"}, extract.Command)
	s.Equal("/work", extract.WorkDir)
	s.Equal(map[string]string{"MODE": "incremental", "TOKEN": "secret://env/EXPORT_TOKEN"}, extract.Env)
	s.Equal([]container.Mount{{Type: container.MountTypeBind, Source: "/tmp/export-src", Target: "/src", ReadOnly: true}}, extract.Mounts)
	s.Equal([]schema.VolumeMount{
		{Volume: "shared", Path: "/shared"},
		{Volume: "scratch", Path: "/scratch"},
	}, extract.VolumeMounts)
	s.Equal(map[string]string{"zone": "us-east-1"}, extract.NodeSelector)
	s.Equal(2, extract.Retries)
	s.Equal(30*time.Second, extract.RetryDelay)
	s.True(extract.RetryBackoff)
	s.True(extract.ReplaySafe)
	s.Equal(&schema.StepRateLimit{Resource: "warehouse-api", Units: 2}, extract.RateLimit)
	s.Equal(map[string]any{"type": "object", "required": []any{"partitions"}}, extract.OutputSchema)
	s.Equal([]string{"fanned"}, extract.Next)
	s.Empty(extract.DependsOn)

	s.Require().NotNil(extract.Datasets)
	s.Equal([]schema.ConsumedDataset{{Name: "vendor-drop"}}, extract.Datasets.Consumes)
	s.Require().Len(extract.Datasets.Produces, 1)
	s.Equal(schema.ProducedDataset{
		Name:         "raw-extract",
		SchemaFrom:   schema.DatasetSchemaFromOutput,
		Version:      2,
		Freshness:    "6h",
		MaxStaleness: "12h",
		Watermark:    &schema.Watermark{Key: "as_of"},
		Assertions: &schema.DatasetAssertions{
			RowCount: &schema.AssertionSpec{Min: ptr.Of(float64(1))},
			Custom:   []schema.AssertionSpec{{Metric: "dupeRate", Max: ptr.Of(0.2)}},
		},
		OnViolation: schema.DatasetOnViolationHold,
		Release:     schema.DatasetReleaseManual,
	}, extract.Datasets.Produces[0])

	fanned := byName["fanned"]
	s.Equal(schema.TriggerRuleAllDone, fanned.TriggerRule)
	s.Equal(false, fanned.Cache)
	s.Equal(&schema.FanOut{
		From:          "extract",
		Env:           "PARTITION",
		MaxPartitions: 16,
		MaxParallel:   4,
		OnEmpty:       schema.FanOutOnEmptyFail,
		FailurePolicy: schema.FanOutFailureContinue,
	}, fanned.FanOut)
	s.Equal(map[string]map[string]any{"extract": {"type": "object"}}, fanned.InputSchema)
	s.Equal([]string{"decide"}, fanned.Next)

	decide := byName["decide"]
	s.Equal(schema.StepTypeBranch, decide.Type)
	s.Equal([]string{"publish", "archive"}, decide.Next)

	publish := byName["publish"]
	s.Equal(schema.EngineKubernetes, publish.Engine)
	s.Equal("caesium-publisher", publish.ServiceAccountName)
	s.Equal(map[string]string{"iam.gke.io/gcp-service-account": "publisher@example.invalid"}, publish.PodAnnotations)
	s.Require().NotNil(publish.AutomountServiceAccountToken)
	s.True(*publish.AutomountServiceAccountToken)
	s.Equal(&schema.Kueue{QueueName: "batch-queue"}, publish.Kueue)
	s.Equal([]schema.VolumeMount{{Volume: "shared", Path: "/shared", ReadOnly: true, SubPath: "reports"}}, publish.VolumeMounts)
	s.Equal(map[string]any{"ttl": "1h"}, publish.Cache)
	s.Empty(publish.Next)

	archive := byName["archive"]
	s.Equal(schema.TriggerRuleOneSuccess, archive.TriggerRule)
	s.Empty(archive.Next)
}

// TestExportIsIdempotent proves the manifest actually re-applies: applying the
// exported YAML and exporting again must produce byte-identical output.
func (s *ExporterTestSuite) TestExportIsIdempotent() {
	_, _, first := s.applyAndExport(richJob)
	_, _, second := s.applyAndExport(first)
	s.Equal(first, second)
}

// TestExportMinimalJobOmitsDefaults pins the "does not gain fields the author
// never wrote" property for the smallest possible manifest.
func (s *ExporterTestSuite) TestExportMinimalJobOmitsDefaults() {
	_, exported, encoded := s.applyAndExport(testutil.SampleJob)

	s.NotContains(encoded, "type: task")
	s.NotContains(encoded, "triggerRule:")
	s.NotContains(encoded, "replaySafe:")
	s.NotContains(encoded, "volumes:")
	s.NotContains(encoded, "datasets:")
	s.Nil(exported.Metadata.Datasets)
	s.Nil(exported.Volumes)

	// The importer auto-links a manifest with no explicit edges; the exporter
	// re-emits that chain explicitly, which is the same DAG.
	successors, err := schema.DeriveStepSuccessors(exported.Steps)
	s.Require().NoError(err)
	s.Equal(map[string][]string{
		"list":    {"convert"},
		"convert": {"publish"},
	}, successors)
}

// TestExportDropsRuntimeStateTheSchemaCannotExpress covers two shapes only a
// JSON apply can persist (container.Spec's kubernetes and resolvedVolumeMounts
// fields are yaml:"-", so no YAML author can reach them): a KubernetesSpec on a
// DOCKER step, and a resolved mount whose source kind cannot be turned back
// into a volume declaration. Re-emitting either would produce a manifest lint
// rejects — kubernetes-only fields on a docker step, and a volumeMounts entry
// naming an undeclared volume.
func (s *ExporterTestSuite) TestExportDropsRuntimeStateTheSchemaCannotExpress() {
	jobID, triggerID, atomID := uuid.New(), uuid.New(), uuid.New()
	spec := container.Spec{
		Kubernetes: &container.KubernetesSpec{
			ServiceAccountName: "leaked-sa",
			QueueName:          "leaked-queue",
		},
		ResolvedVolumeMounts: []container.VolumeMount{
			// A bind mount with no source: sourceKind cannot classify it, so no
			// volume declaration can be rebuilt for it.
			{Name: "unreconstructable", Type: container.VolumeMountTypeBind, Target: "/data"},
		},
	}
	specJSON, err := json.Marshal(spec)
	s.Require().NoError(err)

	def, err := BuildDefinition(&JobRecords{
		Job:     &models.Job{ID: jobID, Alias: "docker-with-k8s-spec", TriggerID: triggerID},
		Trigger: &models.Trigger{ID: triggerID, Type: models.TriggerTypeCron, Configuration: `{"cron":"0 * * * *"}`},
		Tasks:   []models.Task{{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "only", Type: schema.StepTypeTask, TriggerRule: schema.TriggerRuleAllSuccess}},
		Atoms: map[uuid.UUID]*models.Atom{
			atomID: {ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: `["sh","-c","echo hi"]`, Spec: specJSON},
		},
	})
	s.Require().NoError(err)
	s.Require().Len(def.Steps, 1)

	step := def.Steps[0]
	s.Equal(schema.EngineDocker, step.Engine)
	s.Empty(step.ServiceAccountName)
	s.Nil(step.Kueue)
	s.Nil(step.PodAnnotations)
	s.Nil(step.AutomountServiceAccountToken)
	s.Empty(def.Volumes)
	s.Empty(step.VolumeMounts)

	// And the result is still a manifest the schema accepts.
	encoded, err := yaml.Marshal(def)
	s.Require().NoError(err)
	_, err = schema.Parse(encoded)
	s.Require().NoError(err, "exported manifest must stay valid:\n%s", string(encoded))
}

// emptyConstraintJob declares the triage-only remediation posture at BOTH
// autonomy levels: `allow: []` means "configured, grants nothing", which is a
// different policy from an absent allow-list ("unconfigured", so the tier
// defaults let tier 0/1 actions run without a human).
const emptyConstraintJob = `
apiVersion: v1
kind: Job
metadata:
  alias: export-empty-constraints
  remediation:
    profile: export-agent
    classes: [transient_infra, auth_failure]
    autonomy:
      allow: []
      perClass:
        auth_failure:
          allow: []
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: only
    engine: docker
    image: busybox:1.36.1
    command: ["sh", "-c", "echo hi"]
`

// TestExportPreservesExplicitlyEmptyRemediationConstraints is the apply →
// export → parse → resolve regression for the YAML half of the absent-vs-empty
// rule. #452 fixed MarshalJSON, but the manifest is re-SERIALISED to YAML by
// the exporter, and `yaml:"allow,omitempty"` drops an empty slice just as
// readily — so a stored `perClass.auth_failure.allow: []` exported as
// `auth_failure: {}`, re-parsed as nil, and re-applied as "inherit", widening
// the policy the author wrote to deny.
func (s *ExporterTestSuite) TestExportPreservesExplicitlyEmptyRemediationConstraints() {
	original, exported, encoded := s.applyAndExport(emptyConstraintJob)

	// The authored policy really was explicitly-empty, not absent.
	s.Require().NotNil(original.Metadata.Remediation)
	s.Require().NotNil(original.Metadata.Remediation.Autonomy)
	s.NotNil(original.Metadata.Remediation.Autonomy.Allow)
	s.Empty(original.Metadata.Remediation.Autonomy.Allow)

	// The exported YAML must SAY `allow: []` at both levels, not omit it.
	s.Contains(encoded, "allow: []", "exported YAML must keep the explicit deny-all list:\n%s", encoded)
	s.Equal(2, strings.Count(encoded, "allow: []"),
		"both the autonomy and the per-class allow-list must survive:\n%s", encoded)

	// And re-parsing it must yield configured-but-empty, never nil.
	s.Require().NotNil(exported.Metadata.Remediation)
	autonomy := exported.Metadata.Remediation.Autonomy
	s.Require().NotNil(autonomy)
	s.NotNil(autonomy.Allow, "autonomy.allow must round-trip as configured-but-empty, not absent")
	s.Empty(autonomy.Allow)
	perClass, ok := autonomy.PerClass[schema.RemediationClassAuthFailure]
	s.Require().True(ok)
	s.NotNil(perClass.Allow, "perClass allow must round-trip as configured-but-empty, not absent")
	s.Empty(perClass.Allow)

	// Resolve the way the executor does: re-apply the exported manifest and read
	// the policy back off the persisted column. A nil Allow here would mean the
	// class inherits — the exact widening this pins shut.
	jobModel, err := s.importer.Apply(context.Background(), exported)
	s.Require().NoError(err)
	resolved := incident.DecodePlaybook(jobModel.Remediation).ForClass(schema.RemediationClassAuthFailure)
	s.NotNil(resolved.Allow, "resolved per-class allow-list must stay configured (deny-all), not inherit")
	s.Empty(resolved.Allow)
}

// bumpJobRevisionOnce registers a gorm query callback that, the FIRST time the
// tasks table is read, writes a new jobs.updated_at through the SAME connection
// the read is using. That is precisely the interleaving an apply committing
// mid-export produces, and it is what makes the reads straddle two revisions.
// It returns a counter of how many reads it fired on.
func (s *ExporterTestSuite) bumpJobRevisionOnce(jobID uuid.UUID, at time.Time) *int {
	s.T().Helper()

	fired := 0
	err := s.db.Callback().Query().After("gorm:query").Register("test:bump_job_revision", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "tasks" || fired > 0 {
			return
		}
		fired++
		// ExecContext on the statement's ConnPool (the *sql.Tx inside a
		// transaction) bypasses gorm's callbacks, so this cannot recurse.
		if _, execErr := tx.Statement.ConnPool.ExecContext(
			tx.Statement.Context, "UPDATE jobs SET updated_at = ? WHERE id = ?", at, jobID,
		); execErr != nil {
			s.T().Errorf("inject concurrent apply: %v", execErr)
		}
	})
	s.Require().NoError(err)
	s.T().Cleanup(func() {
		_ = s.db.Callback().Query().Remove("test:bump_job_revision")
	})
	return &fired
}

// TestLoadRetriesWhenTheJobIsAppliedMidRead pins the consistent-revision
// guarantee: Load's reads are one transaction plus a revision re-check, so an
// apply landing between them is DETECTED and the whole read is retried rather
// than returning a manifest stitched from two revisions.
func (s *ExporterTestSuite) TestLoadRetriesWhenTheJobIsAppliedMidRead() {
	def, err := schema.Parse([]byte(testutil.SampleJob))
	s.Require().NoError(err)
	jobModel, err := s.importer.Apply(context.Background(), def)
	s.Require().NoError(err)

	fired := s.bumpJobRevisionOnce(jobModel.ID, jobModel.UpdatedAt.Add(time.Second))

	records, err := s.exporter.Load(context.Background(), jobModel.ID)
	s.Require().NoError(err)
	s.Equal(1, *fired, "the injected apply must have landed inside the first read")
	s.Require().NotNil(records)
	s.Len(records.Tasks, 3)

	// The first attempt SAW the change and rolled back — which also unwinds the
	// injected write, since it rode that same transaction — so the retry read the
	// settled revision. What matters is that the returned records describe ONE
	// revision, the one the database is actually at, rather than a mix.
	var settled models.Job
	s.Require().NoError(s.db.Where("id = ?", jobModel.ID).First(&settled).Error)
	s.True(records.Job.UpdatedAt.Equal(settled.UpdatedAt),
		"exported revision %s must be the settled one %s", records.Job.UpdatedAt, settled.UpdatedAt)
}

// TestLoadFailsWhenTheJobKeepsChanging pins the bounded half of the retry: a job
// re-applied under every read is reported as a conflict, never served as a
// hybrid manifest.
func (s *ExporterTestSuite) TestLoadFailsWhenTheJobKeepsChanging() {
	def, err := schema.Parse([]byte(testutil.SampleJob))
	s.Require().NoError(err)
	jobModel, err := s.importer.Apply(context.Background(), def)
	s.Require().NoError(err)

	bumps := 0
	err = s.db.Callback().Query().After("gorm:query").Register("test:bump_always", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Table != "tasks" {
			return
		}
		bumps++
		if _, execErr := tx.Statement.ConnPool.ExecContext(
			tx.Statement.Context, "UPDATE jobs SET updated_at = ? WHERE id = ?",
			jobModel.UpdatedAt.Add(time.Duration(bumps)*time.Second), jobModel.ID,
		); execErr != nil {
			s.T().Errorf("inject concurrent apply: %v", execErr)
		}
	})
	s.Require().NoError(err)
	defer func() { _ = s.db.Callback().Query().Remove("test:bump_always") }()

	_, err = s.exporter.Load(context.Background(), jobModel.ID)
	s.Require().Error(err)
	s.ErrorIs(err, ErrJobChangedDuringExport)
	s.Equal(exportLoadAttempts, bumps, "every attempt must be retried before giving up")
}

func (s *ExporterTestSuite) TestExportUnknownJobReturnsNotFound() {
	_, err := s.exporter.Export(context.Background(), uuid.New())
	s.Require().Error(err)
	s.ErrorIs(err, gorm.ErrRecordNotFound)
}

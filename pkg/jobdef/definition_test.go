package jobdef

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/require"
)

var example1 = `
$schema: https://yourorg.io/schemas/job.v1.json
apiVersion: v1
kind: Job
metadata:
  alias: csv-to-parquet
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
    timezone: "America/New_York"
callbacks:
  - type: notification
    configuration:
      webhook_url: "https://hooks.slack.com/services/T000/B000/XYZ"
      channel: "#data-pipelines"
      mention: "@oncall"
steps:
  - name: list
    engine: docker
    image: ghcr.io/yourorg/s3ls:1.2
    command: ["s3ls", "s3://demo/csv/*.csv", "--out", "/out/files.json"]
  - name: convert
    engine: docker
    image: ghcr.io/yourorg/csv2pq:0.5
    command: ["csv2pq", "--in", "/in/files.json", "--out", "/out/manifest.json"]
  - name: publish
    engine: docker
    image: ghcr.io/yourorg/uploader:0.3
    command: ["upload", "--manifest", "/out/manifest.json", "--dest", "s3://demo/parquet/"]
`

var example2 = `
apiVersion: v1
kind: Job
metadata:
  alias: nightly-etl
trigger:
  type: cron
  configuration: { cron: "0 2 * * *", timezone: "UTC" }
steps:
  - name: extract
    image: ghcr.io/yourorg/extract:2.0
    command: ["extract"]
  - name: transform
    image: ghcr.io/yourorg/transform:1.7
    command: ["transform"]
  - name: load
    image: ghcr.io/yourorg/load:0.9
    command: ["load"]
`

var dagExample = `
apiVersion: v1
kind: Job
metadata:
  alias: branchy-job
trigger:
  type: cron
  configuration: { cron: "*/5 * * * *", timezone: "UTC" }
steps:
  - name: start
    image: alpine:3.23
    command: ["echo", "start"]
    next:
      - fanout-a
      - fanout-b
  - name: fanout-a
    image: alpine:3.23
    command: ["echo", "a"]
  - name: fanout-b
    image: alpine:3.23
    command: ["echo", "b"]
  - name: join
    image: alpine:3.23
    command: ["echo", "done"]
    dependsOn: ["fanout-a", "fanout-b"]
`

func TestParseValidDefinitions(t *testing.T) {
	defs := []string{example1, example2, dagExample}

	for idx, src := range defs {
		def, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("example %d parse error: %v", idx+1, err)
		}

		if def.Kind != KindJob {
			t.Fatalf("example %d unexpected kind: %s", idx+1, def.Kind)
		}

		if len(def.Steps) == 0 {
			t.Fatalf("example %d steps not parsed", idx+1)
		}

		// Ensure default engine is set when omitted.
		for _, step := range def.Steps {
			if step.Engine == "" {
				t.Fatalf("example %d step %s engine is empty", idx+1, step.Name)
			}
		}

		if def.Metadata.Alias == "branchy-job" {
			var (
				start Step
				found bool
			)
			for _, step := range def.Steps {
				if step.Name == "start" {
					start = step
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("branchy job start step not found")
			}
			if len(start.Next) != 2 {
				t.Fatalf("branchy job should have two successors, got %d", len(start.Next))
			}
		}
	}
}

func TestParseRepositoryExamples(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))

	for _, dir := range []string{"docs/examples", "docs/examples-k8s"} {
		pattern := filepath.Join(repoRoot, filepath.FromSlash(dir), "*.job.yaml")
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err)
		require.NotEmpty(t, matches, "expected example manifests in %s", dir)

		for _, path := range matches {
			path := path
			t.Run(filepath.ToSlash(path[len(repoRoot)+1:]), func(t *testing.T) {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				def, err := Parse(data)
				require.NoError(t, err)
				require.NotEmpty(t, def.Metadata.Alias)
				require.NotEmpty(t, def.Steps)
			})
		}
	}
}

func TestReplaySafeJobAndStepEffectiveValue(t *testing.T) {
	src := `
apiVersion: v1
kind: Job
metadata:
  alias: replay-markers
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: unsafe-by-default
    image: alpine:3.23
  - name: step-safe
    replaySafe: true
    image: alpine:3.23
`
	def, err := Parse([]byte(src))
	require.NoError(t, err)
	require.False(t, def.Metadata.ReplaySafe)
	require.False(t, def.Steps[0].ReplaySafe)
	require.True(t, def.Steps[1].ReplaySafe)
	require.False(t, def.EffectiveReplaySafeForStep(&def.Steps[0]))
	require.True(t, def.EffectiveReplaySafeForStep(&def.Steps[1]))

	def.Metadata.ReplaySafe = true
	require.True(t, def.EffectiveReplaySafeForStep(&def.Steps[0]))
	require.True(t, def.EffectiveReplaySafeForStep(&def.Steps[1]))
}

func TestParseSchedulingMetadata(t *testing.T) {
	src := `
apiVersion: v1
kind: Job
metadata:
  alias: scheduling-metadata
  priority: high
  concurrency:
    maxRuns: 1
    strategy: skip
  rateLimits:
    - resource: warehouse-api
      limit: 120
      window: 1m
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: alpine:3.23
    rateLimit:
      resource: warehouse-api
      units: 2
`
	def, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Equal(t, PriorityHigh, def.Metadata.Priority)
	require.Equal(t, &Concurrency{MaxRuns: 1, Strategy: ConcurrencyStrategySkip}, def.Metadata.Concurrency)
	require.Equal(t, []RateLimit{{Resource: "warehouse-api", Limit: 120, Window: "1m"}}, def.Metadata.RateLimits)
	require.NotNil(t, def.Steps[0].RateLimit)
	require.Equal(t, "warehouse-api", def.Steps[0].RateLimit.Resource)
	require.Equal(t, 2, def.Steps[0].RateLimit.Units)
}

func TestParseSchedulingMetadataNormalizesAndDefaults(t *testing.T) {
	src := `
apiVersion: v1
kind: Job
metadata:
  alias: scheduling-defaults
  concurrency:
    maxRuns: 1
  rateLimits:
    - resource: " api "
      limit: 10
      window: 1m
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: call
    image: alpine:3.23
    rateLimit:
      resource: " api "
`
	def, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Equal(t, &Concurrency{MaxRuns: 1, Strategy: ConcurrencyStrategyQueue}, def.Metadata.Concurrency)
	require.Equal(t, []RateLimit{{Resource: "api", Limit: 10, Window: "1m"}}, def.Metadata.RateLimits)
	require.NotNil(t, def.Steps[0].RateLimit)
	require.Equal(t, "api", def.Steps[0].RateLimit.Resource)
	require.Equal(t, 1, def.Steps[0].RateLimit.Units)
}

func TestParseInvalidDefinitions(t *testing.T) {
	cases := map[string]string{
		"bad version": `apiVersion: v2
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {}
steps:
  - name: step
    image: example
`,
		"duplicate step": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
  - name: build
    image: example
`,
		"unknown next": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    next: missing
`,
		"bad trigger": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: foo
  configuration: {}
steps:
  - name: build
    image: example
`,
		"http trigger missing path": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: http
  configuration: {}
steps:
  - name: build
    image: example
`,
		"http trigger invalid signature scheme": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: http
  configuration:
    path: /hooks/test
    signatureScheme: oauth2
steps:
  - name: build
    image: example
`,
		"http trigger invalid param mapping": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: http
  configuration:
    path: /hooks/test
    paramMapping:
      branch: ref
steps:
  - name: build
    image: example
`,
		"unknown dependsOn": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    dependsOn: ["missing"]
`,
		"cycle": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    next: deploy
  - name: deploy
    image: example
    next: build
`,
		"unknown volume": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    volumeMounts: [{volume: missing, path: /work}]
`,
		"missing engine source": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
volumes:
  - name: work
    sources:
      kubernetes: {pvc: shared}
steps:
  - name: build
    image: example
    volumeMounts: [{volume: work, path: /work}]
`,
		"incompatible source": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
volumes:
  - name: work
    sources:
      docker: {pvc: shared}
steps:
  - name: build
    image: example
    volumeMounts: [{volume: work, path: /work}]
`,
		"duplicate mount path": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
volumes:
  - name: work
    source: {bind: /tmp/work}
steps:
  - name: build
    image: example
    mounts: [{type: bind, source: /tmp/other, target: /work}]
    volumeMounts: [{volume: work, path: /work}]
`,
		"service account on docker": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    serviceAccountName: deployer
`,
		"kueue on docker": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    kueue: {queueName: data-eng}
`,
		"kueue missing queue name": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    engine: kubernetes
    image: example
    kueue: {queueName: "  "}
`,
		"kueue uppercase queue name": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    engine: kubernetes
    image: example
    kueue: {queueName: Data-Eng}
`,
		"kueue queue name with spaces": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    engine: kubernetes
    image: example
    kueue: {queueName: "data eng"}
`,
		"kueue queue name too long": `apiVersion: v1
kind: Job
metadata:
  alias: test
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    engine: kubernetes
    image: example
    kueue: {queueName: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}
`,
		"bad priority": `apiVersion: v1
kind: Job
metadata:
  alias: test
  priority: urgent
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
`,
		"bad concurrency strategy": `apiVersion: v1
kind: Job
metadata:
  alias: test
  concurrency:
    maxRuns: 1
    strategy: newest
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
`,
		"negative concurrency maxRuns": `apiVersion: v1
kind: Job
metadata:
  alias: test
  concurrency:
    maxRuns: -1
    strategy: skip
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
`,
		"non-positive rate limit limit": `apiVersion: v1
kind: Job
metadata:
  alias: test
  rateLimits:
    - resource: api
      limit: 0
      window: 1m
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
`,
		"duplicate rate limit resource": `apiVersion: v1
kind: Job
metadata:
  alias: test
  rateLimits:
    - resource: api
      limit: 10
      window: 1m
    - resource: " api "
      limit: 20
      window: 1m
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
`,
		"bad rate limit window": `apiVersion: v1
kind: Job
metadata:
  alias: test
  rateLimits:
    - resource: api
      limit: 10
      window: forever
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
`,
		"blank rate limit resource": `apiVersion: v1
kind: Job
metadata:
  alias: test
  rateLimits:
    - resource: " "
      limit: 10
      window: 1m
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
`,
		"step rate limit unknown resource": `apiVersion: v1
kind: Job
metadata:
  alias: test
  rateLimits:
    - resource: api
      limit: 10
      window: 1m
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    rateLimit:
      resource: database
      units: 1
`,
		"negative step rate limit units": `apiVersion: v1
kind: Job
metadata:
  alias: test
  rateLimits:
    - resource: api
      limit: 10
      window: 1m
trigger:
  type: cron
  configuration: {cron: "* * * * *"}
steps:
  - name: build
    image: example
    rateLimit:
      resource: api
      units: -1
`,
	}

	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestVolumesAndWorkloadIdentityRuntimeSpec(t *testing.T) {
	automount := false
	src := `
apiVersion: v1
kind: Job
metadata:
  alias: volume-identity
  serviceAccountName: default-deployer
  podAnnotations:
    team: platform
  automountServiceAccountToken: true
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
volumes:
  - name: work
    sources:
      docker: {bind: /mnt/shared/work}
      kubernetes: {pvc: ci-shared-rwx}
steps:
  - name: local
    image: alpine:3.23
    volumeMounts:
      - {volume: work, path: /work}
  - name: cluster
    engine: kubernetes
    image: alpine:3.23
    serviceAccountName: step-deployer
    podAnnotations:
      purpose: deploy
    automountServiceAccountToken: false
    volumeMounts:
      - {volume: work, path: /work, readOnly: true, subPath: plans}
`
	def, err := Parse([]byte(src))
	require.NoError(t, err)

	localSpec, err := def.RuntimeSpecForStep(&def.Steps[0])
	require.NoError(t, err)
	require.Len(t, localSpec.ResolvedVolumeMounts, 1)
	require.Equal(t, container.VolumeMountTypeBind, localSpec.ResolvedVolumeMounts[0].Type)
	require.Equal(t, "/mnt/shared/work", localSpec.ResolvedVolumeMounts[0].Source)
	require.Nil(t, localSpec.Kubernetes)

	def.Metadata.AutomountServiceAccountToken = &automount
	clusterSpec, err := def.RuntimeSpecForStep(&def.Steps[1])
	require.NoError(t, err)
	require.Len(t, clusterSpec.ResolvedVolumeMounts, 1)
	require.Equal(t, container.VolumeMountTypePVC, clusterSpec.ResolvedVolumeMounts[0].Type)
	require.Equal(t, "ci-shared-rwx", clusterSpec.ResolvedVolumeMounts[0].Source)
	require.True(t, clusterSpec.ResolvedVolumeMounts[0].ReadOnly)
	require.Equal(t, "plans", clusterSpec.ResolvedVolumeMounts[0].SubPath)
	require.NotNil(t, clusterSpec.Kubernetes)
	require.Equal(t, "step-deployer", clusterSpec.Kubernetes.ServiceAccountName)
	require.Equal(t, map[string]string{"team": "platform", "purpose": "deploy"}, clusterSpec.Kubernetes.PodAnnotations)
	require.NotNil(t, clusterSpec.Kubernetes.AutomountServiceAccountToken)
	require.False(t, *clusterSpec.Kubernetes.AutomountServiceAccountToken)
}

// TestKueueDelegationRuntimeSpec asserts a kubernetes step declaring a Kueue
// queue threads the queue name into the runtime KubernetesSpec (the carrier the
// engine reads to stamp the kueue.x-k8s.io/queue-name label), and that a docker
// step is unaffected. Validation already rejects kueue on non-kubernetes steps;
// here a queue-only kubernetes step must still materialize a KubernetesSpec.
func TestKueueDelegationRuntimeSpec(t *testing.T) {
	src := `
apiVersion: v1
kind: Job
metadata:
  alias: kueue-delegation
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: local
    image: alpine:3.23
  - name: cluster
    engine: kubernetes
    image: alpine:3.23
    kueue:
      queueName: data-eng
`
	def, err := Parse([]byte(src))
	require.NoError(t, err)

	localSpec, err := def.RuntimeSpecForStep(&def.Steps[0])
	require.NoError(t, err)
	require.Nil(t, localSpec.Kubernetes, "docker step must not carry a KubernetesSpec")

	clusterSpec, err := def.RuntimeSpecForStep(&def.Steps[1])
	require.NoError(t, err)
	require.NotNil(t, clusterSpec.Kubernetes, "a queue-only kubernetes step must materialize a KubernetesSpec")
	require.Equal(t, "data-eng", clusterSpec.Kubernetes.QueueName)
	// The queue alone carries no execution identity.
	require.False(t, clusterSpec.Kubernetes.HasIdentityFields())
}

// TestKueueQueueNameAcceptsValidDNSLabels asserts the queueName validation
// accepts the DNS-1123 forms Kueue uses for a LocalQueue name — a plain label, a
// dotted subdomain, and the 63-character boundary — so the guard rejects only
// genuinely invalid names. The invalid forms (uppercase, spaces, >63 chars) are
// covered as error cases in TestParseInvalidDefinitions.
func TestKueueQueueNameAcceptsValidDNSLabels(t *testing.T) {
	valid := []string{
		"data-eng",
		"gpu.shared",
		"q0",
		strings.Repeat("a", 63), // length boundary
	}
	for _, name := range valid {
		src := `
apiVersion: v1
kind: Job
metadata:
  alias: kueue-valid
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: cluster
    engine: kubernetes
    image: alpine:3.23
    kueue:
      queueName: ` + name + `
`
		_, err := Parse([]byte(src))
		require.NoErrorf(t, err, "queueName %q should be valid", name)
	}
}

func TestLegacyMountRelativeTargetStillValidates(t *testing.T) {
	src := `
apiVersion: v1
kind: Job
metadata:
  alias: legacy-mount
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: build
    image: alpine:3.23
    mounts:
      - {type: bind, source: /tmp/work, target: relative/work}
`

	_, err := Parse([]byte(src))
	require.NoError(t, err)
}

func TestRuntimeSpecForStepDeepCopiesRawVolumeSource(t *testing.T) {
	src := `
apiVersion: v1
kind: Job
metadata:
  alias: raw-volume-source
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
volumes:
  - name: nfs-data
    source:
      volumeSource:
        nfs:
          server: nfs.example.com
          path: /exports/data
steps:
  - name: cluster
    engine: kubernetes
    image: alpine:3.23
    volumeMounts:
      - {volume: nfs-data, path: /data}
`

	def, err := Parse([]byte(src))
	require.NoError(t, err)
	spec, err := def.RuntimeSpecForStep(&def.Steps[0])
	require.NoError(t, err)
	require.Len(t, spec.ResolvedVolumeMounts, 1)

	resolvedNFS := spec.ResolvedVolumeMounts[0].VolumeSource["nfs"].(map[string]any)
	resolvedNFS["server"] = "changed.example.com"

	originalNFS := def.Volumes[0].Source.VolumeSource["nfs"].(map[string]any)
	require.Equal(t, "nfs.example.com", originalNFS["server"])
}

func TestStepUnmarshalJSONIncludesVolumeAndIdentityFields(t *testing.T) {
	raw := []byte(`{
		"name": "apply",
		"engine": "kubernetes",
		"image": "alpine:3.23",
		"serviceAccountName": "deployer",
		"podAnnotations": {"iam": "enabled"},
		"automountServiceAccountToken": false,
		"rateLimit": {"resource": "api", "units": 3},
		"volumeMounts": [{"volume": "work", "path": "/work"}]
	}`)

	var step Step
	require.NoError(t, json.Unmarshal(raw, &step))
	require.Equal(t, EngineKubernetes, step.Engine)
	require.Equal(t, "deployer", step.ServiceAccountName)
	require.Equal(t, map[string]string{"iam": "enabled"}, step.PodAnnotations)
	require.NotNil(t, step.AutomountServiceAccountToken)
	require.False(t, *step.AutomountServiceAccountToken)
	require.Equal(t, &StepRateLimit{Resource: "api", Units: 3}, step.RateLimit)
	require.Equal(t, []VolumeMount{{Volume: "work", Path: "/work"}}, step.VolumeMounts)
}

func TestValidateSimpleJSONPath(t *testing.T) {
	t.Parallel()

	valid := []string{"$", "$[0]", "$[0].id", "$.ref", "$.sender.login", "$.items.0.name", "$.items[0].name"}
	for _, expr := range valid {
		require.NoError(t, validateSimpleJSONPath(expr), expr)
	}

	invalid := []string{"", "ref", "$.", "$.sender..login", "$.sender. login"}
	for _, expr := range invalid {
		require.Error(t, validateSimpleJSONPath(expr), expr)
	}
}

func TestValidateEventTriggerConfiguration(t *testing.T) {
	t.Parallel()

	valid := &Trigger{
		Type: TriggerEvent,
		Configuration: map[string]any{
			"events": []any{
				map[string]any{
					"type":   "webhook.*",
					"source": "github",
					"filter": map[string]any{"repository.full_name": "caesium-cloud/caesium"},
				},
			},
			"paramMapping":  map[string]any{"branch": "$.ref"},
			"defaultParams": map[string]any{"environment": "staging"},
		},
	}
	require.NoError(t, ValidateTriggerSpec(valid))

	missingEvents := &Trigger{Type: TriggerEvent, Configuration: map[string]any{}}
	require.Error(t, ValidateTriggerSpec(missingEvents))

	nonStringFilter := &Trigger{
		Type: TriggerEvent,
		Configuration: map[string]any{
			"events": []any{
				map[string]any{"type": "webhook.*", "filter": map[string]any{"delivery.attempt": 2}},
			},
		},
	}
	require.Error(t, ValidateTriggerSpec(nonStringFilter))

	badParamMapping := &Trigger{
		Type: TriggerEvent,
		Configuration: map[string]any{
			"events":       []any{map[string]any{"type": "webhook.*"}},
			"paramMapping": map[string]any{"branch": "ref"},
		},
	}
	require.Error(t, ValidateTriggerSpec(badParamMapping))

	badDefaultParams := &Trigger{
		Type: TriggerEvent,
		Configuration: map[string]any{
			"events":        []any{map[string]any{"type": "webhook.*"}},
			"defaultParams": map[string]any{"attempt": 2},
		},
	}
	require.Error(t, ValidateTriggerSpec(badDefaultParams))
}

func TestValidateFreshnessTriggerRequiresGateAndDatasets(t *testing.T) {
	t.Setenv("CAESIUM_FRESHNESS_ENABLED", "false")
	err := ValidateTriggerSpec(&Trigger{Type: TriggerFreshness, Configuration: map[string]any{}})
	require.ErrorContains(t, err, "CAESIUM_FRESHNESS_ENABLED=true")

	// A bare freshness trigger (no accompanying definition, the trigger-create/
	// update API path) has no dataset context to satisfy the requirement, so it
	// is rejected even when the feature is enabled.
	t.Setenv("CAESIUM_FRESHNESS_ENABLED", "true")
	err = ValidateTriggerSpec(&Trigger{Type: TriggerFreshness, Configuration: map[string]any{}})
	require.ErrorContains(t, err, "cannot be created via the trigger API")

	valid := `
apiVersion: v1
kind: Job
metadata:
  alias: freshness-job
  datasets:
    skipWhenFresh: false
trigger:
  type: freshness
  configuration: {}
steps:
  - name: build
    image: alpine:3.23
    datasets:
      consumes: [raw.vendor_x]
      produces:
        - name: mart.vendor_x
          freshness: 1h
`
	def, err := Parse([]byte(valid))
	require.NoError(t, err)
	require.NotNil(t, def.Metadata.Datasets)
	require.NotNil(t, def.Metadata.Datasets.SkipWhenFresh)
	require.False(t, *def.Metadata.Datasets.SkipWhenFresh)
	// Validated with its definition, the same freshness trigger is accepted.
	require.NoError(t, ValidateTriggerSpec(&def.Trigger, def))

	missingDatasets := strings.Replace(valid, "      consumes: [raw.vendor_x]\n", "", 1)
	_, err = Parse([]byte(missingDatasets))
	require.ErrorContains(t, err, "requires at least one consumed dataset")
}

// A freshness job that binds its consumed input through metadata.datasets.sources
// (the external-arrival path) rather than a step-level consumes still satisfies
// the "has a consumed input" requirement, because BuildDeclarations persists each
// source as a declaration.
func TestValidateFreshnessTriggerAcceptsMetadataSourcesAsConsumedInput(t *testing.T) {
	t.Setenv("CAESIUM_FRESHNESS_ENABLED", "true")
	sourcesOnly := `
apiVersion: v1
kind: Job
metadata:
  alias: freshness-sources
  datasets:
    sources:
      - name: raw.vendor_x
        external: true
        arrival:
          event:
            type: s3.object.created
          watermark: $.detail.object.key
trigger:
  type: freshness
  configuration: {}
steps:
  - name: build
    image: alpine:3.23
    datasets:
      produces:
        - name: mart.vendor_x
          freshness: 1h
`
	def, err := Parse([]byte(sourcesOnly))
	require.NoError(t, err)
	require.NoError(t, ValidateTriggerSpec(&def.Trigger, def))
}

func TestStepUnmarshalJSONAppliesDefaults(t *testing.T) {
	var step Step
	err := json.Unmarshal([]byte(`{"name":"emit","image":"alpine:3.23","command":["echo","ok"]}`), &step)
	if err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	if step.Type != StepTypeTask {
		t.Fatalf("step type = %q, want %q", step.Type, StepTypeTask)
	}

	if step.Engine != EngineDocker {
		t.Fatalf("step engine = %q, want %q", step.Engine, EngineDocker)
	}
}

func TestStepUnmarshalJSONPreservesFalseCacheOverride(t *testing.T) {
	var step Step
	err := json.Unmarshal([]byte(`{"name":"emit","image":"alpine:3.23","cache":false}`), &step)
	if err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}

	cache, ok := step.Cache.(bool)
	if !ok {
		t.Fatalf("step cache has type %T, want bool", step.Cache)
	}
	if cache {
		t.Fatalf("step cache = true, want false")
	}
}

func TestMarshalPreservesFalseCacheOverrides(t *testing.T) {
	def := Definition{
		APIVersion: APIVersionV1,
		Kind:       KindJob,
		Metadata: Metadata{
			Alias: "cache-false-json",
			Cache: false,
		},
		Trigger: Trigger{
			Type:          TriggerCron,
			Configuration: map[string]any{"expression": "0 0 * * *"},
		},
		Steps: []Step{
			{
				Name:  "step-a",
				Image: "alpine:3.23",
				Cache: false,
			},
		},
	}

	body, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal definition json: %v", err)
	}

	metadata, ok := decoded["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing from marshaled definition: %s", string(body))
	}
	if cache, ok := metadata["cache"].(bool); !ok || cache {
		t.Fatalf("metadata.cache not preserved as false: %s", string(body))
	}

	steps, ok := decoded["steps"].([]any)
	if !ok || len(steps) != 1 {
		t.Fatalf("steps missing from marshaled definition: %s", string(body))
	}
	step, ok := steps[0].(map[string]any)
	if !ok {
		t.Fatalf("step missing from marshaled definition: %s", string(body))
	}
	if cache, ok := step["cache"].(bool); !ok || cache {
		t.Fatalf("step.cache not preserved as false: %s", string(body))
	}
}

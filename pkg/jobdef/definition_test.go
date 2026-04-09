package jobdef

import (
	"encoding/json"
	"testing"

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
	}

	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func TestValidateSimpleJSONPath(t *testing.T) {
	t.Parallel()

	valid := []string{"$", "$.ref", "$.sender.login", "$.items.0.name"}
	for _, expr := range valid {
		require.NoError(t, validateSimpleJSONPath(expr), expr)
	}

	invalid := []string{"", "ref", "$.", "$.sender..login", "$.sender. login"}
	for _, expr := range invalid {
		require.Error(t, validateSimpleJSONPath(expr), expr)
	}
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

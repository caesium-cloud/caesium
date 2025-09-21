package jobdef

import "testing"

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

func TestParseValidDefinitions(t *testing.T) {
	defs := []string{example1, example2}

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
	}

	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

package jobdef

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
)

const datasetsJob = `
apiVersion: v1
kind: Job
metadata:
  alias: orders-daily
  datasets:
    sources:
      - name: raw.vendor_x
        expectedEvery: 24h
        external: true
        arrival:
          event:
            type: "s3:ObjectCreated"
            filter: { "detail.bucket.name": "vendor-x-drop" }
          watermark: "$.detail.object.key"
trigger:
  type: cron
  configuration:
    expression: "0 */6 * * *"
steps:
  - name: extract
    image: etl:1.4
    datasets:
      consumes: [raw.vendor_x]
      produces:
        - name: staging.orders
          freshness: 8h
          watermark: { key: max_order_ts }
  - name: transform
    image: etl:1.4
    datasets:
      consumes: [staging.orders]
      produces:
        - name: analytics.orders_daily
          freshness: 6h
          maxStaleness: 12h
          watermark: { key: max_order_ts }
`

func TestParseDatasetsSurface(t *testing.T) {
	def, err := Parse([]byte(datasetsJob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if def.Metadata.Datasets == nil || len(def.Metadata.Datasets.Sources) != 1 {
		t.Fatalf("expected 1 source, got %+v", def.Metadata.Datasets)
	}
	src := def.Metadata.Datasets.Sources[0]
	if src.Name != "raw.vendor_x" || src.ExpectedEvery != "24h" || !src.External {
		t.Fatalf("unexpected source: %+v", src)
	}
	if src.Arrival == nil || src.Arrival.Event == nil || src.Arrival.Event.Type != "s3:ObjectCreated" {
		t.Fatalf("unexpected arrival: %+v", src.Arrival)
	}
	if src.Arrival.Watermark != "$.detail.object.key" {
		t.Fatalf("unexpected arrival watermark: %q", src.Arrival.Watermark)
	}

	extract := def.Steps[0]
	if extract.Datasets == nil || len(extract.Datasets.Consumes) != 1 || extract.Datasets.Consumes[0].Name != "raw.vendor_x" {
		t.Fatalf("unexpected extract consumes: %+v", extract.Datasets)
	}
	consumeJSON, err := json.Marshal(extract.Datasets.Consumes)
	if err != nil {
		t.Fatalf("marshal consumes: %v", err)
	}
	if strings.Contains(string(consumeJSON), `"raw.vendor_x"`) && !strings.Contains(string(consumeJSON), `"name":"raw.vendor_x"`) {
		t.Fatalf("consumes JSON should use object shape, got %s", consumeJSON)
	}
	if len(extract.Datasets.Produces) != 1 {
		t.Fatalf("expected 1 produced dataset")
	}
	p := extract.Datasets.Produces[0]
	if p.Name != "staging.orders" || p.Freshness != "8h" || p.Watermark == nil || p.Watermark.Key != "max_order_ts" {
		t.Fatalf("unexpected produced dataset: %+v", p)
	}
}

func TestParseDatasetContractSchemas(t *testing.T) {
	y := `
apiVersion: v1
kind: Job
metadata:
  alias: contracts
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: export
    image: etl:1
    outputSchema:
      type: object
      required: [customer_id, row_count]
      properties:
        customer_id: {type: string}
        row_count: {type: integer}
    datasets:
      produces:
        - name: lake.vendor_x_customers
          schemaFrom: output
          version: 2
  - name: load
    image: etl:1
    datasets:
      consumes:
        - name: lake.vendor_x_customers
          schema:
            type: object
            required: [customer_id]
            properties:
              customer_id: {type: string}
`
	def, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	produced := def.Steps[0].Datasets.Produces[0]
	if produced.SchemaFrom != DatasetSchemaFromOutput || produced.Version != 2 || produced.Schema != nil {
		t.Fatalf("unexpected produced contract schema fields: %+v", produced)
	}
	consumed := def.Steps[1].Datasets.Consumes[0]
	if consumed.Name != "lake.vendor_x_customers" || consumed.Schema == nil {
		t.Fatalf("unexpected consumed contract schema fields: %+v", consumed)
	}
}

// TestConsumesSurviveJSONRoundTrip pins the CLI→server apply path: a legacy
// scalar `consumes: [name]` manifest is parsed from YAML by the CLI, marshaled
// to JSON for the apply API (which emits the object shape), unmarshaled
// server-side, and re-validated. The scalar-vs-object distinction does not
// survive that round trip, so validation must not depend on it — a consumer
// without a schema stays valid in every shape. Regression for the freshness
// integration failures ("consumes[0].schema is required") on W1-ε.
func TestConsumesSurviveJSONRoundTrip(t *testing.T) {
	def, err := Parse([]byte(datasetStep(`consumes: [orders]`)))
	if err != nil {
		t.Fatalf("parse legacy scalar consumes: %v", err)
	}

	encoded, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("marshal definition: %v", err)
	}
	var roundTripped Definition
	if err := json.Unmarshal(encoded, &roundTripped); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	if err := roundTripped.Validate(); err != nil {
		t.Fatalf("re-validate round-tripped definition: %v", err)
	}
	got := roundTripped.Steps[0].Datasets.Consumes[0]
	if got.Name != "orders" || got.Schema != nil {
		t.Fatalf("unexpected round-tripped consume: %+v", got)
	}

	// The object form without a schema is equally valid straight from YAML.
	if _, err := Parse([]byte(datasetStep(`consumes: [{name: orders}]`))); err != nil {
		t.Fatalf("parse object-form consumes without schema: %v", err)
	}
}

// TestConsumesResolveYAMLAliases pins anchor/alias support: an aliased scalar
// consumes entry must dereference to its target rather than erroring on
// yaml.AliasNode.
func TestConsumesResolveYAMLAliases(t *testing.T) {
	y := `
apiVersion: v1
kind: Job
metadata:
  alias: aliased
  datasets:
    sources:
      - name: &upstream orders
        external: true
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: s
    image: etl:1
    datasets: {consumes: [*upstream]}
`
	def, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse aliased consumes: %v", err)
	}
	if got := def.Steps[0].Datasets.Consumes[0].Name; got != "orders" {
		t.Fatalf("aliased consume resolved to %q, want %q", got, "orders")
	}
}

// datasetStep builds a minimal single-step job whose step carries the provided
// inline `datasets:` body, for table-driven validation tests.
func datasetStep(datasetsBody string) string {
	return `
apiVersion: v1
kind: Job
metadata:
  alias: j
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: s
    image: etl:1
    datasets: {` + datasetsBody + `}
`
}

func TestValidateDatasetsRejectsBadInputs(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "bad freshness duration",
			yaml: datasetStep(`produces: [{name: a, freshness: "6hours"}]`),
			want: "freshness",
		},
		{
			name: "bad maxStaleness duration",
			yaml: datasetStep(`produces: [{name: a, maxStaleness: nope}]`),
			want: "maxStaleness",
		},
		{
			name: "non-positive freshness duration",
			yaml: datasetStep(`produces: [{name: a, freshness: "-6h"}]`),
			want: "freshness \"-6h\" must be a positive duration",
		},
		{
			name: "zero maxStaleness duration",
			yaml: datasetStep(`produces: [{name: a, maxStaleness: "0s"}]`),
			want: "maxStaleness \"0s\" must be a positive duration",
		},
		{
			name: "watermark without key",
			yaml: datasetStep(`produces: [{name: a, watermark: {}}]`),
			want: "watermark.key is required",
		},
		{
			name: "duplicate produced name",
			yaml: datasetStep(`produces: [{name: a}, {name: a}]`),
			want: "produced more than once",
		},
		{
			name: "empty consumes entry",
			yaml: datasetStep(`consumes: [""]`),
			want: "must not be empty",
		},
		{
			name: "duplicate consumes entry",
			yaml: datasetStep(`consumes: [a, a]`),
			want: "duplicate entry",
		},
		{
			name: "produced schema and schemaFrom are mutually exclusive",
			yaml: datasetStep(`produces: [{name: a, schemaFrom: output, schema: {type: object}}]`),
			want: "cannot set both schema and schemaFrom",
		},
		{
			name: "produced schemaFrom must be output",
			yaml: datasetStep(`produces: [{name: a, schemaFrom: input}]`),
			want: `schemaFrom "input" must be "output"`,
		},
		{
			name: "schemaFrom output requires outputSchema",
			yaml: datasetStep(`produces: [{name: a, schemaFrom: output}]`),
			want: "requires step \"s\" to declare outputSchema",
		},
		{
			name: "bad produced schema",
			yaml: datasetStep(`produces: [{name: a, schema: {type: nope}}]`),
			want: "produces[0].schema: invalid schema",
		},
		{
			name: "bad consumed schema",
			yaml: datasetStep(`consumes: [{name: a, schema: {type: nope}}]`),
			want: "consumes[0].schema: invalid schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateDatasetsRejectsBadArrivalJSONPath(t *testing.T) {
	y := `
apiVersion: v1
kind: Job
metadata:
  alias: j
  datasets:
    sources:
      - name: raw.x
        arrival:
          watermark: "not a jsonpath"
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: s
    image: etl:1
`
	_, err := Parse([]byte(y))
	if err == nil || !strings.Contains(err.Error(), "arrival.watermark") {
		t.Fatalf("expected arrival.watermark error, got %v", err)
	}
}

// TestValidateDatasetsAllowsCrossJobConsume asserts the single-definition
// validator does NOT reject a consumes name that resolves to no dataset in this
// job — cross-job resolution is the batch validator's responsibility (A3).
func TestValidateDatasetsAllowsCrossJobConsume(t *testing.T) {
	y := `
apiVersion: v1
kind: Job
metadata:
  alias: downstream
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: rollup
    image: etl:1
    datasets:
      consumes: [analytics.orders_daily]
`
	if _, err := Parse([]byte(y)); err != nil {
		t.Fatalf("cross-job consume should validate at single-def level, got %v", err)
	}
}

// TestDatasetsDoNotAffectCacheHash proves datasets are scheduling metadata:
// adding a datasets block to a step leaves the container spec that feeds the
// cache identity — and thus HashInput.Compute() — byte-identical.
func TestDatasetsDoNotAffectCacheHash(t *testing.T) {
	base := `
apiVersion: v1
kind: Job
metadata:
  alias: j
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: extract
    image: etl:1.4
    command: ["sh", "-c", "run"]
`
	withDatasets := `
apiVersion: v1
kind: Job
metadata:
  alias: j
  datasets:
    sources:
      - name: raw.x
        external: true
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: extract
    image: etl:1.4
    command: ["sh", "-c", "run"]
    datasets:
      consumes: [raw.x]
      produces:
        - name: staging.orders
          freshness: 6h
          maxStaleness: 12h
          watermark: { key: max_order_ts }
`

	hashFor := func(y string) string {
		def, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		step := &def.Steps[0]
		spec, err := def.RuntimeSpecForStep(step)
		if err != nil {
			t.Fatalf("runtime spec: %v", err)
		}
		h := cache.HashInput{
			JobAlias:             def.Metadata.Alias,
			TaskName:             step.Name,
			Image:                step.Image,
			Command:              step.Command,
			Env:                  spec.Env,
			WorkDir:              spec.WorkDir,
			Mounts:               spec.Mounts,
			ResolvedVolumeMounts: spec.ResolvedVolumeMounts,
			Kubernetes:           spec.Kubernetes,
		}
		return h.Compute()
	}

	if got, want := hashFor(withDatasets), hashFor(base); got != want {
		t.Fatalf("datasets changed the cache hash: with=%s without=%s", got, want)
	}
}

package freshness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openRegistryDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql.DB: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(models.All...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

const registrySampleJob = `
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
          watermark: "$.detail.object.key"
trigger:
  type: cron
  configuration: {expression: "0 */6 * * *"}
steps:
  - name: extract
    image: etl:1.4
    datasets:
      consumes: [raw.vendor_x]
      produces:
        - name: staging.orders
          freshness: 8h
          watermark: { key: max_order_ts }
`

func TestBuildDeclarations(t *testing.T) {
	def, err := schema.Parse([]byte(registrySampleJob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	jobID := uuid.New()
	decls, err := BuildDeclarations(def, jobID, def.Metadata.Alias)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// 1 source + 1 consume + 1 produce
	if len(decls) != 3 {
		t.Fatalf("expected 3 declarations, got %d: %+v", len(decls), decls)
	}

	byDir := map[string]models.DatasetDeclaration{}
	for _, d := range decls {
		byDir[d.Direction] = d
		if d.JobID != jobID || d.JobAlias != "orders-daily" {
			t.Fatalf("declaration missing job identity: %+v", d)
		}
	}

	src := byDir[models.DatasetDirectionSource]
	if src.Name != "raw.vendor_x" || src.ExpectedEvery != "24h" || !src.External {
		t.Fatalf("unexpected source declaration: %+v", src)
	}
	if len(src.ArrivalBinding) == 0 {
		t.Fatalf("expected arrival binding JSON on source")
	}
	var arrival schema.Arrival
	if err := json.Unmarshal(src.ArrivalBinding, &arrival); err != nil {
		t.Fatalf("arrival binding not valid JSON: %v", err)
	}
	if arrival.Watermark != "$.detail.object.key" {
		t.Fatalf("arrival binding watermark = %q", arrival.Watermark)
	}

	prod := byDir[models.DatasetDirectionProduces]
	if prod.Name != "staging.orders" || prod.Freshness != "8h" || prod.WatermarkKey != "max_order_ts" || prod.StepName != "extract" {
		t.Fatalf("unexpected produce declaration: %+v", prod)
	}
	if prod.SkipWhenFresh == nil || !*prod.SkipWhenFresh {
		t.Fatalf("skipWhenFresh default = %v, want true", prod.SkipWhenFresh)
	}

	con := byDir[models.DatasetDirectionConsumes]
	if con.Name != "raw.vendor_x" || con.StepName != "extract" {
		t.Fatalf("unexpected consume declaration: %+v", con)
	}
}

const registrySchemaJob = `
apiVersion: v1
kind: Job
metadata:
  alias: contract-datasets
trigger:
  type: cron
  configuration: {expression: "0 */6 * * *"}
steps:
  - name: transform
    image: etl:1.4
    outputSchema:
      type: object
      required: [customer_id]
      properties:
        customer_id:
          type: string
    datasets:
      consumes:
        - name: raw.customers
          schema:
            type: object
            required: [legacy_id]
            properties:
              legacy_id:
                type: string
      produces:
        - name: lake.customers
          schemaFrom: output
          version: 2
        - name: lake.orders
          schema:
            type: object
            required: [order_id]
            properties:
              order_id:
                type: string
          version: 5
`

func TestBuildDeclarationsPersistsSchemaMetadata(t *testing.T) {
	def, err := schema.Parse([]byte(registrySchemaJob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	jobID := uuid.New()
	decls, err := BuildDeclarations(def, jobID, def.Metadata.Alias)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	db := openRegistryDB(t)
	if err := ReplaceForJobTx(db, jobID, decls); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := NewRegistry(db).ListByJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("list by job: %v", err)
	}

	consume := findDeclaration(t, got, models.DatasetDirectionConsumes, "raw.customers")
	if consume.SchemaFrom != "" || consume.SchemaVersion != 0 {
		t.Fatalf("unexpected consume schema source metadata: %+v", consume)
	}
	requireSchemaJSONRequired(t, consume.SchemaJSON, "legacy_id")

	output := findDeclaration(t, got, models.DatasetDirectionProduces, "lake.customers")
	if output.SchemaJSON != "" || output.SchemaFrom != schema.DatasetSchemaFromOutput || output.SchemaVersion != 2 {
		t.Fatalf("unexpected schemaFrom declaration metadata: %+v", output)
	}

	inline := findDeclaration(t, got, models.DatasetDirectionProduces, "lake.orders")
	if inline.SchemaFrom != "" || inline.SchemaVersion != 5 {
		t.Fatalf("unexpected inline declaration metadata: %+v", inline)
	}
	requireSchemaJSONRequired(t, inline.SchemaJSON, "order_id")
}

func TestBuildDeclarationsCarriesSkipWhenFreshOptOut(t *testing.T) {
	src := strings.Replace(registrySampleJob, "  datasets:\n    sources:", "  datasets:\n    skipWhenFresh: false\n    sources:", 1)
	def, err := schema.Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	decls, err := BuildDeclarations(def, uuid.New(), def.Metadata.Alias)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, decl := range decls {
		if decl.SkipWhenFresh == nil || *decl.SkipWhenFresh {
			t.Fatalf("declaration %s skipWhenFresh = %v, want false", decl.Name, decl.SkipWhenFresh)
		}
	}
}

func findDeclaration(t *testing.T, decls []models.DatasetDeclaration, direction, name string) models.DatasetDeclaration {
	t.Helper()
	for _, decl := range decls {
		if decl.Direction == direction && decl.Name == name {
			return decl
		}
	}
	t.Fatalf("declaration %s %s not found in %+v", direction, name, decls)
	return models.DatasetDeclaration{}
}

func requireSchemaJSONRequired(t *testing.T, raw, required string) {
	t.Helper()
	if raw == "" {
		t.Fatalf("schema_json is empty, want required key %q", required)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("schema_json is not valid JSON: %v", err)
	}
	values, ok := parsed["required"].([]any)
	if !ok {
		t.Fatalf("schema_json required = %#v, want list containing %q", parsed["required"], required)
	}
	for _, value := range values {
		if value == required {
			return
		}
	}
	t.Fatalf("schema_json required = %#v, want %q", values, required)
}

func TestReplaceForJobTxRebuildsAndPrunes(t *testing.T) {
	db := openRegistryDB(t)
	ctx := context.Background()
	reg := NewRegistry(db)
	jobID := uuid.New()

	decls := []models.DatasetDeclaration{
		{ID: uuid.New(), JobID: jobID, JobAlias: "j", Name: "a", Direction: models.DatasetDirectionProduces},
		{ID: uuid.New(), JobID: jobID, JobAlias: "j", Name: "b", Direction: models.DatasetDirectionConsumes},
	}
	if err := ReplaceForJobTx(db, jobID, decls); err != nil {
		t.Fatalf("replace: %v", err)
	}
	got, err := reg.ListByJob(ctx, jobID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}

	// Rebuild with a smaller set → the removed declaration is pruned.
	fewer := []models.DatasetDeclaration{
		{ID: uuid.New(), JobID: jobID, JobAlias: "j", Name: "a", Direction: models.DatasetDirectionProduces},
	}
	if err := ReplaceForJobTx(db, jobID, fewer); err != nil {
		t.Fatalf("replace 2: %v", err)
	}
	got, err = reg.ListByJob(ctx, jobID)
	if err != nil {
		t.Fatalf("list 2: %v", err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("expected only dataset 'a' after rebuild, got %+v", got)
	}

	// DeleteForJobsTx clears the job entirely.
	if err := DeleteForJobsTx(db, []uuid.UUID{jobID}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, err = reg.ListAll(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 rows after delete, got %d", len(got))
	}
}

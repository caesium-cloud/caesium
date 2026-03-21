package database

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSchemaIncludesTablesColumnsAndCounts(t *testing.T) {
	svc := newTestService(t)

	schema, err := svc.Schema()
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}

	if schema.Dialect != "sqlite" {
		t.Fatalf("schema dialect = %q, want sqlite", schema.Dialect)
	}
	if !schema.ReadOnly {
		t.Fatalf("schema read_only = false, want true")
	}

	var jobsTable *TableSchema
	for i := range schema.Tables {
		if schema.Tables[i].Name == "jobs" {
			jobsTable = &schema.Tables[i]
			break
		}
	}
	if jobsTable == nil {
		t.Fatalf("jobs table not found in schema")
	}
	if jobsTable.RowCount != 2 {
		t.Fatalf("jobs row_count = %d, want 2", jobsTable.RowCount)
	}

	var foundAlias bool
	for _, column := range jobsTable.Columns {
		if column.Name == "alias" {
			foundAlias = true
			break
		}
	}
	if !foundAlias {
		t.Fatalf("jobs.alias column not found in schema")
	}
}

func TestQueryReturnsRowsAndTruncates(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.Query(QueryRequest{
		SQL:   "SELECT alias, paused FROM jobs ORDER BY alias ASC",
		Limit: 1,
	})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}

	if resp.StatementType != "select" {
		t.Fatalf("statement_type = %q, want select", resp.StatementType)
	}
	if len(resp.Columns) != 2 {
		t.Fatalf("len(columns) = %d, want 2", len(resp.Columns))
	}
	if resp.Columns[0].Name != "alias" {
		t.Fatalf("first column = %q, want alias", resp.Columns[0].Name)
	}
	if resp.RowCount != 1 {
		t.Fatalf("row_count = %d, want 1", resp.RowCount)
	}
	if !resp.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if got := resp.Rows[0][0]; got != "alpha" {
		t.Fatalf("first row alias = %v, want alpha", got)
	}
}

func TestQueryRejectsUnsafeStatements(t *testing.T) {
	svc := newTestService(t)

	for _, query := range []string{
		"DELETE FROM jobs",
		"SELECT * FROM jobs; SELECT * FROM triggers",
		"CREATE TABLE debug(id INTEGER)",
	} {
		if _, err := svc.Query(QueryRequest{SQL: query}); err == nil {
			t.Fatalf("Query(%q) unexpectedly succeeded", query)
		}
	}
}

func newTestService(t *testing.T) *Service {
	t.Helper()

	gdb, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatalf("open sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})

	if err := gdb.AutoMigrate(models.All...); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}

	now := time.Now().UTC()
	triggerID := uuid.New()
	if err := gdb.Create(&models.Trigger{
		ID:            triggerID,
		Alias:         "manual",
		Type:          models.TriggerTypeHTTP,
		Configuration: `{"path":"/run"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error; err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	for _, job := range []models.Job{
		{
			ID:        uuid.New(),
			Alias:     "alpha",
			TriggerID: triggerID,
			Paused:    false,
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        uuid.New(),
			Alias:     "beta",
			TriggerID: triggerID,
			Paused:    true,
			CreatedAt: now,
			UpdatedAt: now,
		},
	} {
		if err := gdb.Create(&job).Error; err != nil {
			t.Fatalf("create job %s: %v", job.Alias, err)
		}
	}

	return NewWithDB(context.Background(), gdb)
}

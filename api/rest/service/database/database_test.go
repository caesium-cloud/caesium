package database

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestReadOnlyGuardLexicalStates(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  error
	}{
		{"SELECT 1;", nil},
		{"/* DELETE; */ -- UPDATE;\nSELECT 1", nil},
		{"SELECT 'DELETE; ''UPDATE'", nil},
		{"SELECT \"DELETE;\", `UPDATE;`", nil},
		{"SELECT 1 -- DELETE;\n", nil},
		{"SELECT 1 /* DELETE;\nUPDATE */", nil},
		{"SELECT 'unterminated DELETE;", nil},
		{"SELECT \"unterminated DELETE;", nil},
		{"SELECT `unterminated DELETE;", nil},
		{"SELECT 1 /* unterminated DELETE;", nil},
		{"/* unterminated SELECT 1", ErrEmptyQuery},
		{"-- SELECT 1", ErrEmptyQuery},
		{"SELECT 1; -- comment", ErrMultipleStatements},
		{"SELECT 1; /* comment */", ErrMultipleStatements},
		{"SELECT 1;;", ErrMultipleStatements},
		{"SELECT (1; 2)", ErrMultipleStatements},
		{"SELECT 'safe'; DELETE FROM jobs", ErrMultipleStatements},
		{"SELECT 1 /* safe */ DELETE", ErrUnsafeQuery},
		{"'safe' SELECT 1", ErrUnsafeQuery},
		{"SELECT 'backslash\\'; DELETE", ErrMultipleStatements},
		{"SELECT INTO archive FROM jobs", ErrUnsafeQuery},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got := validateReadOnlyQuery(tc.query); !errors.Is(got, tc.want) {
				t.Fatalf("guard = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSQLMaskPreservesBytePositions(t *testing.T) {
	for _, tc := range []struct{ query, masked string }{
		{"SELECT 'x;''y' AS x", "SELECT " + strings.Repeat(" ", 7) + " AS x"},
		{"SELECT \"x;\", `y;`", "SELECT     ,     "},
		{"-- x;\nSELECT 1", "     \nSELECT 1"},
		{"/* x;\n*/SELECT 1", "        SELECT 1"},
		{"SELECT 'é'", "SELECT " + strings.Repeat(" ", 4)},
		{"SELECT 'x", "SELECT   "},
	} {
		t.Run(tc.query, func(t *testing.T) {
			if got, _ := scanSQL(tc.query); got != tc.masked || len(got) != len(tc.query) {
				t.Fatalf("mask = %q (%d bytes), want %q (%d bytes)", got, len(got), tc.masked, len(tc.query))
			}
		})
	}
}

func TestSchemaIncludesTablesAndColumns(t *testing.T) {
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
	if jobsTable.RowCount != nil {
		t.Fatalf("jobs row_count = %v, want nil", *jobsTable.RowCount)
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
		"SELECT  INTO archive_jobs FROM jobs",
	} {
		if _, err := svc.Query(QueryRequest{SQL: query}); err == nil {
			t.Fatalf("Query(%q) unexpectedly succeeded", query)
		}
	}
}

func TestQueryAllowsKeywordsInsideLiterals(t *testing.T) {
	svc := newTestService(t)

	resp, err := svc.Query(QueryRequest{
		SQL: "SELECT 'DELETE' AS verb, alias FROM jobs ORDER BY alias ASC",
	})
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if resp.RowCount != 2 {
		t.Fatalf("row_count = %d, want 2", resp.RowCount)
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

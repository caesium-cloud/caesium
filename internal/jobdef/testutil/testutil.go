package testutil

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// SampleJob is a baseline manifest used across importer/git tests.
const SampleJob = `
apiVersion: v1
kind: Job
metadata:
  alias: csv-to-parquet
  labels:
    team: data
  annotations:
    owner: etl
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
    timezone: "UTC"
callbacks:
  - type: notification
    configuration:
      url: "https://example"
steps:
  - name: list
    image: busybox:1.36
    command: ["sh", "-c", "echo list"]
  - name: convert
    image: busybox:1.36
    command: ["sh", "-c", "echo convert"]
  - name: publish
    image: busybox:1.36
    command: ["sh", "-c", "echo publish"]
`

// OpenTestDB returns an in-memory sqlite DB with migrations applied.
func OpenTestDB(tb testing.TB) *gorm.DB {
	tb.Helper()

	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		tb.Fatalf("open sqlite: %v", err)
	}

	if err := db.AutoMigrate(models.All...); err != nil {
		tb.Fatalf("migrate: %v", err)
	}

	return db
}

// CloseDB closes the underlying sql.DB if available.
func CloseDB(db *gorm.DB) {
	if db == nil {
		return
	}
	if sqlDB, err := db.DB(); err == nil {
		if err := sqlDB.Close(); err != nil {
			panic(err)
		}
	}
}

// AssertCount asserts a count for the provided model using the supplied DB.
func AssertCount(tb testing.TB, db *gorm.DB, model any, expected int64) {
	tb.Helper()

	var count int64
	if err := db.Model(model).Count(&count).Error; err != nil {
		tb.Fatalf("count: %v", err)
	}
	if count != expected {
		tb.Fatalf("expected %d records, got %d", expected, count)
	}
}

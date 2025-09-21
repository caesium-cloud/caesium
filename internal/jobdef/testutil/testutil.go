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
    image: ghcr.io/yourorg/s3ls:1.2
    command: ["s3ls"]
  - name: convert
    image: ghcr.io/yourorg/csv2pq:0.5
    command: ["csv2pq"]
  - name: publish
    image: ghcr.io/yourorg/uploader:0.3
    command: ["upload"]
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
		sqlDB.Close()
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

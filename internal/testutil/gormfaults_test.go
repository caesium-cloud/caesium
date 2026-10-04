package testutil

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFailJobRunReadsScopesAndCleansUpCallbacks(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, table := range []string{"job_runs", "other_rows"} {
		if err := db.Exec("CREATE TABLE " + table + " (id INTEGER)").Error; err != nil {
			t.Fatal(err)
		}
	}
	query := func(table string) error {
		var rows []struct{ ID int }
		return db.Table(table).Find(&rows).Error
	}
	t.Run("two installations", func(t *testing.T) {
		FailJobRunReads(t, db, 1)
		FailJobRunReads(t, db, 2)
		if err := query("other_rows"); err != nil {
			t.Fatalf("unrelated table: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := query("job_runs"); err == nil {
				t.Fatalf("query %d should fail", i)
			}
		}
		if err := query("job_runs"); err != nil {
			t.Fatalf("counter exhausted: %v", err)
		}
	})
	t.Run("unused installation", func(t *testing.T) { FailJobRunReads(t, db, 10) })
	if err := query("job_runs"); err != nil {
		t.Fatalf("callback cleanup: %v", err)
	}
}

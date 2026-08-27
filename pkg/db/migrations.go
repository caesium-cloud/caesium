package db

import (
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/gorm"
)

// taskRunJobRunTaskIndexName is the composite index on task_runs that fan-out
// widened. The NAME is unchanged from before fan-out — which is exactly the
// problem this migration exists to solve.
const taskRunJobRunTaskIndexName = "idx_taskrun_jobrun_task"

// MigrateTaskRunUniquePartitionIndex drops a pre-fan-out
// idx_taskrun_jobrun_task so AutoMigrate can recreate it in its fan-out shape:
// UNIQUE over (job_run_id, task_id, partition_index).
//
// Why an explicit migration is required. Stream A1 changed the struct tags on
// models.TaskRun from `index:idx_taskrun_jobrun_task` to
// `uniqueIndex:idx_taskrun_jobrun_task` and added partition_index as a third
// member — but kept the index NAME. GORM's AutoMigrate only ever asks
// "does an index with this name exist?"; when it does, it leaves it completely
// alone. It never compares the existing index's columns or its uniqueness
// against the model. So on a fresh database the new tags produce the right
// index and everything looks correct in CI, while every EXISTING deployment
// silently keeps the old two-column, non-unique index forever. The unique
// (job_run_id, task_id, partition_index) constraint — the core invariant that
// makes a TaskRun row addressable under fan-out, and the DB-level backstop
// against a double expansion inserting duplicate instances — would simply never
// be enforced in production, with no error anywhere.
//
// The fix is idempotent and safe to run on every boot:
//
//  1. Read the existing index definition (nothing to do if there is none —
//     AutoMigrate creates the correct one).
//  2. If it is already UNIQUE and already covers partition_index, leave it.
//  3. Otherwise DROP it; AutoMigrate, which runs immediately after, recreates it
//     from the current struct tags.
//
// Dropping is safe with respect to data: the new index is strictly wider than
// the old one, so any row set the old index permitted (one row per
// (job_run_id, task_id), i.e. partition_index 0) satisfies the new uniqueness,
// and a database that already holds fanned rows has distinct partition_index
// values per group by construction. There is no window in which queries lose
// their index either — both steps run inside the same Migrate() call before the
// server starts serving.
func MigrateTaskRunUniquePartitionIndex(conn *gorm.DB) error {
	if conn == nil {
		return nil
	}
	definition, found, err := taskRunIndexDefinition(conn)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if indexIsUniqueOverPartitionIndex(definition) {
		return nil
	}
	log.Info("migrating task_runs composite index to UNIQUE (job_run_id, task_id, partition_index)",
		"index", taskRunJobRunTaskIndexName)
	if err := conn.Exec(fmt.Sprintf("DROP INDEX IF EXISTS %s", taskRunJobRunTaskIndexName)).Error; err != nil {
		return fmt.Errorf("db: drop stale %s: %w", taskRunJobRunTaskIndexName, err)
	}
	return nil
}

// taskRunIndexDefinition returns the DDL of the existing composite index, and
// whether it exists at all. Dialect-aware: the dqlite/sqlite family keeps index
// DDL in sqlite_master, Postgres in pg_indexes.
func taskRunIndexDefinition(conn *gorm.DB) (string, bool, error) {
	var (
		definition string
		query      string
		args       []interface{}
	)
	switch strings.ToLower(conn.Name()) {
	case "postgres":
		query = "SELECT COALESCE(indexdef, '') FROM pg_indexes WHERE tablename = 'task_runs' AND indexname = ?"
		args = []interface{}{taskRunJobRunTaskIndexName}
	default:
		// dqlite, sqlite, sqlite3 — all sqlite_master-shaped.
		query = "SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND name = ?"
		args = []interface{}{taskRunJobRunTaskIndexName}
	}

	rows, err := conn.Raw(query, args...).Rows()
	if err != nil {
		// A missing catalog table (sqlite_master always exists; pg_indexes always
		// exists) is not expected, but a brand-new database with no task_runs
		// table simply yields no rows rather than an error.
		return "", false, fmt.Errorf("db: inspect %s: %w", taskRunJobRunTaskIndexName, err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		return "", false, rows.Err()
	}
	if err := rows.Scan(&definition); err != nil {
		return "", false, fmt.Errorf("db: scan %s definition: %w", taskRunJobRunTaskIndexName, err)
	}
	return definition, true, rows.Err()
}

// indexIsUniqueOverPartitionIndex reports whether an index DDL already matches
// the post-fan-out shape. An index row with an EMPTY definition (sqlite reports
// NULL sql for indexes it created implicitly from a table constraint) is treated
// as not matching, so it is dropped and rebuilt from the struct tags — the
// conservative direction.
func indexIsUniqueOverPartitionIndex(definition string) bool {
	lowered := strings.ToLower(definition)
	return strings.Contains(lowered, "unique") && strings.Contains(lowered, "partition_index")
}

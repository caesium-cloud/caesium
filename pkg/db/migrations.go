package db

import (
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
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
		args       []any
	)
	switch strings.ToLower(conn.Name()) {
	case "postgres":
		query = "SELECT COALESCE(indexdef, '') FROM pg_indexes WHERE tablename = 'task_runs' AND indexname = ?"
		args = []any{taskRunJobRunTaskIndexName}
	default:
		// dqlite, sqlite, sqlite3 — all sqlite_master-shaped.
		query = "SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'index' AND name = ?"
		args = []any{taskRunJobRunTaskIndexName}
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

// MigrateNamespaceDefaults repairs legacy NULL/empty ownership before
// AutoMigrate tightens the columns. It never derives historical ownership from
// the mutable job: old rows belong to default; explicit namespaces are retained.
// Run this on catalog, hot shards and cold storage because JobRun is sharded.
func MigrateNamespaceDefaults(conn *gorm.DB) error {
	if conn == nil {
		return nil
	}
	return conn.Transaction(func(tx *gorm.DB) error {
		for _, model := range []any{&models.Job{}, &models.JobRun{}, &models.Backfill{}, &models.Incident{}} {
			if !tx.Migrator().HasTable(model) || !tx.Migrator().HasColumn(model, "Namespace") {
				continue
			}
			if err := tx.Unscoped().Model(model).Where("namespace IS NULL OR namespace = ''").UpdateColumn("namespace", models.DefaultNamespace).Error; err != nil {
				return fmt.Errorf("db: normalize namespace on %T: %w", model, err)
			}
		}
		if !tx.Migrator().HasTable(&models.Incident{}) {
			return nil
		}
		var incidents []models.Incident
		if err := tx.Select("id", "job_id", "task_name", "class", "namespace", "dedupe_key", "active_dedupe_key").Find(&incidents).Error; err != nil {
			return err
		}
		// A partially migrated or otherwise invalid catalog can hold both the
		// legacy and namespaced active keys for one incident identity. Do not
		// merge or suppress either history: stop the upgrade with their IDs.
		activeOwners := make(map[string]string, len(incidents))
		for _, inc := range incidents {
			if inc.ActiveDedupeKey == nil {
				continue
			}
			key := *inc.ActiveDedupeKey
			legacy := fmt.Sprintf("%s|%s|%s", inc.JobID, inc.TaskName, inc.Class)
			if inc.DedupeKey == legacy && key == legacy {
				key = fmt.Sprintf("%s|%s", models.NamespaceOrDefault(inc.Namespace), legacy)
			}
			if owner, exists := activeOwners[key]; exists {
				return fmt.Errorf("db: namespace migration active dedupe key collision %q between incidents %s and %s; resolve the conflicting incident records before retrying", key, owner, inc.ID)
			}
			activeOwners[key] = inc.ID.String()
		}
		for _, inc := range incidents {
			legacy := fmt.Sprintf("%s|%s|%s", inc.JobID, inc.TaskName, inc.Class)
			if inc.DedupeKey != legacy {
				continue
			}
			key := fmt.Sprintf("%s|%s", models.NamespaceOrDefault(inc.Namespace), legacy)
			updates := map[string]any{"dedupe_key": key}
			if inc.ActiveDedupeKey != nil && *inc.ActiveDedupeKey == legacy {
				updates["active_dedupe_key"] = key
			}
			if err := tx.Model(&models.Incident{}).Where("id = ?", inc.ID).Updates(updates).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ensureNamespaceConstraint covers dialects whose AutoMigrate skips tightening
// a nullable column when its other metadata already matches the new model.
func ensureNamespaceConstraint(conn *gorm.DB, model any) error {
	switch model.(type) {
	case *models.Job, *models.JobRun, *models.Backfill, *models.Incident:
	default:
		return nil
	}
	columns, err := conn.Migrator().ColumnTypes(model)
	if err != nil {
		return err
	}
	for _, column := range columns {
		if column.Name() != "namespace" {
			continue
		}
		if nullable, known := column.Nullable(); known && nullable {
			return conn.Migrator().AlterColumn(model, "Namespace")
		}
	}
	return nil
}

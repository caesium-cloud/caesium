package dqlite

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAlterColumnDDLExactFieldsAndConstraints(t *testing.T) {
	for _, tc := range []struct{ name, raw, column, want string }{
		{"first", "CREATE TABLE example (`first` text,middle text,last text)", "first", "CREATE TABLE example (`first` datetime NOT NULL,middle text,last text)"},
		{"interior", "CREATE TABLE example (first text,`middle` text,last text)", "middle", "CREATE TABLE example (first text,`middle` datetime NOT NULL,last text)"},
		{"last", "CREATE TABLE example (first text,middle text,`last` text)", "last", "CREATE TABLE example (first text,middle text,`last` datetime NOT NULL)"},
		{"exact", "CREATE TABLE example (last_suffix text,`last` text)", "last", "CREATE TABLE example (last_suffix text,`last` datetime NOT NULL)"},
		{"escaped other default", "CREATE TABLE example (first text DEFAULT 'a,b''c?',last text)", "last", "CREATE TABLE example (first text DEFAULT 'a,b''c?',last datetime NOT NULL)"},
		{"inline", "CREATE TABLE example (id integer PRIMARY KEY,last text CONSTRAINT valid CHECK(length(last)>0) COLLATE NOCASE UNIQUE DEFAULT 'old' NOT NULL)", "last", "CREATE TABLE example (id integer PRIMARY KEY,last datetime NOT NULL CONSTRAINT valid CHECK(length(last)>0) COLLATE NOCASE UNIQUE)"},
		{"references", "CREATE TABLE example (id integer,last text REFERENCES parent(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED)", "last", "CREATE TABLE example (id integer,last datetime NOT NULL REFERENCES parent(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED)"},
		{"primary", "CREATE TABLE example (last integer PRIMARY KEY AUTOINCREMENT)", "last", "CREATE TABLE example (last datetime NOT NULL PRIMARY KEY AUTOINCREMENT)"},
		{"table suffix", "CREATE TABLE example (id text PRIMARY KEY,last text) WITHOUT ROWID", "last", "CREATE TABLE example (id text PRIMARY KEY,last datetime NOT NULL) WITHOUT ROWID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := alterColumnDDL(tc.raw, tc.column, "datetime NOT NULL")
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestAlterColumnDDLRefusesMalformedMissingAndAmbiguous(t *testing.T) {
	for _, raw := range []string{
		"CREATE TABLE example (id text)",
		"CREATE TABLE example (last text,LAST text)",
		"CREATE TABLE example (last text DEFAULT 'unfinished)",
		"CREATE TABLE example (last text CHECK((last))",
		"CREATE TABLE example (last text,)",
		"CREATE TABLE example (last text); DROP TABLE example",
		"CREATE TABLE example (last text DEFAULT)",
		"CREATE TABLE example (last text CONSTRAINT named)",
	} {
		t.Run(raw, func(t *testing.T) { _, err := alterColumnDDL(raw, "last", "datetime"); require.Error(t, err) })
	}
}

type migrationColumns struct {
	First  string `gorm:"type:blob"`
	Middle string `gorm:"type:blob"`
	Last   string `gorm:"type:blob;not null"`
}

func (migrationColumns) TableName() string { return "migration_columns" }

func sqliteMigrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	pool, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	db, err := gorm.Open(Dialector{Conn: pool}, &gorm.Config{})
	require.NoError(t, err)
	return db
}

func TestAlterColumnPreservesRowsIndexesAndInlineConstraints(t *testing.T) {
	db := sqliteMigrationDB(t)
	require.NoError(t, db.Exec("CREATE TABLE migration_columns (first text,middle text,last text NOT NULL CHECK(length(last)>0) COLLATE NOCASE)").Error)
	for _, query := range []string{
		"CREATE INDEX idx_first ON migration_columns(first)",
		"CREATE UNIQUE INDEX idx_last_unique ON migration_columns(last)",
		"CREATE INDEX idx_partial ON migration_columns(middle) WHERE last <> 'skip'",
		"CREATE INDEX idx_expression ON migration_columns(lower(last))",
		"INSERT INTO migration_columns VALUES ('first','middle','keep')",
	} {
		require.NoError(t, db.Exec(query).Error)
	}
	indexes := migrationIndexSQL(t, db, "migration_columns")
	for _, name := range []string{"First", "Middle", "Last"} {
		require.NoError(t, db.Migrator().AlterColumn(&migrationColumns{}, name))
	}
	require.Equal(t, indexes, migrationIndexSQL(t, db, "migration_columns"))
	var row migrationColumns
	require.NoError(t, db.First(&row).Error)
	require.Equal(t, migrationColumns{First: "first", Middle: "middle", Last: "keep"}, row)
	var types []struct {
		Name string
		Type string
	}
	require.NoError(t, db.Raw("PRAGMA table_info(migration_columns)").Scan(&types).Error)
	require.Len(t, types, 3)
	var names []string
	for _, column := range types {
		names = append(names, column.Name)
		require.Equal(t, "BLOB", strings.ToUpper(column.Type))
	}
	require.Equal(t, []string{"first", "middle", "last"}, names)
	require.Error(t, db.Exec("INSERT INTO migration_columns VALUES ('a','b','')").Error)
	require.Error(t, db.Exec("INSERT INTO migration_columns VALUES ('a','b','KEEP')").Error)
	require.Error(t, db.Exec("INSERT INTO migration_columns VALUES ('a','b',NULL)").Error)
}

func migrationIndexSQL(t *testing.T, db *gorm.DB, table string) []string {
	t.Helper()
	var indexes []string
	require.NoError(t, db.Raw("SELECT sql FROM sqlite_master WHERE type='index' AND tbl_name=? AND sql IS NOT NULL ORDER BY name", table).Scan(&indexes).Error)
	return indexes
}

func TestAlterColumnIndexFailureRollsBackTableRowsAndIndexes(t *testing.T) {
	db := sqliteMigrationDB(t)
	require.NoError(t, db.Exec("CREATE TABLE migration_columns (first text,middle text,last text)").Error)
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX idx_restore ON migration_columns(last)").Error)
	require.NoError(t, db.Exec("INSERT INTO migration_columns VALUES ('a','b','c')").Error)
	before, err := db.Migrator().(Migrator).getRawDDL("migration_columns")
	require.NoError(t, err)
	indexes := migrationIndexSQL(t, db, "migration_columns")
	failure := errors.New("injected index restore failure")
	require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register("migration:fail-index", func(tx *gorm.DB) {
		if strings.HasPrefix(tx.Statement.SQL.String(), "CREATE UNIQUE INDEX idx_restore") {
			tx.AddError(failure)
		}
	}))
	require.ErrorIs(t, db.Migrator().AlterColumn(&migrationColumns{}, "Last"), failure)
	after, err := db.Migrator().(Migrator).getRawDDL("migration_columns")
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, indexes, migrationIndexSQL(t, db, "migration_columns"))
	var count int64
	require.NoError(t, db.Table("migration_columns").Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.False(t, db.Migrator().HasTable("migration_columns__temp"))
}

func TestPopulatedSAMLReplayAutoMigrateAgain(t *testing.T) {
	db := sqliteMigrationDB(t)
	assertPopulatedSAMLReplayMigration(t, db)
}

func assertPopulatedSAMLReplayMigration(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&models.SAMLAssertionReplay{}))
	require.NoError(t, db.Exec("INSERT INTO saml_assertion_ids VALUES (?, ?, ?, ?)", "issuer", "assertion", "2030-01-01T00:00:00Z", "2026-01-01T00:00:00Z").Error)
	indexes := migrationIndexSQL(t, db, "saml_assertion_ids")
	require.Len(t, indexes, 2)
	assertSAMLReplayDates(t, db)
	for range 2 {
		require.NoError(t, db.AutoMigrate(&models.SAMLAssertionReplay{}))
		assertSAMLReplayDates(t, db)
		require.Equal(t, indexes, migrationIndexSQL(t, db, "saml_assertion_ids"))
		require.NoError(t, db.Migrator().AlterColumn(&models.SAMLAssertionReplay{}, "CreatedAt"))
		assertSAMLReplayDates(t, db)
		require.Equal(t, indexes, migrationIndexSQL(t, db, "saml_assertion_ids"))
	}
	require.Error(t, db.Exec("INSERT INTO saml_assertion_ids VALUES (?, ?, ?, ?)", "issuer", "assertion", "2030-01-01T00:00:00Z", "2026-01-01T00:00:00Z").Error)
	return indexes
}

func assertSAMLReplayDates(t *testing.T, db *gorm.DB) {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.SAMLAssertionReplay{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	var row models.SAMLAssertionReplay
	require.NoError(t, db.First(&row).Error)
	require.Equal(t, "issuer", row.Issuer)
	require.Equal(t, "assertion", row.AssertionID)
	require.Equal(t, time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), row.CreatedAt.UTC())
	require.Equal(t, time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC), row.ExpiresAt.UTC())
}

func TestDropColumnDoesNotRestoreIndexesForRemovedColumn(t *testing.T) {
	db := sqliteMigrationDB(t)
	require.NoError(t, db.Exec("CREATE TABLE `migration_columns` (first text,middle text,last text)").Error)
	require.NoError(t, db.Exec("CREATE INDEX idx_removed ON migration_columns(first)").Error)
	require.NoError(t, db.Exec("INSERT INTO migration_columns VALUES ('a','b','c')").Error)
	require.NoError(t, db.Migrator().DropColumn(&migrationColumns{}, "First"))
	require.Empty(t, migrationIndexSQL(t, db, "migration_columns"))
	var row migrationColumns
	require.NoError(t, db.First(&row).Error)
	require.Equal(t, "b", row.Middle)
	require.Equal(t, "c", row.Last)
}

func TestAlterColumnCopyFailureRollsBack(t *testing.T) {
	db := sqliteMigrationDB(t)
	require.NoError(t, db.Exec("CREATE TABLE migration_columns (first text,middle text,last text)").Error)
	require.NoError(t, db.Exec("CREATE INDEX idx_copy ON migration_columns(first)").Error)
	require.NoError(t, db.Exec("INSERT INTO migration_columns VALUES ('a','b',NULL)").Error)
	before, err := db.Migrator().(Migrator).getRawDDL("migration_columns")
	require.NoError(t, err)
	indexes := migrationIndexSQL(t, db, "migration_columns")
	require.Error(t, db.Migrator().AlterColumn(&migrationColumns{}, "Last"))
	after, err := db.Migrator().(Migrator).getRawDDL("migration_columns")
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, indexes, migrationIndexSQL(t, db, "migration_columns"))
	var count int64
	require.NoError(t, db.Table("migration_columns").Where("last IS NULL").Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.False(t, db.Migrator().HasTable("migration_columns__temp"))
}

func TestAlterColumnPreservesEscapedDefaultAndGeneratedTableConstraint(t *testing.T) {
	db := sqliteMigrationDB(t)
	require.NoError(t, db.Exec("CREATE TABLE migration_columns (first text DEFAULT 'a,b''c?',middle text,last text, derived text GENERATED ALWAYS AS (upper(first)) VIRTUAL, UNIQUE(middle,last))").Error)
	require.NoError(t, db.Exec("INSERT INTO migration_columns (middle,last) VALUES ('m','l')").Error)
	require.NoError(t, db.Migrator().AlterColumn(&migrationColumns{}, "Last"))
	require.NoError(t, db.Exec("INSERT INTO migration_columns (middle,last) VALUES ('m2','l2')").Error)
	var rows []struct {
		First   string
		Derived string
	}
	require.NoError(t, db.Table("migration_columns").Find(&rows).Error)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, "a,b'c?", row.First)
		require.Equal(t, "A,B'C?", row.Derived)
	}
	require.Error(t, db.Exec("INSERT INTO migration_columns (middle,last) VALUES ('m','l')").Error)
}

func TestAlterColumnRecognizesGeneratedConstraintsStructurally(t *testing.T) {
	for _, tc := range []struct {
		definition string
		generated  bool
	}{
		{"first text DEFAULT GENERATED", false},
		{"first text DEFAULT 'GENERATED'", false},
		{"first text COLLATE GENERATED", false},
		{"first text CONSTRAINT GENERATED CHECK(length(first)>0)", false},
		{"first text REFERENCES GENERATED(id)", false},
		{"first text CHECK(first <> 'AS')", false},
		{"derived text GENERATED ALWAYS AS (upper(first)) VIRTUAL", true},
		{"derived text AS (upper(first)) STORED", true},
	} {
		t.Run(tc.definition, func(t *testing.T) {
			tokens, err := ddlTokens(tc.definition)
			require.NoError(t, err)
			_, generated, err := columnConstraints(tc.definition, tokens)
			require.NoError(t, err)
			require.Equal(t, tc.generated, generated)
		})
	}
}

func TestAlterColumnPreservesDefaultGeneratedKeywordValue(t *testing.T) {
	assertDefaultGeneratedKeywordMigration(t, sqliteMigrationDB(t))
}

func assertDefaultGeneratedKeywordMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec("CREATE TABLE migration_columns (first text DEFAULT GENERATED,middle text CONSTRAINT GENERATED CHECK(length(middle)>0),last text,derived text AS (upper(first)))").Error)
	require.NoError(t, db.Exec("INSERT INTO migration_columns (first,middle,last) VALUES ('keep','middle','last')").Error)
	require.NoError(t, db.Migrator().AlterColumn(&migrationColumns{}, "Last"))
	require.NoError(t, db.Exec("INSERT INTO migration_columns (middle,last) VALUES ('next','last2')").Error)
	var rows []struct{ First, Middle, Last, Derived string }
	require.NoError(t, db.Table("migration_columns").Order("rowid").Find(&rows).Error)
	require.Len(t, rows, 2)
	require.Equal(t, "keep", rows[0].First)
	require.Equal(t, "middle", rows[0].Middle)
	require.Equal(t, "last", rows[0].Last)
	require.Equal(t, "KEEP", rows[0].Derived)
	require.Equal(t, "GENERATED", rows[1].First)
	require.Equal(t, "GENERATED", rows[1].Derived)
}

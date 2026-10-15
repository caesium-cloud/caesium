package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func openMigrationTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared&_busy_timeout=5000"
	conn, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := conn.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return conn
}

func indexDDL(t *testing.T, conn *gorm.DB) (string, bool) {
	t.Helper()
	def, found, err := taskRunIndexDefinition(conn)
	require.NoError(t, err)
	return def, found
}

// TestMigrateTaskRunUniquePartitionIndexUpgradesLegacyIndex reproduces the
// upgrade an EXISTING deployment goes through: a task_runs table already
// carrying the pre-fan-out non-unique (job_run_id, task_id) index under the name
// idx_taskrun_jobrun_task. AutoMigrate would see that name and leave it alone
// forever, so the unique (job_run_id, task_id, partition_index) constraint would
// never be created and the core fan-out invariant would go unenforced in
// production while passing on every fresh CI database.
func TestMigrateTaskRunUniquePartitionIndexUpgradesLegacyIndex(t *testing.T) {
	conn := openMigrationTestDB(t)

	// Build the table WITHOUT the index, then create the legacy index by hand,
	// exactly as the pre-fan-out struct tags would have.
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))
	require.NoError(t, conn.Exec("DROP INDEX IF EXISTS idx_taskrun_jobrun_task").Error)
	require.NoError(t, conn.Exec(
		"CREATE INDEX idx_taskrun_jobrun_task ON task_runs(job_run_id, task_id)").Error)

	legacy, found := indexDDL(t, conn)
	require.True(t, found)
	require.False(t, indexIsUniqueOverPartitionIndex(legacy), "fixture must start on the legacy shape")

	// AutoMigrate ALONE is not enough — this is the defect being fixed.
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))
	stillLegacy, _ := indexDDL(t, conn)
	require.False(t, indexIsUniqueOverPartitionIndex(stillLegacy),
		"AutoMigrate skips an index whose name already exists; without an explicit migration the legacy index survives")

	// The explicit migration + AutoMigrate does the upgrade.
	require.NoError(t, MigrateTaskRunUniquePartitionIndex(conn))
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))

	upgraded, found := indexDDL(t, conn)
	require.True(t, found)
	assert.True(t, indexIsUniqueOverPartitionIndex(upgraded),
		"index must now be UNIQUE over (job_run_id, task_id, partition_index); got %q", upgraded)

	assertPartitionUniquenessEnforced(t, conn)
}

func TestMigrateTaskRunUniquePartitionIndexIsIdempotent(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))

	before, found := indexDDL(t, conn)
	require.True(t, found)
	require.True(t, indexIsUniqueOverPartitionIndex(before), "a fresh AutoMigrate already produces the new shape")

	for range 3 {
		require.NoError(t, MigrateTaskRunUniquePartitionIndex(conn))
		require.NoError(t, conn.AutoMigrate(&models.TaskRun{}))
	}

	after, found := indexDDL(t, conn)
	require.True(t, found)
	assert.Equal(t, before, after, "a correct index must be left completely alone")
	assertPartitionUniquenessEnforced(t, conn)
}

func TestMigrateTaskRunUniquePartitionIndexNoTableIsNoOp(t *testing.T) {
	conn := openMigrationTestDB(t)
	// No task_runs table at all — a brand new database. Nothing to inspect and
	// nothing to drop; AutoMigrate will create the correct index.
	require.NoError(t, MigrateTaskRunUniquePartitionIndex(conn))
	_, found := indexDDL(t, conn)
	assert.False(t, found)
}

// assertPartitionUniquenessEnforced proves the migrated index actually
// constrains the data, not just that its DDL string looks right: two instances
// of one (run, task) at DIFFERENT partition indexes are allowed, and a duplicate
// partition index is rejected.
func assertPartitionUniquenessEnforced(t *testing.T, conn *gorm.DB) {
	t.Helper()
	runID := uuid.New()
	taskID := uuid.New()
	newRow := func(index int) *models.TaskRun {
		return &models.TaskRun{
			ID:             uuid.New(),
			JobRunID:       runID,
			TaskID:         taskID,
			AtomID:         uuid.New(),
			Engine:         models.AtomEngineDocker,
			Image:          "alpine:3.23",
			Command:        `["echo","hi"]`,
			Status:         "pending",
			PartitionIndex: index,
			PartitionCount: 2,
		}
	}
	require.NoError(t, conn.Create(newRow(0)).Error, "two siblings at distinct partition indexes must be allowed")
	require.NoError(t, conn.Create(newRow(1)).Error)

	err := conn.Create(newRow(1)).Error
	require.Error(t, err, "a duplicate (job_run_id, task_id, partition_index) must be rejected by the unique index")
	assert.Contains(t, err.Error(), "UNIQUE")
}

// connectorCatalogTables are the execution-connector catalog. They are not hot
// execution tables.
var connectorCatalogTables = []any{
	&models.ExternalExecution{},
	&models.ExternalExecutionSnapshot{},
	&models.ExternalExecutionRelation{},
	&models.ConnectorOperation{},
	&models.ConnectorConfiguration{},
}

func TestConnectorCatalogRoutesToCatalogOnly(t *testing.T) {
	catalog := openMigrationTestDB(t)
	hot1 := openMigrationTestDB(t)
	hot2 := openMigrationTestDB(t)
	cold := openMigrationTestDB(t)
	router, err := NewRouter(catalog, []*gorm.DB{hot1, hot2}, cold)
	require.NoError(t, err)
	require.Greater(t, router.ShardCount(), 1)

	// Same split as Migrate: every model on the catalog, hot-path models on
	// each hot shard and on cold.
	require.NoError(t, migrateModels(router.Catalog(), models.All...))
	for _, shard := range router.HotShards() {
		require.NoError(t, migrateModels(shard, hotPathModels()...))
	}
	require.NoError(t, migrateModels(router.Cold(), hotPathModels()...))

	hotNames := map[string]struct{}{}
	for _, model := range hotPathModels() {
		hotNames[modelTable(t, catalog, model)] = struct{}{}
	}
	for _, model := range connectorCatalogTables {
		table := modelTable(t, catalog, model)
		require.Truef(t, catalog.Migrator().HasTable(table), "catalog missing %s", table)
		require.Falsef(t, hot1.Migrator().HasTable(table), "hot shard has %s", table)
		require.Falsef(t, hot2.Migrator().HasTable(table), "hot shard has %s", table)
		require.Falsef(t, cold.Migrator().HasTable(table), "cold database has %s", table)

		conn, role, shard, err := router.RouteTable(table, uuid.Nil)
		require.NoError(t, err)
		require.Equal(t, DatabaseRoleCatalog, role)
		require.Equal(t, -1, shard)
		require.Same(t, catalog, conn)

		_, onHotModel := hotNames[table]
		require.Falsef(t, onHotModel, "hotPathModels includes %s", table)
		_, onHotRoute := hotTables[table]
		require.Falsef(t, onHotRoute, "hotTables includes %s", table)
	}
}

func TestConnectorCatalogUpgradeKeepsJobs(t *testing.T) {
	catalog := openMigrationTestDB(t)
	require.NoError(t, catalog.AutoMigrate(&models.Trigger{}, &models.Job{}))
	now := time.Now().UTC()
	triggerID := uuid.New()
	jobID := uuid.New()
	require.NoError(t, catalog.Create(&models.Trigger{
		ID:        triggerID,
		Alias:     "upgrade-trigger",
		Type:      models.TriggerTypeCron,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, catalog.Create(&models.Job{
		ID:        jobID,
		Alias:     "upgrade-job",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	require.NoError(t, migrateModels(catalog, models.All...))
	for _, model := range connectorCatalogTables {
		table := modelTable(t, catalog, model)
		require.Truef(t, catalog.Migrator().HasTable(table), "upgraded catalog missing %s", table)
	}
	var jobs int64
	require.NoError(t, catalog.Model(&models.Job{}).Where("id = ?", jobID).Count(&jobs).Error)
	require.Equal(t, int64(1), jobs)
	var got models.Job
	require.NoError(t, catalog.First(&got, "id = ?", jobID).Error)
	require.Equal(t, "upgrade-job", got.Alias)
	require.True(t, catalog.Migrator().HasTable("jobs"))
}

func modelTable(t *testing.T, db *gorm.DB, model any) string {
	t.Helper()
	stmt := &gorm.Statement{DB: db}
	require.NoError(t, stmt.Parse(model))
	return stmt.Schema.Table
}

// This is the persisted incident layout from before namespaces became required.
type legacyNamespaceIncident struct {
	ID              uuid.UUID `gorm:"type:uuid;primaryKey"`
	Namespace       *string   `gorm:"type:text;index"`
	JobID           uuid.UUID `gorm:"type:uuid;not null"`
	TaskName        string
	Class           string                `gorm:"not null"`
	Status          models.IncidentStatus `gorm:"not null"`
	DedupeKey       string                `gorm:"not null"`
	ActiveDedupeKey *string               `gorm:"uniqueIndex:idx_incidents_active_dedupe"`
	OpenedAt        time.Time             `gorm:"not null"`
	CreatedAt       time.Time             `gorm:"not null"`
	UpdatedAt       time.Time             `gorm:"not null"`
}

func (legacyNamespaceIncident) TableName() string { return "incidents" }

func TestNamespaceMigrationRepairsLegacyNullsAndPreservesHistory(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&legacyNamespaceIncident{}))
	now := time.Now().UTC().Truncate(time.Second)
	empty, marketing := "", "marketing"
	namespaces := []*string{nil, &empty, &marketing}
	ids := make([]uuid.UUID, len(namespaces))
	for i, namespace := range namespaces {
		jobID := uuid.New()
		key := fmt.Sprintf("%s|extract|unknown", jobID)
		ids[i] = uuid.New()
		row := legacyNamespaceIncident{ID: ids[i], Namespace: namespace, JobID: jobID, TaskName: "extract", Class: "unknown", Status: models.IncidentStatusOpen, DedupeKey: key, ActiveDedupeKey: &key, OpenedAt: now, CreatedAt: now, UpdatedAt: now}
		if i == 1 {
			row.Status = models.IncidentStatusClosed
			row.ActiveDedupeKey = nil
		}
		require.NoError(t, conn.Create(&row).Error)
	}
	for range 2 {
		require.NoError(t, MigrateNamespaceDefaults(conn))
		require.NoError(t, migrateModels(conn, &models.Incident{}))
	}
	for i, id := range ids {
		var row models.Incident
		require.NoError(t, conn.First(&row, "id = ?", id).Error)
		want := "default"
		if i == 2 {
			want = "marketing"
		}
		require.Equal(t, want, row.Namespace)
		require.Equal(t, fmt.Sprintf("%s|%s|extract|unknown", want, row.JobID), row.DedupeKey)
		if i == 1 {
			require.Nil(t, row.ActiveDedupeKey)
		} else {
			require.Equal(t, row.DedupeKey, *row.ActiveDedupeKey)
		}
		require.True(t, now.Equal(row.OpenedAt), "history's original timestamp must not move")
	}
	require.True(t, conn.Migrator().HasIndex(&models.Incident{}, "idx_incidents_namespace"))
	require.Error(t, conn.Exec("UPDATE incidents SET namespace = NULL WHERE id = ?", ids[0]).Error, "NULL must be rejected after the upgrade")
	columns, err := conn.Migrator().ColumnTypes(&models.Incident{})
	require.NoError(t, err)
	for _, column := range columns {
		if column.Name() == "namespace" {
			nullable, ok := column.Nullable()
			require.True(t, ok)
			require.False(t, nullable)
			value, ok := column.DefaultValue()
			require.True(t, ok)
			require.Equal(t, "default", value)
		}
	}
}

func TestNamespaceMigrationNormalizesOnlyUnsetOwnershipAcrossTables(t *testing.T) {
	conn := openMigrationTestDB(t)
	for _, table := range []string{"jobs", "job_runs", "backfills"} {
		require.NoError(t, conn.Exec("CREATE TABLE "+table+" (id TEXT PRIMARY KEY, namespace TEXT)").Error)
		require.NoError(t, conn.Exec("INSERT INTO "+table+" (id, namespace) VALUES ('null', NULL), ('empty', ''), ('owned', 'finance')").Error)
	}
	require.NoError(t, MigrateNamespaceDefaults(conn))
	for _, table := range []string{"jobs", "job_runs", "backfills"} {
		var namespaces []string
		require.NoError(t, conn.Table(table).Order("id").Pluck("namespace", &namespaces).Error)
		require.Equal(t, []string{"default", "default", "finance"}, namespaces)
	}
	require.NoError(t, MigrateNamespaceDefaults(conn))
}

func TestNamespaceMigrationFreshDatabaseIsNoOp(t *testing.T) {
	require.NoError(t, MigrateNamespaceDefaults(openMigrationTestDB(t)))
}

func TestNamespaceMigrationFailsClosedOnActiveDedupeCollision(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&legacyNamespaceIncident{}))
	now := time.Now().UTC().Truncate(time.Second)
	jobID := uuid.New()
	legacyKey := fmt.Sprintf("%s|extract|unknown", jobID)
	namespacedKey := "default|" + legacyKey
	defaultNamespace := "default"
	rows := []legacyNamespaceIncident{
		{ID: uuid.New(), JobID: jobID, TaskName: "extract", Class: "unknown", Status: models.IncidentStatusOpen, DedupeKey: legacyKey, ActiveDedupeKey: &legacyKey, OpenedAt: now, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), Namespace: &defaultNamespace, JobID: jobID, TaskName: "extract", Class: "unknown", Status: models.IncidentStatusOpen, DedupeKey: namespacedKey, ActiveDedupeKey: &namespacedKey, OpenedAt: now, CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, conn.Create(&rows).Error)
	var before []legacyNamespaceIncident
	require.NoError(t, conn.Order("id").Find(&before).Error)
	for range 2 {
		err := MigrateNamespaceDefaults(conn)
		require.ErrorContains(t, err, "namespace migration active dedupe key collision")
		require.ErrorContains(t, err, namespacedKey)
		for _, row := range rows {
			require.ErrorContains(t, err, row.ID.String())
		}
		var after []legacyNamespaceIncident
		require.NoError(t, conn.Order("id").Find(&after).Error)
		require.Equal(t, before, after, "failed migration must roll back namespace repair and retain both active histories")
	}
}

// The persisted agent_profiles layout from before shared objects carried a
// required tenancy namespace (the column existed, nullable and never written).
type legacyNamespaceAgentProfile struct {
	ID        uuid.UUID      `gorm:"type:uuid;primaryKey"`
	Namespace *string        `gorm:"type:text;index"`
	Name      string         `gorm:"uniqueIndex;not null"`
	Image     string         `gorm:"type:text;not null"`
	Engine    string         `gorm:"type:text;not null;default:'docker'"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
	CreatedAt time.Time      `gorm:"not null"`
	UpdatedAt time.Time      `gorm:"not null"`
}

func (legacyNamespaceAgentProfile) TableName() string { return "agent_profiles" }

// createLegacySharedNotificationTables creates the pre-upgrade notification
// tables, which have no namespace column at all.
func createLegacySharedNotificationTables(t *testing.T, conn *gorm.DB) {
	t.Helper()
	for _, ddl := range []string{
		"CREATE TABLE notification_channels (id uuid PRIMARY KEY, name text NOT NULL, type text NOT NULL, config json NOT NULL, enabled numeric NOT NULL DEFAULT true, deleted_at datetime, created_at datetime NOT NULL, updated_at datetime NOT NULL)",
		"CREATE UNIQUE INDEX idx_notification_channels_name ON notification_channels(name)",
		"CREATE INDEX idx_notification_channels_type ON notification_channels(type)",
		"CREATE INDEX idx_notification_channels_deleted_at ON notification_channels(deleted_at)",
		"CREATE TABLE notification_policies (id uuid PRIMARY KEY, name text NOT NULL, channel_id uuid NOT NULL, event_types json NOT NULL, filters json, enabled numeric NOT NULL DEFAULT true, deleted_at datetime, created_at datetime NOT NULL, updated_at datetime NOT NULL, CONSTRAINT fk_notification_policies_channel FOREIGN KEY (channel_id) REFERENCES notification_channels(id) ON DELETE CASCADE)",
		"CREATE UNIQUE INDEX idx_notification_policies_name ON notification_policies(name)",
		"CREATE INDEX idx_notification_policies_channel_id ON notification_policies(channel_id)",
		"CREATE INDEX idx_notification_policies_deleted_at ON notification_policies(deleted_at)",
	} {
		require.NoError(t, conn.Exec(ddl).Error)
	}
}

func TestNamespaceMigrationTightensSharedObjects(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&legacyNamespaceAgentProfile{}))
	createLegacySharedNotificationTables(t, conn)

	now := time.Now().UTC().Truncate(time.Second)
	empty, finance := "", "finance"
	profiles := []legacyNamespaceAgentProfile{
		{ID: uuid.New(), Name: "legacy-null", Image: "img:1", Engine: "docker", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), Name: "legacy-empty", Namespace: &empty, Image: "img:1", Engine: "docker", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), Name: "legacy-finance", Namespace: &finance, Image: "img:1", Engine: "docker", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), Name: models.DefaultTriageOnlyProfileName, Image: "img:1", Engine: "docker", CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), Name: "legacy-deleted", Image: "img:1", Engine: "docker", DeletedAt: gorm.DeletedAt{Time: now, Valid: true}, CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, conn.Create(&profiles).Error)
	channelID, policyID := uuid.New(), uuid.New()
	require.NoError(t, conn.Exec("INSERT INTO notification_channels (id, name, type, config, enabled, created_at, updated_at) VALUES (?, 'legacy-channel', 'webhook', '{}', true, ?, ?)", channelID, now, now).Error)
	require.NoError(t, conn.Exec(`INSERT INTO notification_policies (id, name, channel_id, event_types, enabled, created_at, updated_at) VALUES (?, 'legacy-policy', ?, '["run_failed"]', true, ?, ?)`, policyID, channelID, now, now).Error)

	for range 2 {
		require.NoError(t, MigrateNamespaceDefaults(conn))
		require.NoError(t, migrateModels(conn, &models.NotificationChannel{}, &models.NotificationPolicy{}, &models.AgentProfile{}))
	}

	want := map[string]string{
		"legacy-null":    models.DefaultNamespace,
		"legacy-empty":   models.DefaultNamespace,
		"legacy-finance": "finance",
		// The built-in default moves to the cluster-shared namespace so every
		// job namespace keeps resolving it (design §10.4).
		models.DefaultTriageOnlyProfileName: models.ClusterNamespace,
		"legacy-deleted":                    models.DefaultNamespace,
	}
	var stored []models.AgentProfile
	require.NoError(t, conn.Unscoped().Find(&stored).Error)
	require.Len(t, stored, len(want))
	for _, profile := range stored {
		require.Equal(t, want[profile.Name], profile.Namespace, "profile %s", profile.Name)
	}
	var channel models.NotificationChannel
	require.NoError(t, conn.First(&channel, "id = ?", channelID).Error)
	require.Equal(t, models.DefaultNamespace, channel.Namespace)
	var policy models.NotificationPolicy
	require.NoError(t, conn.First(&policy, "id = ?", policyID).Error)
	require.Equal(t, models.DefaultNamespace, policy.Namespace)
	require.Equal(t, channelID, policy.ChannelID)

	for _, model := range []any{&models.NotificationChannel{}, &models.NotificationPolicy{}, &models.AgentProfile{}} {
		table := modelTable(t, conn, model)
		require.True(t, conn.Migrator().HasIndex(model, "idx_"+table+"_namespace"), table)
		require.Error(t, conn.Exec("UPDATE "+table+" SET namespace = NULL").Error, "%s must reject NULL after the upgrade", table)
		columns, err := conn.Migrator().ColumnTypes(model)
		require.NoError(t, err)
		var seen bool
		for _, column := range columns {
			if column.Name() != "namespace" {
				continue
			}
			seen = true
			nullable, ok := column.Nullable()
			require.True(t, ok)
			require.False(t, nullable, table)
			value, ok := column.DefaultValue()
			require.True(t, ok)
			require.Equal(t, models.DefaultNamespace, value, table)
		}
		require.True(t, seen, "%s.namespace", table)
	}
	require.Error(t, conn.Exec("INSERT INTO notification_channels (id, namespace, name, type, config, enabled, created_at, updated_at) VALUES (?, 'finance', 'legacy-channel', 'webhook', '{}', true, ?, ?)", uuid.New(), now, now).Error, "names stay globally unique")
	require.Error(t, conn.Exec("INSERT INTO agent_profiles (id, namespace, name, image, engine, created_at, updated_at) VALUES (?, 'finance', 'legacy-null', 'img', 'docker', ?, ?)", uuid.New(), now, now).Error, "names stay globally unique")
}

// An explicitly owned triage-only row is retained: only unset ownership is
// moved to the shared namespace.
func TestNamespaceMigrationKeepsExplicitTriageOnlyNamespace(t *testing.T) {
	conn := openMigrationTestDB(t)
	require.NoError(t, conn.AutoMigrate(&legacyNamespaceAgentProfile{}))
	finance := "finance"
	now := time.Now().UTC()
	require.NoError(t, conn.Create(&legacyNamespaceAgentProfile{ID: uuid.New(), Name: models.DefaultTriageOnlyProfileName, Namespace: &finance, Image: "img", Engine: "docker", CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, MigrateNamespaceDefaults(conn))
	var namespaces []string
	require.NoError(t, conn.Table("agent_profiles").Where("name = ?", models.DefaultTriageOnlyProfileName).Pluck("namespace", &namespaces).Error)
	require.Equal(t, []string{"finance"}, namespaces)
}

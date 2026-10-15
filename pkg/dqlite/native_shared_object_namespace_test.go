package dqlite

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The persisted shared-object layouts from before tenancy namespaces became
// required: agent_profiles.namespace is nullable, notification channels and
// policies have no namespace column. AutoMigrating these structs produces the
// same DDL (and index/constraint names) a pre-upgrade server persisted.
type legacySharedNSProfile struct {
	ID         uuid.UUID      `gorm:"type:uuid;primaryKey"`
	Namespace  *string        `gorm:"type:text;index"`
	Name       string         `gorm:"uniqueIndex;not null"`
	Image      string         `gorm:"type:text;not null"`
	Engine     string         `gorm:"type:text;not null;default:'docker'"`
	Limits     datatypes.JSON `gorm:"type:json"`
	SecretRefs datatypes.JSON `gorm:"type:json"`
	Budgets    datatypes.JSON `gorm:"type:json"`
	Playbook   datatypes.JSON `gorm:"type:json"`
	DeletedAt  gorm.DeletedAt `gorm:"index"`
	CreatedAt  time.Time      `gorm:"not null"`
	UpdatedAt  time.Time      `gorm:"not null"`
}

func (legacySharedNSProfile) TableName() string { return "agent_profiles" }

type legacySharedNSChannel struct {
	ID        uuid.UUID      `gorm:"type:uuid;primaryKey"`
	Name      string         `gorm:"uniqueIndex;not null"`
	Type      string         `gorm:"type:text;not null;index"`
	Config    datatypes.JSON `gorm:"type:json;not null"`
	Enabled   bool           `gorm:"not null;default:true"`
	DeletedAt gorm.DeletedAt `gorm:"index"`
	CreatedAt time.Time      `gorm:"not null"`
	UpdatedAt time.Time      `gorm:"not null"`
}

func (legacySharedNSChannel) TableName() string { return "notification_channels" }

type legacySharedNSPolicy struct {
	ID         uuid.UUID             `gorm:"type:uuid;primaryKey"`
	Name       string                `gorm:"uniqueIndex;not null"`
	ChannelID  uuid.UUID             `gorm:"type:uuid;index;not null"`
	Channel    legacySharedNSChannel `gorm:"constraint:OnDelete:CASCADE"`
	EventTypes datatypes.JSON        `gorm:"type:json;not null"`
	Filters    datatypes.JSON        `gorm:"type:json"`
	Enabled    bool                  `gorm:"not null;default:true"`
	DeletedAt  gorm.DeletedAt        `gorm:"index"`
	CreatedAt  time.Time             `gorm:"not null"`
	UpdatedAt  time.Time             `gorm:"not null"`
}

func (legacySharedNSPolicy) TableName() string { return "notification_policies" }

// TestNativeSharedObjectNamespaceUpgradeKeepsChildRows upgrades populated
// legacy shared-object tables on the actual native app. Tightening
// agent_profiles.namespace (and GORM's default-value branch, which fires on
// every dqlite AutoMigrate because native ColumnTypes hide defaults) rebuilds
// the PARENT tables agent_profiles and notification_channels. The rebuild's
// DROP TABLE must not fire the children's FK actions: agent_sessions-style
// ON DELETE SET NULL references and notification_policies' ON DELETE CASCADE
// must survive, run after run.
func TestNativeSharedObjectNamespaceUpgradeKeepsChildRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	require.Nil(t, currentApp.Load(), "do not replace a foreign native app")
	app := startTunedNode(t, ctx, t.TempDir(), address, nil)
	currentApp.Store(app)
	t.Cleanup(func() {
		if currentApp.CompareAndSwap(app, nil) {
			require.NoError(t, app.Close())
		}
	})
	// The probe deliberately provokes constraint failures; keep GORM quiet.
	conn, err := gorm.Open(Open("shared_object_namespace_native"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	pool, err := conn.DB()
	require.NoError(t, err)
	// Production pins every internal-dqlite writer pool to one connection
	// (pkg/db installDqliteReadWriteSplit), so the foreign_keys PRAGMAs and the
	// rebuild transaction share it. pkg/dqlite cannot import pkg/db; copy that.
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	db := conn.WithContext(ctx)

	require.NoError(t, db.AutoMigrate(&legacySharedNSProfile{}, &legacySharedNSChannel{}, &legacySharedNSPolicy{}))
	require.NoError(t, db.Exec("CREATE TABLE shared_ns_profile_children (id INTEGER PRIMARY KEY, profile_id uuid REFERENCES agent_profiles(id) ON DELETE SET NULL)").Error)

	now := time.Now().UTC().Truncate(time.Second)
	empty, finance := "", "finance"
	profiles := []legacySharedNSProfile{
		{ID: uuid.New(), Name: "legacy-null", Image: "img:1"},
		{ID: uuid.New(), Name: "legacy-empty", Namespace: &empty, Image: "img:1"},
		{ID: uuid.New(), Name: "legacy-finance", Namespace: &finance, Image: "img:1"},
		{ID: uuid.New(), Name: models.DefaultTriageOnlyProfileName, Image: "img:1"},
	}
	for i := range profiles {
		profiles[i].Engine, profiles[i].CreatedAt, profiles[i].UpdatedAt = "docker", now, now
		require.NoError(t, db.Create(&profiles[i]).Error)
		require.NoError(t, db.Exec("INSERT INTO shared_ns_profile_children (id, profile_id) VALUES (?, ?)", i+1, profiles[i].ID).Error)
	}
	channels := []legacySharedNSChannel{
		{ID: uuid.New(), Name: "legacy-channel-a", Type: "webhook", Config: datatypes.JSON(`{}`), Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), Name: "legacy-channel-b", Type: "webhook", Config: datatypes.JSON(`{}`), Enabled: true, CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, db.Create(&channels).Error)
	policies := []legacySharedNSPolicy{
		{ID: uuid.New(), Name: "legacy-policy-a", ChannelID: channels[0].ID, EventTypes: datatypes.JSON(`["run_failed"]`), Enabled: true, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), Name: "legacy-policy-b", ChannelID: channels[1].ID, EventTypes: datatypes.JSON(`["run_failed"]`), Enabled: true, CreatedAt: now, UpdatedAt: now},
	}
	require.NoError(t, db.Omit("Channel").Create(&policies).Error)

	// Setter diagnostic. pkg/dqlite RunWithoutForeignKey discards the OFF
	// setter's error; record what native dqlite actually does with it so a
	// failure below can be reported with the evidence. The OFF read-back is
	// logged, not asserted.
	offErr := db.Exec("PRAGMA foreign_keys = OFF").Error
	var offRead int
	offReadErr := db.Raw("PRAGMA foreign_keys").Scan(&offRead).Error
	t.Logf("native dqlite PRAGMA foreign_keys = OFF: setter err=%v; read-back=%d (read err=%v)", offErr, offRead, offReadErr)
	require.NoError(t, db.Exec("PRAGMA foreign_keys = ON").Error)
	assertSharedNSForeignKeysOn(t, db)

	var (
		mu    sync.Mutex
		drops = map[string]int{}
	)
	require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register("shared-ns-probe:count-drops", func(tx *gorm.DB) {
		sql := tx.Statement.SQL.String()
		if !strings.HasPrefix(sql, "DROP TABLE ") {
			return
		}
		mu.Lock()
		drops[strings.Trim(strings.TrimPrefix(sql, "DROP TABLE "), "`\"")]++
		mu.Unlock()
	}))
	t.Cleanup(func() { _ = db.Callback().Raw().Remove("shared-ns-probe:count-drops") })

	for run := 1; run <= 2; run++ {
		// The inline equivalent of pkg/db.MigrateNamespaceDefaults (pkg/dqlite
		// cannot import pkg/db): the built-in triage-only profile moves to the
		// cluster-shared namespace, every other unset profile to default.
		require.NoError(t, db.Exec("UPDATE agent_profiles SET namespace = ? WHERE name = ? AND (namespace IS NULL OR namespace = '')", models.ClusterNamespace, models.DefaultTriageOnlyProfileName).Error)
		require.NoError(t, db.Exec("UPDATE agent_profiles SET namespace = ? WHERE namespace IS NULL OR namespace = ''", models.DefaultNamespace).Error)
		require.NoError(t, db.AutoMigrate(&models.NotificationChannel{}, &models.NotificationPolicy{}, &models.AgentProfile{}), "run %d", run)

		var orphaned, children int64
		require.NoError(t, db.Raw("SELECT count(*) FROM shared_ns_profile_children").Scan(&children).Error)
		require.NoError(t, db.Raw("SELECT count(*) FROM shared_ns_profile_children WHERE profile_id IS NULL").Scan(&orphaned).Error)
		require.Equal(t, int64(len(profiles)), children, "run %d: child rows must survive the agent_profiles rebuild", run)
		require.Zero(t, orphaned, "run %d: ON DELETE SET NULL fired during the agent_profiles rebuild", run)
		for i, profile := range profiles {
			var profileID string
			require.NoError(t, db.Raw("SELECT profile_id FROM shared_ns_profile_children WHERE id = ?", i+1).Scan(&profileID).Error)
			require.Equal(t, profile.ID.String(), profileID, "run %d", run)
		}
		var policyCount int64
		require.NoError(t, db.Raw("SELECT count(*) FROM notification_policies").Scan(&policyCount).Error)
		require.Equal(t, int64(len(policies)), policyCount, "run %d: ON DELETE CASCADE fired during the notification_channels rebuild", run)

		wantProfiles := map[string]string{
			"legacy-null":                       models.DefaultNamespace,
			"legacy-empty":                      models.DefaultNamespace,
			"legacy-finance":                    "finance",
			models.DefaultTriageOnlyProfileName: models.ClusterNamespace,
		}
		var stored []models.AgentProfile
		require.NoError(t, db.Find(&stored).Error)
		require.Len(t, stored, len(wantProfiles))
		for _, profile := range stored {
			require.Equal(t, wantProfiles[profile.Name], profile.Namespace, "run %d: profile %s", run, profile.Name)
		}
		var channelNS, policyNS []string
		require.NoError(t, db.Model(&models.NotificationChannel{}).Pluck("namespace", &channelNS).Error)
		require.NoError(t, db.Model(&models.NotificationPolicy{}).Pluck("namespace", &policyNS).Error)
		require.Equal(t, []string{models.DefaultNamespace, models.DefaultNamespace}, channelNS, "run %d", run)
		require.Equal(t, []string{models.DefaultNamespace, models.DefaultNamespace}, policyNS, "run %d", run)

		for _, table := range []string{"agent_profiles", "notification_channels", "notification_policies"} {
			require.ErrorContains(t, db.Exec("UPDATE "+table+" SET namespace = NULL").Error, "NOT NULL", "run %d: %s must reject NULL namespace", run, table)
			require.True(t, db.Migrator().HasIndex(table, "idx_"+table+"_namespace"), "run %d: %s namespace index", run, table)
		}
		require.ErrorContains(t, db.Exec("INSERT INTO agent_profiles (id, namespace, name, image, engine, created_at, updated_at) VALUES (?, 'default', 'legacy-null', 'img', 'docker', ?, ?)", uuid.New(), now, now).Error, "UNIQUE", "run %d: unique name index", run)
		assertSharedNSForeignKeysOn(t, db)
		require.ErrorContains(t, db.Exec("INSERT INTO shared_ns_profile_children (id, profile_id) VALUES (99, ?)", uuid.New()).Error, "FOREIGN KEY", "run %d: child FK must still reference agent_profiles", run)
		require.ErrorContains(t, db.Exec("INSERT INTO notification_policies (id, namespace, name, channel_id, event_types, enabled, created_at, updated_at) VALUES (?, 'default', 'orphan', ?, '[]', 1, ?, ?)", uuid.New(), uuid.New(), now, now).Error, "FOREIGN KEY", "run %d: policy FK must still reference notification_channels", run)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Logf("parent/child table rebuilds across two AutoMigrate runs: %v", drops)
	// The probe is only meaningful if the parents were actually rebuilt.
	require.Positive(t, drops["agent_profiles"], "agent_profiles must be rebuilt (namespace tightened)")
	require.Positive(t, drops["notification_channels"], "notification_channels rebuild (default-value branch) not exercised")
}

func assertSharedNSForeignKeysOn(t *testing.T, db *gorm.DB) {
	t.Helper()
	var enabled int
	require.NoError(t, db.Raw("PRAGMA foreign_keys").Scan(&enabled).Error)
	require.Equal(t, 1, enabled, "foreign keys must be on")
}

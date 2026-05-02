package db

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	_ "github.com/jackc/pgx/v4"
	"go.uber.org/zap/zapcore"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	defaultMaxOpenConns = 4
	defaultMaxIdleConns = 2

	catalogDatabaseName = "caesium"
	historyDatabaseName = "caesium_history"
	hotShardNameFormat  = "caesium_hot_%02d"
)

var (
	routerOnce    sync.Once
	defaultRouter *Router
	routerErr     error
)

func gormLogLevel() gormlogger.LogLevel {
	switch log.GetLevel() {
	case zapcore.DebugLevel:
		return gormlogger.Info // log all queries
	case zapcore.InfoLevel, zapcore.WarnLevel:
		return gormlogger.Warn // errors + slow queries
	default:
		return gormlogger.Error // errors only
	}
}

func Connection() *gorm.DB {
	return DefaultRouter().Catalog()
}

// DefaultRouter returns the process-wide database router.
func DefaultRouter() *Router {
	routerOnce.Do(func() {
		defaultRouter, routerErr = openRouterFromEnv()
		if routerErr != nil {
			log.Fatal("failed to connect to database", "error", routerErr)
		}
	})

	return defaultRouter
}

func openRouterFromEnv() (*Router, error) {
	vars := env.Variables()
	dbType := strings.ToLower(strings.TrimSpace(vars.DatabaseType))
	shardCount := vars.DatabaseShards
	if shardCount <= 0 {
		shardCount = 1
	}
	if shardCount > 1 && !isInternalDqlite(dbType) {
		return nil, fmt.Errorf("CAESIUM_DATABASE_SHARDS > 1 requires the internal dqlite database backend")
	}

	catalog, err := openConnection(catalogDatabaseName, true)
	if err != nil {
		return nil, err
	}

	hot := make([]*gorm.DB, shardCount)
	if shardCount == 1 {
		hot[0] = catalog
	} else {
		for idx := range hot {
			conn, err := openConnection(fmt.Sprintf(hotShardNameFormat, idx), false)
			if err != nil {
				return nil, err
			}
			hot[idx] = conn
		}
	}

	cold := catalog
	if shardCount > 1 {
		cold, err = openConnection(historyDatabaseName, false)
		if err != nil {
			return nil, err
		}
	}

	router, err := NewRouter(catalog, hot, cold)
	if err != nil {
		return nil, err
	}

	log.Info(
		"database router initialized",
		"type", vars.DatabaseType,
		"shards", router.ShardCount(),
	)
	return router, nil
}

func openConnection(databaseName string, enforceForeignKeys bool) (*gorm.DB, error) {
	vars := env.Variables()
	dbType := strings.ToLower(strings.TrimSpace(vars.DatabaseType))

	log.Info("establishing db connection", "type", vars.DatabaseType, "database", databaseName)

	cfg := &gorm.Config{
		Logger: NewLogger().LogMode(gormLogLevel()),
	}
	if !enforceForeignKeys {
		cfg.DisableForeignKeyConstraintWhenMigrating = true
	}

	var (
		conn *gorm.DB
		err  error
	)
	switch dbType {
	case "postgres":
		conn, err = gorm.Open(
			postgres.Open(vars.DatabaseDSN),
			cfg,
		)
	case "internal":
		fallthrough
	case dqlite.DriverName:
		fallthrough
	default:
		conn, err = gorm.Open(
			dqlite.Open(databaseName),
			cfg,
		)
	}
	if err != nil {
		return nil, err
	}

	if sqlDB, err := conn.DB(); err == nil {
		configureConnectionPool(sqlDB, vars.DatabaseMaxOpenConns, vars.DatabaseMaxIdleConns)
	}

	if isInternalDqlite(dbType) && enforceForeignKeys {
		// Enable foreign key enforcement for SQLite-based catalog databases.
		conn.Exec("PRAGMA foreign_keys = ON")
	}
	return conn, nil
}

func isInternalDqlite(dbType string) bool {
	switch strings.ToLower(strings.TrimSpace(dbType)) {
	case "", "internal", dqlite.DriverName:
		return true
	default:
		return false
	}
}

func configureConnectionPool(sqlDB *sql.DB, maxOpen, maxIdle int) {
	if sqlDB == nil {
		return
	}
	if maxOpen <= 0 {
		maxOpen = defaultMaxOpenConns
	}
	if maxIdle <= 0 {
		maxIdle = defaultMaxIdleConns
	}
	if maxIdle > maxOpen {
		maxIdle = maxOpen
	}

	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
}

func Migrate() (err error) {
	router := DefaultRouter()
	if err = migrateModels(router.Catalog(), models.All...); err != nil {
		return err
	}
	if router.ShardCount() > 1 {
		for _, shard := range router.HotShards() {
			if err = migrateModels(shard, hotPathModels()...); err != nil {
				return err
			}
		}
		if err = migrateModels(router.Cold(), hotPathModels()...); err != nil {
			return err
		}
	}

	var triggers []models.Trigger
	if err = router.Catalog().Where("type = ?", models.TriggerTypeHTTP).Find(&triggers).Error; err != nil {
		return err
	}
	for idx := range triggers {
		trigger := &triggers[idx]
		if err = trigger.ApplyDerivedFields(); err != nil {
			return err
		}
		if err = router.Catalog().
			Model(&models.Trigger{}).
			Where("id = ?", trigger.ID).
			Update("normalized_path", trigger.NormalizedPath).
			Error; err != nil {
			return err
		}
	}
	return nil
}

func migrateModels(conn *gorm.DB, models ...interface{}) error {
	for _, model := range models {
		if err := conn.AutoMigrate(model); err != nil {
			return err
		}
	}
	return nil
}

func hotPathModels() []interface{} {
	return []interface{}{
		&models.JobRun{},
		&models.TaskRun{},
		&models.CallbackRun{},
		&models.ExecutionEvent{},
	}
}

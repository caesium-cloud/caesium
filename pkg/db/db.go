package db

import (
	"database/sql"
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
)

var (
	once sync.Once
	gdb  *gorm.DB
	err  error
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
	once.Do(func() {
		dbType := env.Variables().DatabaseType

		log.Info("establishing db connection", "type", dbType)

		cfg := &gorm.Config{
			Logger: NewLogger().LogMode(gormLogLevel()),
		}

		switch dbType {
		case "postgres":
			gdb, err = gorm.Open(
				postgres.Open(env.Variables().DatabaseDSN),
				cfg,
			)
		case "internal":
			fallthrough
		case dqlite.DriverName:
			fallthrough
		default:
			gdb, err = gorm.Open(
				dqlite.Open(""),
				cfg,
			)
		}

		if err != nil {
			log.Fatal("failed to connect to database", "error", err)
		}

		if sqlDB, err := gdb.DB(); err == nil {
			configureConnectionPool(sqlDB, env.Variables().DatabaseMaxOpenConns, env.Variables().DatabaseMaxIdleConns)
		}

		if dbType == "internal" || dbType == dqlite.DriverName {
			// Enable foreign key enforcement for SQLite-based databases.
			gdb.Exec("PRAGMA foreign_keys = ON")
		}
	})

	return gdb
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
	for _, model := range models.All {
		if err = Connection().AutoMigrate(model); err != nil {
			return
		}
	}

	var triggers []models.Trigger
	if err = Connection().Where("type = ?", models.TriggerTypeHTTP).Find(&triggers).Error; err != nil {
		return
	}
	for idx := range triggers {
		trigger := &triggers[idx]
		if err = trigger.ApplyDerivedFields(); err != nil {
			return
		}
		if err = Connection().
			Model(&models.Trigger{}).
			Where("id = ?", trigger.ID).
			Update("normalized_path", trigger.NormalizedPath).
			Error; err != nil {
			return
		}
	}
	return
}

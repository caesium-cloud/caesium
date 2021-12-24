package db

import (
	"sync"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	_ "github.com/jackc/pgx/v4"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	once sync.Once
	gdb  *gorm.DB
	err  error
)

func Connection() *gorm.DB {
	once.Do(func() {
		dbType := env.Variables().DatabaseType

		log.Info("establishing db connection", "type", dbType)

		switch dbType {
		case "postgres":
			gdb, err = gorm.Open(
				postgres.Open(env.Variables().DatabaseDSN),
				&gorm.Config{},
			)
		case "internal":
			fallthrough
		case dqlite.DriverName:
			fallthrough
		default:
			gdb, err = gorm.Open(
				dqlite.Open(""),
				&gorm.Config{},
			)
		}

		if err != nil {
			log.Fatal("failed to connect to database", "error", err)
		}
	})

	return gdb
}

func Migrate() (err error) {
	for _, model := range models.All {
		if err = Connection().AutoMigrate(model); err != nil {
			return
		}
	}
	return
}

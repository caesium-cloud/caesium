package db

import (
	"sync"

	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	_ "github.com/jackc/pgx/v4"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var (
	gdb    *gorm.DB
	err    error
	dbType = env.Variables().DatabaseType
	once   sync.Once
)

func Connection() *gorm.DB {
	once.Do(func() {
		log.Info("establishing db connection", "type", dbType)

		switch dbType {
		case "postgres":
			gdb, err = gorm.Open(
				postgres.Open(env.Variables().DatabaseDSN),
				&gorm.Config{},
			)
		case "internal":
			fallthrough
		case "dqlite":
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

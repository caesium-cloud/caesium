package db

import (
	"log"

	"github.com/caesium-cloud/caesium/pkg/env"
	_ "github.com/jackc/pgx/v4"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func init() {
}
func Connection() *gorm.DB {
	var (
		gdb *gorm.DB
		err error
	)

	switch env.Variables().DatabaseType {
	case "postgres":
		gdb, err = gorm.Open(
			postgres.Open(env.Variables().DatabaseDSN),
			&gorm.Config{},
		)
	}

	if err != nil {
		log.Fatal("failed to connect to database", "error", err)
	}

	return gdb
}

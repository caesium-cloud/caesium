package query

import (
	"github.com/caesium-cloud/caesium/pkg/sqlite"
	"gorm.io/gorm"
)

func Session() *gorm.DB {
	db, err := gorm.Open(sqlite.Open("gorm.db"), &gorm.Config{})
	if err != nil {
		panic(err)
	}

	return db.Session(&gorm.Session{DryRun: true})
}

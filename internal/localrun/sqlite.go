package localrun

import (
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/caesium-cloud/caesium/pkg/db"
)

func openSQLite(dsn string) (*gorm.DB, error) {
	// Use the repo's zap-backed GORM logger: gorm.Config{}'s default logger
	// writes SQL traces (including routine record-not-found probes) straight
	// to os.Stdout, which corrupts machine-readable CLI output — `caesium
	// reproduce --json` and `caesium dev` both ride this runner.
	return gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: db.NewLogger()})
}

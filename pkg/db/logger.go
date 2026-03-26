package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const slowQueryThreshold = 200 * time.Millisecond

// zapLogger implements gorm's logger.Interface, routing all database
// logging through the application's structured Zap logger.
type zapLogger struct {
	level gormlogger.LogLevel
}

// NewLogger creates a GORM logger that delegates to pkg/log.
func NewLogger() gormlogger.Interface {
	return &zapLogger{level: gormlogger.Warn}
}

func (l *zapLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	return &zapLogger{level: level}
}

func (l *zapLogger) Info(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Info {
		log.Info(fmt.Sprintf(msg, data...), "source", "gorm")
	}
}

func (l *zapLogger) Warn(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Warn {
		log.Warn(fmt.Sprintf(msg, data...), "source", "gorm")
	}
}

func (l *zapLogger) Error(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Error {
		log.Error(fmt.Sprintf(msg, data...), "source", "gorm")
	}
}

func (l *zapLogger) Trace(_ context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	sql, rows := fc()

	fields := []interface{}{
		"source", "gorm",
		"duration", elapsed.String(),
		"rows", rows,
		"sql", sql,
	}

	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		log.Debug("record not found", fields...)
	case err != nil:
		log.Error("database error", append(fields, "error", err)...)
	case elapsed > slowQueryThreshold:
		log.Warn("slow query", fields...)
	default:
		if l.level >= gormlogger.Info {
			log.Debug("query", fields...)
		}
	}
}

package log

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func init() {
	atom := zap.NewAtomicLevelAt(zapcore.DebugLevel)

	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "timestamp"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder

	logger := zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.Lock(os.Stdout),
		atom,
	))

	zap.ReplaceGlobals(logger)
}

// Debug logs a debug message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Debug(msg string, args ...interface{}) {
	if logLevel <= DEBUG {
		if len(args) > 0 {
			zap.S().Debugf(msg, args...)
		} else {
			zap.S().Debug(msg)
		}
	}
}

// Info logs an info message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Info(msg string, args ...interface{}) {
	if logLevel <= INFO {
		if len(args) > 0 {
			zap.S().Infof(msg, args...)
		} else {
			zap.S().Info(msg)
		}
	}
}

// Warn logs a warning message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Warn(msg string, args ...interface{}) {
	if logLevel <= WARNING {
		if len(args) > 0 {
			zap.S().Warnf(msg, args...)
		} else {
			zap.S().Warn(msg)
		}
	}
}

// Error logs an error message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Error(msg string, args ...interface{}) {
	if logLevel <= ERROR {
		if len(args) > 0 {
			zap.S().Errorf(msg, args...)
		} else {
			zap.S().Error(msg)
		}
	}
}

// Fatal logs a fatal message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Fatal(msg string, args ...interface{}) {
	if len(args) > 0 {
		zap.S().Fatalf(msg, args...)
	} else {
		zap.S().Fatal(msg)
	}
}

// SetLevel sets the log level.
func SetLevel(level Level) {
	logLevel = level
}

// SetLevelFromString sets the log level by specifying
// a string which can be any of:
// ["DEBUG", "INFO", "WARNING", "ERROR", "FATAL"],
// case-insensitive.
func SetLevelFromString(level string) error {
	switch strings.ToUpper(level) {
	case "DEBUG":
		logLevel = DEBUG
	case "INFO":
		logLevel = INFO
	case "WARNING":
		logLevel = WARNING
	case "ERROR":
		logLevel = ERROR
	case "FATAL":
		logLevel = FATAL
	default:
		return fmt.Errorf("invalid log level string: %v", level)
	}

	return nil
}

// Level enumerates the supported log levels
type Level int

const (
	// DEBUG log level is the lowest log level and meant
	// to be used for diagnosing issues.
	DEBUG Level = iota
	// INFO is the standard log level and is the recommended
	// level to run Caesium with.
	INFO
	// WARNING log messages indicates something isn't ideal
	// in Caesium, but that it can carry on doing its job
	// anyways.
	WARNING
	// ERROR log messages indicate something is wrong and
	// it should be addressed and/or diagnosed. A stack
	// trace will be printed for ERROR messages.
	ERROR
	// FATAL log messages are only seen once per run of
	// Caesium because it will immediately exit after one
	// is logged. RIP.
	FATAL
)

var logLevel Level

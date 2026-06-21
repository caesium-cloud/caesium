package log

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	logLevel zap.AtomicLevel
	ring     *RingBuffer
)

const defaultRingSize = 2000

func init() {
	logLevel = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	ring = NewRingBuffer(defaultRingSize, zapcore.DebugLevel)
	// Default the structured logger to STDERR so that machine-readable command
	// output on stdout (e.g. `caesium receipt get` → a JSON receipt piped into
	// `caesium verify`) is never interleaved with log lines — including logs
	// emitted from package init() functions, which run before main(). The
	// server restores stdout in its start command (see ToStdout) before it
	// installs the stderr capture pipe.
	rebuildLogger(os.Stderr)
}

// newEncoder builds the encoder honoring CAESIUM_LOG_FORMAT (text|json).
func newEncoder() zapcore.Encoder {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CAESIUM_LOG_FORMAT"))) {
	case "text":
		return zapcore.NewConsoleEncoder(textConfig())
	default:
		return zapcore.NewJSONEncoder(jsonConfig())
	}
}

// rebuildLogger installs the global logger writing structured logs to ws,
// teed into the in-memory ring buffer. Safe to call repeatedly.
func rebuildLogger(ws *os.File) {
	core := zapcore.NewCore(newEncoder(), zapcore.Lock(ws), logLevel)
	logger := zap.New(
		zapcore.NewTee(core, ring),
		zap.AddCaller(),
		zap.AddCallerSkip(1), // skip the wrapper functions in this package
	)
	zap.ReplaceGlobals(logger)
}

// ToStderr routes structured logs to stderr. CLI commands call this so that
// stdout carries only the command's machine-readable output (e.g. a
// `caesium receipt get` JSON receipt that gets piped into `caesium verify`).
// The server does NOT use this — it logs to stdout and captures C-library
// stderr separately (see CaptureStderr), where routing logs to stderr would
// feed the capture pipe back into the logger.
func ToStderr() { rebuildLogger(os.Stderr) }

// ToStdout routes structured logs to stdout (the server default). Used to
// restore stdout logging before the server installs its stderr capture.
func ToStdout() { rebuildLogger(os.Stdout) }

func jsonConfig() zapcore.EncoderConfig {
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "ts"
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg
}

func textConfig() zapcore.EncoderConfig {
	cfg := zap.NewDevelopmentEncoderConfig()
	cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	return cfg
}

// Buffer returns the global ring buffer for log streaming.
func Buffer() *RingBuffer {
	return ring
}

// Debug logs a debug message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Debug(msg string, kv ...interface{}) {
	zap.S().Debugw(msg, kv...)
}

// Info logs an info message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Info(msg string, kv ...interface{}) {
	zap.S().Infow(msg, kv...)
}

// Warn logs a warning message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Warn(msg string, kv ...interface{}) {
	zap.S().Warnw(msg, kv...)
}

// Error logs an error message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Error(msg string, kv ...interface{}) {
	zap.S().Errorw(msg, kv...)
}

// Panic logs a panic message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Panic(msg string, kv ...interface{}) {
	zap.S().Panicw(msg, kv...)
}

// Fatal logs a fatal message. Refer to:
// https://godoc.org/go.uber.org/zap
// for more details.
func Fatal(msg string, kv ...interface{}) {
	zap.S().Fatalw(msg, kv...)
}

// SetLevel sets the log level by specifying a string which
// can be any of:
// ["DEBUG", "INFO", "WARNING", "ERROR", "PANIC", "FATAL"],
// case-insensitive.
func SetLevel(level string) error {
	switch strings.ToUpper(level) {
	case "DEBUG":
		logLevel.SetLevel(zapcore.DebugLevel)
	case "INFO":
		logLevel.SetLevel(zapcore.InfoLevel)
	case "WARN":
		fallthrough
	case "WARNING":
		logLevel.SetLevel(zapcore.WarnLevel)
	case "ERROR":
		logLevel.SetLevel(zapcore.ErrorLevel)
	case "PANIC":
		logLevel.SetLevel(zapcore.PanicLevel)
	case "FATAL":
		logLevel.SetLevel(zapcore.FatalLevel)
	default:
		return fmt.Errorf("invalid log level string: %v", level)
	}

	return nil
}

// GetLevel returns the current log level.
func GetLevel() zapcore.Level {
	return logLevel.Level()
}

// Clean sanitizes a log message to be lower case and
// removes newline characters.
func Clean(msg string) string {
	return strings.ToLower(strings.ReplaceAll(msg, "\n", ""))
}

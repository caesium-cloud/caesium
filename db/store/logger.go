package store

import (
	"io"
	"log"

	clog "github.com/caesium-cloud/caesium/pkg/log"
	"github.com/hashicorp/go-hclog"
	"go.uber.org/zap/zapcore"
)

type RaftLogger struct {
	hclog.Logger
}

func (l *RaftLogger) Trace(msg string, args ...interface{}) {}

func (l *RaftLogger) Debug(msg string, args ...interface{}) {
	clog.Debug(msg, args...)
}

func (l *RaftLogger) Info(msg string, args ...interface{}) {
	clog.Info(msg, args...)
}

func (l *RaftLogger) Warn(msg string, args ...interface{}) {
	clog.Warn(msg, args...)
}

func (l *RaftLogger) Error(msg string, args ...interface{}) {
	clog.Error(msg, args...)
}

func (l *RaftLogger) IsTrace() bool {
	return false
}

func (l *RaftLogger) IsDebug() bool {
	return clog.GetLevel() == zapcore.DebugLevel
}

func (l *RaftLogger) IsInfo() bool {
	return clog.GetLevel() == zapcore.InfoLevel
}

func (l *RaftLogger) IsWarn() bool {
	return clog.GetLevel() == zapcore.WarnLevel
}

func (l *RaftLogger) IsError() bool {
	return clog.GetLevel() == zapcore.ErrorLevel
}

func (l *RaftLogger) With(args ...interface{}) hclog.Logger {
	return nil
}

func (l *RaftLogger) Named(name string) hclog.Logger {
	return nil
}

func (l *RaftLogger) ResetNamed(name string) hclog.Logger {
	return nil
}

func (l *RaftLogger) SetLevel(level hclog.Level) {}

func (l *RaftLogger) StandardLogger(opts *hclog.StandardLoggerOptions) *log.Logger {
	return nil
}

func (l *RaftLogger) StandardWriter(opts *hclog.StandardLoggerOptions) io.Writer {
	return nil
}

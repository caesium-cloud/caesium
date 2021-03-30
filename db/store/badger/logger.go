package badger

import (
	"fmt"

	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/dgraph-io/badger/v3"
)

type BadgerLogger struct {
	badger.Logger
}

func (l *BadgerLogger) Debugf(msg string, args ...interface{}) {
	log.Debug(log.Clean(fmt.Sprintf(msg, args...)))
}

func (l *BadgerLogger) Infof(msg string, args ...interface{}) {
	log.Info(log.Clean(fmt.Sprintf(msg, args...)))
}

func (l *BadgerLogger) Warningf(msg string, args ...interface{}) {
	log.Warn(log.Clean(fmt.Sprintf(msg, args...)))
}

func (l *BadgerLogger) Errorf(msg string, args ...interface{}) {
	log.Error(log.Clean(fmt.Sprintf(msg, args...)))
}

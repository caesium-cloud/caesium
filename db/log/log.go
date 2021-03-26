package log

import (
	"github.com/caesium-cloud/caesium/db/store/badger"
	"github.com/hashicorp/raft"
	"github.com/pkg/errors"
)

// Log is an object that can return information about the Raft log.
type Log struct {
	*badger.BadgerStore
}

// NewLog returns an instantiated Log object.
func NewLog(path string) (*Log, error) {
	bs, err := badger.NewBadgerStore(path)
	if err != nil {
		return nil, errors.Wrap(err, "new badger store")
	}
	return &Log{bs}, nil
}

// Indexes returns the first and last indexes.
func (l *Log) Indexes() (uint64, uint64, error) {
	fi, err := l.FirstIndex()
	if err != nil {
		return 0, 0, errors.Wrap(err, "failed to get first index")
	}
	li, err := l.LastIndex()
	if err != nil {
		return 0, 0, errors.Wrap(err, "failed to get last index")
	}
	return fi, li, nil
}

// LastCommandIndex returns the index of the last Command
// log entry written to the Raft log. Returns an index of
// zero if no such log exists.
func (l *Log) LastCommandIndex() (uint64, error) {
	fi, li, err := l.Indexes()
	if err != nil {
		return 0, errors.Wrap(err, "get indexes")
	}

	// Check for empty log.
	if li == 0 {
		return 0, nil
	}

	var rl raft.Log
	for i := li; i >= fi; i-- {
		if err := l.GetLog(i, &rl); err != nil {
			return 0, errors.Wrapf(err, "get log at index %d", i)
		}
		if rl.Type == raft.LogCommand {
			return i, nil
		}
	}
	return 0, nil
}

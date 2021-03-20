package badger

import (
	"errors"
	"time"

	"github.com/caesium-dev/caesium/pkg/bytes"
	"github.com/dgraph-io/badger/v3"
	"github.com/hashicorp/raft"
	"github.com/vmihailenco/msgpack/v5"
)

var (
	// Prefix names to distingish between logs and conf
	prefixLogs = []byte{0x0}
	prefixConf = []byte{0x1}

	// ErrKeyNotFound is an error indicating a given key does not exist
	ErrKeyNotFound = errors.New("not found")
)

// BadgerStore provides access to Badger for Raft to store and retrieve
// log entries. It also provides key/value storage, and can be used as
// a LogStore and StableStore.
type BadgerStore struct {
	// conn is the underlying handle to the db.
	conn *badger.DB

	// The path to the Badger database directory.
	path string

	vlogTicker          *time.Ticker // runs every 1m, check size of vlog and run GC conditionally.
	mandatoryVlogTicker *time.Ticker // runs every 10m, we always run vlog GC.
}

// Options contains all the configuration used to open the Badger db
type Options struct {
	// Path is the directory path to the Badger db to use.
	Path string

	// BadgerOptions contains any specific Badger options you might
	// want to specify.
	BadgerOptions *badger.Options

	// NoSync causes the database to skip fsync calls after each
	// write to the log. This is unsafe, so it should be used
	// with caution.
	NoSync bool

	// ValueLogGC enables a periodic goroutine that does a garbage
	// collection of the value log while the underlying Badger is online.
	ValueLogGC bool

	// GCInterval is the interval between conditionally running the garbage
	// collection process, based on the size of the vlog. By default, runs every 1m.
	GCInterval time.Duration

	// GCInterval is the interval between mandatory running the garbage
	// collection process. By default, runs every 10m.
	MandatoryGCInterval time.Duration

	// GCThreshold sets threshold in bytes for the vlog size to be included in the
	// garbage collection cycle. By default, 1GB.
	GCThreshold int64
}

// NewBadgerStore takes a file path and returns a connected Raft backend.
func NewBadgerStore(path string) (*BadgerStore, error) {
	return New(Options{Path: path})
}

// func NewDefaultStableStore(path string) (*BadgerStore, error) {
// 	opts := badger.DefaultOptions
// 	opts.MaxLevels = 2
// 	return New(Options{Path: path, BadgerOptions: &opts})
// }

// New uses the supplied options to open the Badger db and prepare it for
// use as a raft backend.
func New(options Options) (*BadgerStore, error) {

	// build badger options
	if options.BadgerOptions == nil {
		defaultOpts := badger.DefaultOptions(options.Path)
		options.BadgerOptions = &defaultOpts
	}
	options.BadgerOptions.SyncWrites = !options.NoSync

	// Try to connect
	handle, err := badger.Open(*options.BadgerOptions)
	if err != nil {
		return nil, err
	}

	// Create the new store
	store := &BadgerStore{
		conn: handle,
		path: options.Path,
	}

	// Start GC routine
	if options.ValueLogGC {

		var gcInterval time.Duration
		var mandatoryGCInterval time.Duration
		var threshold int64

		if gcInterval = 1 * time.Minute; options.GCInterval != 0 {
			gcInterval = options.GCInterval
		}
		if mandatoryGCInterval = 10 * time.Minute; options.MandatoryGCInterval != 0 {
			mandatoryGCInterval = options.MandatoryGCInterval
		}
		if threshold = int64(1 << 30); options.GCThreshold != 0 {
			threshold = options.GCThreshold
		}

		store.vlogTicker = time.NewTicker(gcInterval)
		store.mandatoryVlogTicker = time.NewTicker(mandatoryGCInterval)
		go store.runVlogGC(handle, threshold)
	}

	return store, nil
}

func (b *BadgerStore) runVlogGC(db *badger.DB, threshold int64) {
	// Get initial size on start.
	_, lastVlogSize := db.Size()

	runGC := func() {
		var err error
		for err == nil {
			// If a GC is successful, immediately run it again.
			err = db.RunValueLogGC(0.7)
		}
		_, lastVlogSize = db.Size()
	}

	for {
		select {
		case <-b.vlogTicker.C:
			_, currentVlogSize := db.Size()
			if currentVlogSize < lastVlogSize+threshold {
				continue
			}
			runGC()
		case <-b.mandatoryVlogTicker.C:
			runGC()
		}
	}
}

// Close is used to gracefully close the DB connection.
func (b *BadgerStore) Close() error {
	if b.vlogTicker != nil {
		b.vlogTicker.Stop()
	}
	if b.mandatoryVlogTicker != nil {
		b.mandatoryVlogTicker.Stop()
	}
	return b.conn.Close()
}

// FirstIndex returns the first known index from the Raft log.
func (b *BadgerStore) FirstIndex() (uint64, error) {
	var value uint64
	err := b.conn.View(func(tx *badger.Txn) error {
		it := tx.NewIterator(badger.IteratorOptions{
			PrefetchValues: false,
			Reverse:        false,
		})
		defer it.Close()

		it.Seek(prefixLogs)
		if it.ValidForPrefix(prefixLogs) {
			value = bytes.ToUint64(it.Item().Key()[1:])
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return value, nil
}

// LastIndex returns the last known index from the Raft log.
func (b *BadgerStore) LastIndex() (uint64, error) {
	var value uint64
	err := b.conn.View(func(tx *badger.Txn) error {
		it := tx.NewIterator(badger.IteratorOptions{
			PrefetchValues: false,
			Reverse:        true,
		})
		defer it.Close()

		it.Seek(append(prefixLogs, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff))
		if it.ValidForPrefix(prefixLogs) {
			value = bytes.ToUint64(it.Item().Key()[1:])
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return value, nil
}

// GetLog gets a log entry from Badger at a given index.
func (b *BadgerStore) GetLog(index uint64, log *raft.Log) error {
	return b.conn.View(func(tx *badger.Txn) error {
		item, err := tx.Get(append(prefixLogs, bytes.FromUint64(index)...))
		if err != nil {
			switch err {
			case badger.ErrKeyNotFound:
				return raft.ErrLogNotFound
			default:
				return err
			}
		}
		val, err := item.ValueCopy(nil)
		if err != nil {
			return err
		}
		return msgpack.Unmarshal(val, log)
	})
}

// StoreLog stores a single raft log.
func (b *BadgerStore) StoreLog(log *raft.Log) error {
	buf, err := msgpack.Marshal(log)
	if err != nil {
		return err
	}
	return b.conn.Update(func(tx *badger.Txn) error {
		return tx.Set(append(prefixLogs, bytes.FromUint64(log.Index)...), buf)
	})
}

// StoreLogs stores a set of raft logs.
func (b *BadgerStore) StoreLogs(logs []*raft.Log) error {
	// we manage the transaction manually in order to avoid ErrTxnTooBig errors
	tx := b.conn.NewTransaction(true)
	for i, log := range logs {
		key := append(prefixLogs, bytes.FromUint64(log.Index)...)
		buf, err := msgpack.Marshal(log)
		if err != nil {
			return err
		}
		if err := tx.Set(key, buf); err != nil {
			if err == badger.ErrTxnTooBig {
				err = tx.Commit()
				if err != nil {
					return err
				}
				return b.StoreLogs(logs[i:])
			}
			return err
		}
	}
	err := tx.Commit()
	if err != nil {
		return err
	}
	return nil
}

// DeleteRange deletes logs within a given range inclusively.
func (b *BadgerStore) DeleteRange(min, max uint64) error {
	// we manage the transaction manually in order to avoid ErrTxnTooBig errors
	tx := b.conn.NewTransaction(true)
	it := tx.NewIterator(badger.IteratorOptions{
		PrefetchValues: false,
		Reverse:        false,
	})

	start := append(prefixLogs, bytes.FromUint64(min)...)
	for it.Seek(start); it.Valid(); it.Next() {
		key := make([]byte, 9)
		it.Item().KeyCopy(key)
		// Handle out-of-range log index
		if bytes.ToUint64(key[1:]) > max {
			break
		}
		// Delete in-range log index
		if err := tx.Delete(key); err != nil {
			if err == badger.ErrTxnTooBig {
				it.Close()
				err = tx.Commit()
				if err != nil {
					return err
				}
				return b.DeleteRange(bytes.ToUint64(key[1:]), max)
			}
			return err
		}
	}
	it.Close()
	err := tx.Commit()
	if err != nil {
		return err
	}
	return nil
}

// Set is used to set a key/value set outside of the raft log.
func (b *BadgerStore) Set(key []byte, val []byte) error {
	return b.conn.Update(func(tx *badger.Txn) error {
		return tx.Set(append(prefixConf, key...), val)
	})
}

// Get is used to retrieve a value from the k/v store by key
func (b *BadgerStore) Get(key []byte) (value []byte, err error) {
	err = b.conn.View(func(tx *badger.Txn) error {
		item, err := tx.Get(append(prefixConf, key...))
		if err != nil {
			switch err {
			case badger.ErrKeyNotFound:
				return ErrKeyNotFound
			default:
				return err
			}
		}
		value, err = item.ValueCopy(value)
		if err != nil {
			return err
		}
		return nil
	})

	return
}

// SetUint64 is like Set, but handles uint64 values
func (b *BadgerStore) SetUint64(key []byte, val uint64) error {
	return b.Set(key, bytes.FromUint64(val))
}

// GetUint64 is like Get, but handles uint64 values
func (b *BadgerStore) GetUint64(key []byte) (uint64, error) {
	val, err := b.Get(key)
	if err != nil {
		return 0, err
	}
	return bytes.ToUint64(val), nil
}

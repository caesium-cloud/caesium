package badger

import (
	"bytes"
	"io/ioutil"
	"os"
	"reflect"
	"testing"

	"github.com/dgraph-io/badger/v3"
	"github.com/hashicorp/raft"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type BadgerTestSuite struct {
	suite.Suite
	path  string
	store *BadgerStore
}

func (s *BadgerTestSuite) SetupTest() {
	var err error
	s.path, err = ioutil.TempDir("", "raftbadger")
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), os.RemoveAll(s.path))

	badgerOpts := badger.DefaultOptions(s.path).WithLogger(nil)
	s.store, err = New(Options{
		Path:          s.path,
		NoSync:        true,
		BadgerOptions: &badgerOpts,
	})
	assert.Nil(s.T(), err)
}

func (s *BadgerTestSuite) TeardownTest() {
	assert.Nil(s.T(), s.store.Close())
	assert.Nil(s.T(), os.RemoveAll(s.path))
}

func testRaftLog(idx uint64, data string) *raft.Log {
	return &raft.Log{
		Data:  []byte(data),
		Index: idx,
	}
}

func (s *BadgerTestSuite) TestImplements() {
	var store interface{} = &BadgerStore{}

	_, implements := store.(raft.StableStore)
	assert.True(s.T(), implements)

	_, implements = store.(raft.LogStore)
	assert.True(s.T(), implements)
}

func (s *BadgerTestSuite) TestBadgerOptionsReadOnly() {
	log := &raft.Log{
		Data:  []byte("log1"),
		Index: 1,
	}
	assert.Nil(s.T(), s.store.StoreLog(log))
	assert.Nil(s.T(), s.store.Close())

	defaultOpts := badger.DefaultOptions(s.path).WithLogger(nil)
	options := Options{
		Path:          s.path,
		BadgerOptions: &defaultOpts,
	}
	options.BadgerOptions.ReadOnly = true
	roStore, err := New(options)
	assert.Nil(s.T(), err)
	defer roStore.Close()

	result := new(raft.Log)
	assert.Nil(s.T(), roStore.GetLog(1, result))
	assert.True(s.T(), reflect.DeepEqual(log, result))
	assert.Equal(s.T(), badger.ErrReadOnlyTxn, roStore.StoreLog(log))
}

func (s *BadgerTestSuite) TestNewBadgerStore() {
	assert.Equal(s.T(), s.path, s.store.path)
	_, err := os.Stat(s.path)
	assert.Nil(s.T(), err)

	assert.Nil(s.T(), s.store.Close())

	opts := badger.DefaultOptions(s.path).WithLogger(nil)
	db, err := badger.Open(opts)
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), db.Close())
}

func (s *BadgerTestSuite) TestFirstIndex() {
	idx, err := s.store.FirstIndex()
	assert.Nil(s.T(), err)
	assert.Zero(s.T(), idx)

	logs := []*raft.Log{
		testRaftLog(1, "log1"),
		testRaftLog(2, "log2"),
		testRaftLog(3, "log3"),
	}
	assert.Nil(s.T(), s.store.StoreLogs(logs))

	idx, err = s.store.FirstIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(1), idx)
}

func (s *BadgerTestSuite) TestLastIndex() {
	idx, err := s.store.LastIndex()
	assert.Nil(s.T(), err)
	assert.Zero(s.T(), idx)

	logs := []*raft.Log{
		testRaftLog(1, "log1"),
		testRaftLog(2, "log2"),
		testRaftLog(3, "log3"),
	}
	assert.Nil(s.T(), s.store.StoreLogs(logs))

	idx, err = s.store.LastIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(3), idx)
}

func (s *BadgerTestSuite) TestGetLog() {
	log := new(raft.Log)

	assert.Equal(s.T(), raft.ErrLogNotFound, s.store.GetLog(1, log))

	logs := []*raft.Log{
		testRaftLog(1, "log1"),
		testRaftLog(2, "log2"),
		testRaftLog(3, "log3"),
	}
	assert.Nil(s.T(), s.store.StoreLogs(logs))

	assert.Nil(s.T(), s.store.GetLog(2, log))
	assert.True(s.T(), reflect.DeepEqual(log, logs[1]))
}

func (s *BadgerTestSuite) TestSetLog() {
	log := &raft.Log{
		Data:  []byte("log1"),
		Index: 1,
	}

	assert.Nil(s.T(), s.store.StoreLog(log))

	result := new(raft.Log)
	assert.Nil(s.T(), s.store.GetLog(1, result))
	assert.True(s.T(), reflect.DeepEqual(log, result))
}

func (s *BadgerTestSuite) TestSetLogs() {
	logs := []*raft.Log{
		testRaftLog(1, "log1"),
		testRaftLog(2, "log2"),
	}

	assert.Nil(s.T(), s.store.StoreLogs(logs))

	result1, result2 := new(raft.Log), new(raft.Log)
	assert.Nil(s.T(), s.store.GetLog(1, result1))
	assert.True(s.T(), reflect.DeepEqual(logs[0], result1))
	assert.Nil(s.T(), s.store.GetLog(2, result2))
	assert.True(s.T(), reflect.DeepEqual(logs[1], result2))
}

func (s *BadgerTestSuite) TestDeleteRange() {
	log1 := testRaftLog(1, "log1")
	log2 := testRaftLog(2, "log2")
	log3 := testRaftLog(3, "log3")
	logs := []*raft.Log{log1, log2, log3}

	assert.Nil(s.T(), s.store.StoreLogs(logs))

	assert.Nil(s.T(), s.store.DeleteRange(1, 2))

	assert.Equal(s.T(), raft.ErrLogNotFound, s.store.GetLog(1, new(raft.Log)))
	assert.Equal(s.T(), raft.ErrLogNotFound, s.store.GetLog(2, new(raft.Log)))
}

func (s *BadgerTestSuite) TestSetGet() {
	_, err := s.store.Get([]byte("bad"))
	assert.Equal(s.T(), ErrKeyNotFound, err)

	k, v := []byte("hello"), []byte("world")
	assert.Nil(s.T(), s.store.Set(k, v))

	val, err := s.store.Get(k)
	assert.Nil(s.T(), err)
	assert.True(s.T(), bytes.Equal(val, v))
}

func (s *BadgerTestSuite) TestSetGetUint64() {
	_, err := s.store.GetUint64([]byte("bad"))
	assert.Equal(s.T(), ErrKeyNotFound, err)

	k, v := []byte("abc"), uint64(123)
	assert.Nil(s.T(), s.store.SetUint64(k, v))

	val, err := s.store.GetUint64(k)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), v, val)
}

func TestBadgerTestSuite(t *testing.T) {
	suite.Run(t, new(BadgerTestSuite))
}

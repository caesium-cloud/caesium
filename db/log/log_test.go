package log

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/caesium-cloud/caesium/db/store/badger"
	"github.com/hashicorp/raft"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type LogTestSuite struct {
	suite.Suite
	path string
}

func (s *LogTestSuite) SetupTest() {
	var err error
	s.path, err = ioutil.TempDir("", "caesium-db-test")
	assert.Nil(s.T(), err)
}

func (s *LogTestSuite) TeardownTest() {
	assert.Nil(s.T(), os.Remove(s.path))
}

func (s *LogTestSuite) TestNewEmpty() {
	l, err := NewLog(s.path)
	assert.Nil(s.T(), err)

	fi, err := l.FirstIndex()
	assert.Nil(s.T(), err)
	assert.Zero(s.T(), fi)

	li, err := l.LastIndex()
	assert.Nil(s.T(), err)
	assert.Zero(s.T(), li)

	lci, err := l.LastCommandIndex()
	assert.Nil(s.T(), err)
	assert.Zero(s.T(), lci)
}

func (s *LogTestSuite) TestNewExistNotEmpty() {
	bs, err := badger.NewBadgerStore(s.path)
	assert.Nil(s.T(), err)
	for i := 4; i > 0; i-- {
		assert.Nil(s.T(), bs.StoreLog(&raft.Log{Index: uint64(i)}))
	}
	assert.Nil(s.T(), bs.Close())

	l, err := NewLog(s.path)
	assert.Nil(s.T(), err)

	fi, err := l.FirstIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(1), fi)

	li, err := l.LastIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(4), li)

	lci, err := l.LastCommandIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(4), lci)

	assert.Nil(s.T(), l.Close())

	// Delete an entry, recheck index functionality.
	bs, err = badger.NewBadgerStore(s.path)
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), bs.DeleteRange(1, 1))
	assert.Nil(s.T(), bs.Close())

	l, err = NewLog(s.path)
	assert.Nil(s.T(), err)

	fi, err = l.FirstIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(2), fi)

	li, err = l.LastIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(4), li)

	fi, li, err = l.Indexes()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(2), fi)
	assert.Equal(s.T(), uint64(4), li)
	assert.Nil(s.T(), l.Close())
}

func (s *LogTestSuite) TestLastCommandIndexNotExist() {
	bs, err := badger.NewBadgerStore(s.path)
	assert.Nil(s.T(), err)
	for i := 4; i > 0; i-- {
		assert.Nil(s.T(), bs.StoreLog(
			&raft.Log{
				Index: uint64(i),
				Type:  raft.LogNoop,
			},
		))
	}
	assert.Nil(s.T(), bs.Close())

	l, err := NewLog(s.path)
	assert.Nil(s.T(), err)

	fi, err := l.FirstIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(1), fi)

	li, err := l.LastIndex()
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), uint64(4), li)

	lci, err := l.LastCommandIndex()
	assert.Nil(s.T(), err)
	assert.Zero(s.T(), lci)
	assert.Nil(s.T(), l.Close())

	// Delete first log.
	bs, err = badger.NewBadgerStore(s.path)
	assert.Nil(s.T(), err)
	assert.Nil(s.T(), bs.DeleteRange(1, 1))
	assert.Nil(s.T(), bs.Close())

	l, err = NewLog(s.path)
	assert.Nil(s.T(), err)

	lci, err = l.LastCommandIndex()
	assert.Nil(s.T(), err)
	assert.Zero(s.T(), lci)
}

func TestLogTestSuite(t *testing.T) {
	suite.Run(t, new(LogTestSuite))
}

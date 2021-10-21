package atom

import (
	"io/ioutil"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type AtomTestSuite struct {
	suite.Suite
	m sync.Map
}

func (s *AtomTestSuite) SetupTest() {
	dir := mustTempDir()
	tmpStore := store.New(
		mustMockListener("localhost:0"),
		&store.StoreConfig{
			DBConf: store.NewDBConfig("", true),
			Dir:    dir,
			ID:     dir,
		},
	)

	assert.NotNil(s.T(), tmpStore)
	assert.Nil(s.T(), tmpStore.Open(true))

	_, err := tmpStore.WaitForLeader(10 * time.Second)
	assert.Nil(s.T(), err)

	database := db.Service().WithStore(tmpStore)

	resp, err := database.Execute(&db.ExecuteRequest{
		Statements: []*db.Statement{{Sql: models.AtomCreate}},
	})
	assert.Nil(s.T(), err)
	assert.Empty(s.T(), resp.Results[0].Error)
	s.m.Store(s.T().Name(), Service().WithStore(tmpStore))
}

func (s *AtomTestSuite) Service() Atom {
	v, ok := s.m.Load(s.T().Name())
	assert.True(s.T(), ok)
	return v.(Atom)
}

func (s *AtomTestSuite) TestCreate() {
	req := &CreateRequest{
		Engine:  "docker",
		Image:   "caesiumcloud/caesium",
		Command: []string{"caesium", "start"},
	}

	atom, err := s.Service().Create(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), atom)
	assert.NotEmpty(s.T(), atom.ID)
	assert.Equal(s.T(), models.AtomEngine(req.Engine), atom.Engine)
	assert.Equal(s.T(), req.Image, atom.Image)
	assert.NotZero(s.T(), atom.CreatedAt)
	assert.NotZero(s.T(), atom.UpdatedAt)
}

type mockTransport struct {
	ln net.Listener
}

type mockListener struct {
	ln net.Listener
}

func (m *mockListener) Dial(addr string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, timeout)
}

func (m *mockListener) Accept() (net.Conn, error) { return m.ln.Accept() }

func (m *mockListener) Close() error { return m.ln.Close() }

func (m *mockListener) Addr() net.Addr { return m.ln.Addr() }

func mustMockListener(addr string) store.Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic("failed to create new listner")
	}
	return &mockListener{ln}
}

func mustTempDir() string {
	path, err := ioutil.TempDir("", "caesium-test-")
	if err != nil {
		panic("failed to create temp dir")
	}
	return path
}

func TestAtomTestSuite(t *testing.T) {
	suite.Run(t, new(AtomTestSuite))
}

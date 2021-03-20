package kubernetes

import (
	"context"
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/stretchr/testify/assert"
	"k8s.io/client-go/kubernetes/fake"
)

func (s *KubernetesTestSuite) TestNewEngine() {
	engine := NewEngine(
		context.Background(),
		fake.NewSimpleClientset().CoreV1(),
	)
	assert.NotNil(s.T(), engine)
}

func (s *KubernetesTestSuite) TestGet() {
	req := &atom.EngineGetRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Get", testAtomID).
		Return()

	c, err := s.engine.Get(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), req.ID, c.ID())
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestGetError() {
	req := &atom.EngineGetRequest{}

	s.engine.backend.(*mockKubernetesBackend).
		On("Get", "").
		Return(fmt.Errorf("invalid pod name"))

	c, err := s.engine.Get(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestList() {
	req := &atom.EngineListRequest{}

	s.engine.backend.(*mockKubernetesBackend).On("List").Return()

	c, err := s.engine.List(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.Len(s.T(), c, 1)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestCreate() {
	req := &atom.EngineCreateRequest{
		Name:    testAtomID,
		Image:   testImage,
		Command: []string{"test", "cmd"},
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Create").
		Return()

	c, err := s.engine.Create(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.True(s.T(), strings.HasPrefix(c.ID(), testAtomID))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestCreateError() {
	req := &atom.EngineCreateRequest{
		Name:    "",
		Image:   testImage,
		Command: []string{"test", "cmd"},
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Create").
		Return(fmt.Errorf("invalid pod name"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestStop() {
	req := &atom.EngineStopRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Delete", testAtomID).
		Return()

	assert.Nil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestStopError() {
	req := &atom.EngineStopRequest{
		ID: "",
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Delete", "").
		Return(fmt.Errorf("invalid pod name"))

	assert.NotNil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestStopTimeout() {
	req := &atom.EngineStopRequest{
		ID:      testAtomID,
		Timeout: time.Nanosecond,
		Force:   true,
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Delete", testAtomID).
		Return()

	assert.NotNil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestLogs() {
	req := &atom.EngineLogsRequest{
		ID:    testAtomID,
		Since: time.Now(),
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("GetLogs", testAtomID).
		Return()

	logs, err := s.engine.Logs(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), logs)

	buf, err := ioutil.ReadAll(logs)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "logs", string(buf))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestLogsError() {
	req := &atom.EngineLogsRequest{ID: ""}

	s.engine.backend.(*mockKubernetesBackend).
		On("GetLogs", "").
		Return()

	logs, err := s.engine.Logs(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), logs)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

package docker

import (
	"context"
	"fmt"
	"time"

	"github.com/caesium-dev/caesium/internal/capsule"
	"github.com/stretchr/testify/assert"
)

func (s *DockerTestSuite) TestNewEngine() {
	engine := NewEngine(context.Background())
	assert.NotNil(s.T(), engine)
}

func (s *DockerTestSuite) TestGet() {
	req := &capsule.EngineGetRequest{
		ID: testCapsuleID,
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testCapsuleID).
		Return()

	c, err := s.engine.Get(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), req.ID, c.ID())
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestGetInvalidContainer() {
	req := &capsule.EngineGetRequest{}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", "").
		Return(fmt.Errorf("invalid container id"))

	c, err := s.engine.Get(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestList() {
	req := &capsule.EngineListRequest{}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerList").
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testCapsuleID).
		Return()

	capsules, err := s.engine.List(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), capsules)
	assert.Len(s.T(), capsules, 1)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestListError() {
	req := &capsule.EngineListRequest{
		Since: time.Now(),
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerList").
		Return(fmt.Errorf("docker daeamon list error"))

	capsules, err := s.engine.List(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), capsules)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestListGetError() {
	req := &capsule.EngineListRequest{
		Before: time.Now(),
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerList").
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", "").
		Return(fmt.Errorf("invalid container id"))

	capsules, err := s.engine.List(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), capsules)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreate() {
	req := &capsule.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", testContainerName).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerStart", testCapsuleID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testCapsuleID).
		Return()

	c, err := s.engine.Create(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.Equal(s.T(), testCapsuleID, c.ID())
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateError() {
	req := &capsule.EngineCreateRequest{
		Name:    testContainerName,
		Image:   "",
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", testContainerName).
		Return(fmt.Errorf("invalid container image"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateStartError() {
	req := &capsule.EngineCreateRequest{
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", "").
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerStart", "").
		Return(fmt.Errorf("invalid container id"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestStop() {
	req := &capsule.EngineStopRequest{
		ID: testCapsuleID,
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerStop", testCapsuleID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerRemove", testCapsuleID).
		Return()

	assert.Nil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestStopError() {
	req := &capsule.EngineStopRequest{ID: ""}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerStop", "").
		Return(fmt.Errorf("invalid container id"))

	assert.NotNil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestLogs() {
	req := &capsule.EngineLogsRequest{
		ID: testCapsuleID,
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerLogs", testCapsuleID).
		Return()

	logs, err := s.engine.Logs(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), logs)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

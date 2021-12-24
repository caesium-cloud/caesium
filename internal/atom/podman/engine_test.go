package podman

import (
	"fmt"
	"io/ioutil"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/stretchr/testify/assert"
)

// func (s *PodmanTestSuite) TestNewEngine() {
// 	engine := NewEngine(context.Background())
// 	assert.NotNil(s.T(), engine)
// }

func (s *PodmanTestSuite) TestGet() {
	req := &atom.EngineGetRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	c, err := s.engine.Get(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), req.ID, c.ID())
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestGetError() {
	req := &atom.EngineGetRequest{}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", "").
		Return(fmt.Errorf("invalid container id"))

	c, err := s.engine.Get(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestList() {
	req := &atom.EngineListRequest{}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerList").
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	atoms, err := s.engine.List(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), atoms)
	assert.Len(s.T(), atoms, 1)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestListError() {
	req := &atom.EngineListRequest{
		Since: time.Now(),
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerList").
		Return(fmt.Errorf("docker daeamon list error"))

	atoms, err := s.engine.List(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), atoms)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestListGetError() {
	req := &atom.EngineListRequest{
		Before: time.Now(),
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerList").
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", "").
		Return(fmt.Errorf("invalid container id"))

	atoms, err := s.engine.List(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), atoms)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreate() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", testImage).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", testContainerName).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	c, err := s.engine.Create(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.Equal(s.T(), testAtomID, c.ID())
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreateError() {
	req := &atom.EngineCreateRequest{
		Name:    "fail",
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", req.Name).
		Return(fmt.Errorf("invalid container image"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreatePullError() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   "",
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", "").
		Return(fmt.Errorf("invalid image"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreateStartError() {
	req := &atom.EngineCreateRequest{
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", req.Name).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", req.Name).
		Return(fmt.Errorf("invalid container id"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestStop() {
	req := &atom.EngineStopRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStop", testAtomID).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerRemove", testAtomID).
		Return()

	assert.Nil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestStopError() {
	req := &atom.EngineStopRequest{ID: ""}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStop", "").
		Return(fmt.Errorf("invalid container id"))

	assert.NotNil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestLogs() {
	req := &atom.EngineLogsRequest{
		ID:    testAtomID,
		Since: time.Now(),
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerLogs", testAtomID).
		Return()

	logs, err := s.engine.Logs(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), logs)

	buf, err := ioutil.ReadAll(logs)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "logs", string(buf))
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

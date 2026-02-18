package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func (s *DockerTestSuite) TestNewEngine() {
	engine := NewEngine(context.Background())
	assert.NotNil(s.T(), engine)
}

func (s *DockerTestSuite) TestGet() {
	req := &atom.EngineGetRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testAtomID).
		Return()

	c, err := s.engine.Get(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), req.ID, c.ID())
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestGetError() {
	req := &atom.EngineGetRequest{}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", "").
		Return(fmt.Errorf("invalid container id"))

	c, err := s.engine.Get(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestList() {
	req := &atom.EngineListRequest{}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerList").
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testAtomID).
		Return()

	atoms, err := s.engine.List(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), atoms)
	assert.Len(s.T(), atoms, 1)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestListError() {
	req := &atom.EngineListRequest{
		Since: time.Now(),
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerList").
		Return(fmt.Errorf("docker daeamon list error"))

	atoms, err := s.engine.List(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), atoms)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestListGetError() {
	req := &atom.EngineListRequest{
		Before: time.Now(),
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerList").
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", "").
		Return(fmt.Errorf("invalid container id"))

	atoms, err := s.engine.List(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), atoms)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreate() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", testImage).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", mock.AnythingOfType("*container.Config"), mock.Anything, testContainerName).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testAtomID).
		Return()

	c, err := s.engine.Create(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.Equal(s.T(), testAtomID, c.ID())
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateAppliesSpec() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			Env: map[string]string{
				"FOO": "bar",
				"BAR": "baz",
			},
			WorkDir: "/workspace",
			Mounts: []container.Mount{{
				Type:   container.MountTypeBind,
				Source: "/host/data",
				Target: "/data",
			}},
		},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", req.Image).
		Return()

	cfgMatcher := mock.MatchedBy(func(cfg *dockercontainer.Config) bool {
		return cfg.WorkingDir == "/workspace" &&
			len(cfg.Env) == 2 &&
			cfg.Env[0] == "BAR=baz" &&
			cfg.Env[1] == "FOO=bar"
	})

	hostMatcher := mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
		if host == nil || len(host.Mounts) != 1 {
			return false
		}
		m := host.Mounts[0]
		return m.Source == "/host/data" && m.Target == "/data" && m.Type == mount.TypeBind
	})

	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", cfgMatcher, hostMatcher, req.Name).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testAtomID).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreatePullError() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   "",
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", "").
		Return(fmt.Errorf("invalid image"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateError() {
	req := &atom.EngineCreateRequest{
		Name:    "fail",
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", mock.AnythingOfType("*container.Config"), mock.Anything, req.Name).
		Return(fmt.Errorf("invalid container image"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateStartError() {
	req := &atom.EngineCreateRequest{
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", mock.AnythingOfType("*container.Config"), mock.Anything, req.Name).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerStart", req.Name).
		Return(fmt.Errorf("invalid container id"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestStop() {
	req := &atom.EngineStopRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerStop", testAtomID).
		Return()

	// ContainerRemove is now skipped to support post-execution logs
	// s.engine.backend.(*mockDockerBackend).
	// 	On("ContainerRemove", testAtomID).
	// 	Return()

	assert.Nil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestStopError() {
	req := &atom.EngineStopRequest{ID: ""}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerStop", "").
		Return(fmt.Errorf("invalid container id"))

	assert.NotNil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestLogs() {
	req := &atom.EngineLogsRequest{
		ID:    testAtomID,
		Since: time.Now(),
	}

	// Simulate multiplexed logs: [STREAM_TYPE, 0, 0, 0, SIZE1, SIZE2, SIZE3, SIZE4] + content
	// Stream type 1 = stdout
	header := []byte{1, 0, 0, 0, 0, 0, 0, 4}
	content := []byte("logs")
	mockLogs := append(header, content...)

	s.engine.backend.(*mockDockerBackend).
		On("ContainerLogs", testAtomID).
		Return(io.ReadCloser(io.NopCloser(bytes.NewReader(mockLogs))), nil)

	logs, err := s.engine.Logs(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), logs)

	buf, err := io.ReadAll(logs)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "logs", string(buf))
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

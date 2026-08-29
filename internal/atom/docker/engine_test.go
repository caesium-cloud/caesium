package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/errdefs"
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
		On("ImageInspect", testImage).
		Return(errdefs.NotFound(io.EOF))
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
		On("ImageInspect", req.Image).
		Return(errdefs.NotFound(io.EOF))
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

func (s *DockerTestSuite) TestCreateAppliesResolvedVolumeMounts() {
	mode := 0o700
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			ResolvedVolumeMounts: []container.VolumeMount{
				{
					Name:     "cache",
					Type:     container.VolumeMountTypeVolume,
					Source:   "caesium-cache",
					Target:   "/cache",
					ReadOnly: true,
				},
				{
					Name:   "scratch",
					Type:   container.VolumeMountTypeTmpfs,
					Target: "/scratch",
					Tmpfs:  &container.TmpfsOptions{SizeBytes: 1024, Mode: &mode},
				},
			},
		},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(errdefs.NotFound(io.EOF))
	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", req.Image).
		Return()

	hostMatcher := mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
		if host == nil || len(host.Mounts) != 2 {
			return false
		}
		named := host.Mounts[0]
		tmpfs := host.Mounts[1]
		return named.Type == mount.TypeVolume &&
			named.Source == "caesium-cache" &&
			named.Target == "/cache" &&
			named.ReadOnly &&
			tmpfs.Type == mount.TypeTmpfs &&
			tmpfs.Target == "/scratch" &&
			tmpfs.TmpfsOptions != nil &&
			tmpfs.TmpfsOptions.SizeBytes == 1024
	})

	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", mock.AnythingOfType("*container.Config"), hostMatcher, req.Name).
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

func (s *DockerTestSuite) TestCreateAppliesVolumeSubPath() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			ResolvedVolumeMounts: []container.VolumeMount{{
				Name:    "tfstate",
				Type:    container.VolumeMountTypeVolume,
				Source:  "tfstate-vol",
				Target:  "/state",
				SubPath: "stack-a",
			}},
		},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(errdefs.NotFound(io.EOF))
	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ClientVersion").
		Return("1.47")
	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", subPathHelperImage).
		Return(errdefs.NotFound(io.EOF))
	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", subPathHelperImage).
		Return()

	helperCfgMatcher := mock.MatchedBy(func(cfg *dockercontainer.Config) bool {
		return cfg.Image == subPathHelperImage &&
			len(cfg.Cmd) == 3 &&
			cfg.Cmd[0] == "mkdir" &&
			cfg.Cmd[1] == "-p" &&
			cfg.Cmd[2] == subPathHelperMountDir+"/stack-a"
	})
	helperHostMatcher := mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
		return host != nil && len(host.Mounts) == 1 &&
			host.Mounts[0].Type == mount.TypeVolume &&
			host.Mounts[0].Source == "tfstate-vol" &&
			host.Mounts[0].Target == subPathHelperMountDir
	})
	helperNameMatcher := mock.MatchedBy(func(name string) bool {
		return strings.HasPrefix(name, "caesium-subpath-init-")
	})
	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", helperCfgMatcher, helperHostMatcher, helperNameMatcher).
		Return()

	mainHostMatcher := mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
		if host == nil || len(host.Mounts) != 1 {
			return false
		}
		m := host.Mounts[0]
		return m.Type == mount.TypeVolume &&
			m.Source == "tfstate-vol" &&
			m.Target == "/state" &&
			m.VolumeOptions != nil &&
			m.VolumeOptions.Subpath == "stack-a"
	})
	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", mock.AnythingOfType("*container.Config"), mainHostMatcher, req.Name).
		Return()

	s.engine.backend.(*mockDockerBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerWait", testAtomID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerRemove", testAtomID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testAtomID).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateAppliesBindSubPath() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			ResolvedVolumeMounts: []container.VolumeMount{{
				Name:    "tfstate",
				Type:    container.VolumeMountTypeBind,
				Source:  "/host/tfstate",
				Target:  "/state",
				SubPath: "stack-a",
			}},
		},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(errdefs.NotFound(io.EOF))
	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", req.Image).
		Return()

	hostMatcher := mock.MatchedBy(func(host *dockercontainer.HostConfig) bool {
		if host == nil || len(host.Mounts) != 1 {
			return false
		}
		m := host.Mounts[0]
		return m.Type == mount.TypeBind && m.Source == "/host/tfstate/stack-a" && m.Target == "/state"
	})
	s.engine.backend.(*mockDockerBackend).
		On("ContainerCreate", mock.AnythingOfType("*container.Config"), hostMatcher, req.Name).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testAtomID).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockDockerBackend).AssertNotCalled(s.T(), "ClientVersion")
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateVolumeSubPathUnsupportedAPIVersion() {
	req := &atom.EngineCreateRequest{
		Name:  testContainerName,
		Image: testImage,
		Spec: container.Spec{
			ResolvedVolumeMounts: []container.VolumeMount{{
				Type:    container.VolumeMountTypeVolume,
				Source:  "tfstate-vol",
				Target:  "/state",
				SubPath: "stack-a",
			}},
		},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(nil)
	s.engine.backend.(*mockDockerBackend).
		On("ClientVersion").
		Return("1.40")

	c, err := s.engine.Create(req)
	s.Require().Error(err)
	s.Assert().Nil(c)
	s.Assert().Contains(err.Error(), "1.45")
	s.engine.backend.(*mockDockerBackend).AssertNotCalled(s.T(), "ContainerCreate", mock.Anything, mock.Anything, mock.Anything)
	s.engine.backend.(*mockDockerBackend).AssertNotCalled(s.T(), "ImagePull", subPathHelperImage)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateVolumeSubPathHelperImagePullError() {
	req := &atom.EngineCreateRequest{
		Name:  testContainerName,
		Image: testImage,
		Spec: container.Spec{
			ResolvedVolumeMounts: []container.VolumeMount{{
				Type:    container.VolumeMountTypeVolume,
				Source:  "tfstate-vol",
				Target:  "/state",
				SubPath: "stack-a",
			}},
		},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(nil)
	s.engine.backend.(*mockDockerBackend).
		On("ClientVersion").
		Return("1.47")
	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", subPathHelperImage).
		Return(errdefs.NotFound(io.EOF))
	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", subPathHelperImage).
		Return(fmt.Errorf("pull failed"))

	c, err := s.engine.Create(req)
	s.Require().Error(err)
	s.Assert().Nil(c)
	s.engine.backend.(*mockDockerBackend).AssertNotCalled(s.T(), "ContainerCreate", mock.Anything, mock.Anything, mock.Anything)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreateSkipsPullWhenImageAlreadyPresent() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(nil)
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
	s.engine.backend.(*mockDockerBackend).AssertNotCalled(s.T(), "ImagePull", req.Image)
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

func (s *DockerTestSuite) TestCreateInspectError() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(fmt.Errorf("inspect failed"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockDockerBackend).AssertNotCalled(s.T(), "ImagePull", req.Image)
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestCreatePullErrorWhenImageMissing() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockDockerBackend).
		On("ImageInspect", req.Image).
		Return(errdefs.NotFound(io.EOF))
	s.engine.backend.(*mockDockerBackend).
		On("ImagePull", req.Image).
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
		On("ImageInspect", req.Image).
		Return(errdefs.NotFound(io.EOF))
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
		On("ImageInspect", req.Image).
		Return(errdefs.NotFound(io.EOF))
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

	s.engine.backend.(*mockDockerBackend).
		On("ContainerRemove", testAtomID).
		Return()

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

func (s *DockerTestSuite) TestWait() {
	req := &atom.EngineWaitRequest{
		ID:      testAtomID,
		Context: context.Background(),
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerWait", testAtomID).
		Return()
	s.engine.backend.(*mockDockerBackend).
		On("ContainerInspect", testAtomID).
		Return()

	c, err := s.engine.Wait(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.Equal(s.T(), testAtomID, c.ID())
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func (s *DockerTestSuite) TestWaitError() {
	req := &atom.EngineWaitRequest{
		ID:      "",
		Context: context.Background(),
	}

	s.engine.backend.(*mockDockerBackend).
		On("ContainerWait", "").
		Return(fmt.Errorf("container wait error"))

	c, err := s.engine.Wait(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
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
		Return(io.NopCloser(bytes.NewReader(mockLogs)), nil)

	logs, err := s.engine.Logs(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), logs)

	buf, err := io.ReadAll(logs)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "logs", string(buf))
	s.engine.backend.(*mockDockerBackend).AssertExpectations(s.T())
}

func TestHasVolumeSubPath(t *testing.T) {
	assert.False(t, hasVolumeSubPath(nil))
	assert.False(t, hasVolumeSubPath([]container.VolumeMount{{Type: container.VolumeMountTypeVolume}}))
	assert.False(t, hasVolumeSubPath([]container.VolumeMount{{Type: container.VolumeMountTypeBind, SubPath: "x"}}))
	assert.False(t, hasVolumeSubPath([]container.VolumeMount{{Type: container.VolumeMountTypeTmpfs, SubPath: "x"}}))
	assert.True(t, hasVolumeSubPath([]container.VolumeMount{{Type: container.VolumeMountTypeVolume, SubPath: "x"}}))
}

func TestConvertMountsVolumeSubPathSupported(t *testing.T) {
	mounts, err := convertMounts(nil, []container.VolumeMount{{
		Type:    container.VolumeMountTypeVolume,
		Source:  "tfstate",
		Target:  "/state",
		SubPath: "stack-a",
	}}, true)

	assert.NoError(t, err)
	if assert.Len(t, mounts, 1) {
		assert.Equal(t, mount.TypeVolume, mounts[0].Type)
		assert.Equal(t, "tfstate", mounts[0].Source)
		assert.Equal(t, "/state", mounts[0].Target)
		if assert.NotNil(t, mounts[0].VolumeOptions) {
			assert.Equal(t, "stack-a", mounts[0].VolumeOptions.Subpath)
		}
	}
}

func TestConvertMountsVolumeSubPathUnsupportedAPIVersion(t *testing.T) {
	mounts, err := convertMounts(nil, []container.VolumeMount{{
		Type:    container.VolumeMountTypeVolume,
		Source:  "tfstate",
		Target:  "/state",
		SubPath: "stack-a",
	}}, false)

	assert.Error(t, err)
	assert.Nil(t, mounts)
	assert.Contains(t, err.Error(), "subPath")
	assert.Contains(t, err.Error(), "1.45")
}

func TestConvertMountsVolumeNoSubPathUnaffectedByAPIVersion(t *testing.T) {
	mounts, err := convertMounts(nil, []container.VolumeMount{{
		Type:   container.VolumeMountTypeVolume,
		Source: "cache",
		Target: "/cache",
	}}, false)

	assert.NoError(t, err)
	if assert.Len(t, mounts, 1) {
		assert.Nil(t, mounts[0].VolumeOptions)
	}
}

func TestConvertMountsBindSubPathJoinsSource(t *testing.T) {
	mounts, err := convertMounts(nil, []container.VolumeMount{{
		Type:    container.VolumeMountTypeBind,
		Source:  "/data",
		Target:  "/state",
		SubPath: "stack-a/nested",
	}}, false)

	assert.NoError(t, err)
	if assert.Len(t, mounts, 1) {
		assert.Equal(t, mount.TypeBind, mounts[0].Type)
		assert.Equal(t, "/data/stack-a/nested", mounts[0].Source)
		assert.Nil(t, mounts[0].VolumeOptions)
	}
}

func TestConvertMountsBindNoSubPathUnchanged(t *testing.T) {
	mounts, err := convertMounts(nil, []container.VolumeMount{{
		Type:   container.VolumeMountTypeBind,
		Source: "/data",
		Target: "/state",
	}}, false)

	assert.NoError(t, err)
	if assert.Len(t, mounts, 1) {
		assert.Equal(t, "/data", mounts[0].Source)
	}
}

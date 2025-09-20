package docker

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

var (
	testAtomID        = "test_id"
	testContainerName = "test_atom"
	testImage         = "caesiumcloud/caesium"
)

type DockerTestSuite struct {
	suite.Suite
	engine *dockerEngine
}

type mockDockerBackend struct {
	mock.Mock
	dockerBackend
}

func (m *mockDockerBackend) ContainerInspect(ctx context.Context, container string) (types.ContainerJSON, error) {
	args := m.Called(container)
	if container == "" {
		return types.ContainerJSON{}, args.Error(0)
	}

	return newContainer(container, &types.ContainerState{}), nil
}

func (m *mockDockerBackend) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	args := m.Called()
	if options.Since != "" {
		return nil, args.Error(0)
	}

	if options.Before != "" {
		return []types.Container{{ID: ""}}, nil
	}

	return []types.Container{
		{
			ID: testAtomID,
		},
	}, nil
}

func (m *mockDockerBackend) ContainerCreate(ctx context.Context, config *containertypes.Config, hostConfig *containertypes.HostConfig, networkingConfig *networktypes.NetworkingConfig, platform *specs.Platform, containerName string) (container.CreateResponse, error) {
	args := m.Called(containerName)

	switch containerName {
	case "fail":
		return container.CreateResponse{}, args.Error(0)
	case "":
		return container.CreateResponse{ID: ""}, nil
	default:
		return container.CreateResponse{ID: testAtomID}, nil
	}
}

func (m *mockDockerBackend) ContainerStart(ctx context.Context, container string, options container.StartOptions) error {
	args := m.Called(container)
	if container == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockDockerBackend) ContainerStop(ctx context.Context, container string, options container.StopOptions) error {
	args := m.Called(container)
	if container == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockDockerBackend) ContainerRemove(ctx context.Context, container string, options container.RemoveOptions) error {
	args := m.Called(container)
	if container == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockDockerBackend) ContainerLogs(ctx context.Context, container string, options container.LogsOptions) (io.ReadCloser, error) {
	args := m.Called(container)
	if container == "" {
		return nil, args.Error(0)
	}
	return ioutil.NopCloser(bytes.NewReader([]byte("logs"))), nil
}

func (m *mockDockerBackend) ImagePull(ctx context.Context, image string, options image.PullOptions) (io.ReadCloser, error) {
	args := m.Called(image)
	if image == "" {
		return nil, args.Error(0)
	}
	return ioutil.NopCloser(bytes.NewReader([]byte("pull"))), nil
}

func newContainer(id string, state *types.ContainerState) types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{
			ID:      id,
			State:   state,
			Created: time.Now().Format(time.RFC3339Nano),
		},
	}
}

func (s *DockerTestSuite) SetupTest() {
	s.engine = &dockerEngine{
		backend: &mockDockerBackend{},
		ctx:     context.Background(),
	}
}

func TestDockerTestSuite(t *testing.T) {
	suite.Run(t, new(DockerTestSuite))
}

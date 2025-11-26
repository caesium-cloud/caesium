package docker

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	dockercontainer "github.com/docker/docker/api/types/container"
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
}

func (m *mockDockerBackend) ContainerInspect(ctx context.Context, containerID string) (dockercontainer.InspectResponse, error) {
	args := m.Called(containerID)
	if containerID == "" {
		return dockercontainer.InspectResponse{}, args.Error(0)
	}

	return newContainer(containerID, &dockercontainer.State{}), nil
}

func (m *mockDockerBackend) ContainerList(ctx context.Context, options dockercontainer.ListOptions) ([]dockercontainer.Summary, error) {
	args := m.Called()
	if options.Since != "" {
		return nil, args.Error(0)
	}

	if options.Before != "" {
		return []dockercontainer.Summary{{ID: ""}}, nil
	}

	return []dockercontainer.Summary{
		{
			ID: testAtomID,
		},
	}, nil
}

func (m *mockDockerBackend) ContainerCreate(ctx context.Context, config *dockercontainer.Config, hostConfig *dockercontainer.HostConfig, networkingConfig *networktypes.NetworkingConfig, platform *specs.Platform, containerName string) (dockercontainer.CreateResponse, error) {
	args := m.Called(config, hostConfig, containerName)

	switch containerName {
	case "fail":
		return dockercontainer.CreateResponse{}, args.Error(0)
	case "":
		return dockercontainer.CreateResponse{ID: ""}, nil
	default:
		return dockercontainer.CreateResponse{ID: testAtomID}, nil
	}
}

func (m *mockDockerBackend) ContainerStart(ctx context.Context, container string, options dockercontainer.StartOptions) error {
	args := m.Called(container)
	if container == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockDockerBackend) ContainerStop(ctx context.Context, container string, options dockercontainer.StopOptions) error {
	args := m.Called(container)
	if container == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockDockerBackend) ContainerRemove(ctx context.Context, container string, options dockercontainer.RemoveOptions) error {
	args := m.Called(container)
	if container == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockDockerBackend) ContainerLogs(ctx context.Context, containerID string, options dockercontainer.LogsOptions) (io.ReadCloser, error) {
	args := m.Called(containerID)
	if containerID == "" {
		return nil, args.Error(0)
	}
	return io.NopCloser(bytes.NewReader([]byte("logs"))), nil
}

func (m *mockDockerBackend) ImagePull(ctx context.Context, imageRef string, options image.PullOptions) (io.ReadCloser, error) {
	args := m.Called(imageRef)
	if imageRef == "" {
		return nil, args.Error(0)
	}
	return io.NopCloser(bytes.NewReader([]byte("pull"))), nil
}

func newContainer(id string, state *dockercontainer.State) dockercontainer.InspectResponse {
	return dockercontainer.InspectResponse{
		ContainerJSONBase: &dockercontainer.ContainerJSONBase{
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

package podman

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/containers/podman/v5/pkg/domain/entities"
	"github.com/containers/podman/v5/pkg/specgen"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

var (
	testAtomID        = "test_id"
	testContainerName = "test_atom"
	testImage         = "caesiumcloud/caesium"
)

type PodmanTestSuite struct {
	suite.Suite
	engine *podmanEngine
}

type mockPodmanBackend struct {
	mock.Mock
}

func (m *mockPodmanBackend) ContainerInspect(container string) (*define.InspectContainerData, error) {
	args := m.Called(container)
	if container == "" {
		return &define.InspectContainerData{}, args.Error(0)
	}

	return newContainer(container, &define.InspectContainerState{}), nil
}

func (m *mockPodmanBackend) ContainerList(filters map[string][]string, all bool) ([]entities.ListContainer, error) {
	args := m.Called()

	if _, ok := filters["since"]; ok {
		return nil, args.Error(0)
	}

	if _, ok := filters["before"]; ok {
		return []entities.ListContainer{{ID: ""}}, nil
	}

	return []entities.ListContainer{{ID: testAtomID}}, nil
}

func (m *mockPodmanBackend) ContainerCreate(spec *specgen.SpecGenerator) (entities.ContainerCreateResponse, error) {
	args := m.Called(spec.Name)

	switch spec.Name {
	case "fail":
		return entities.ContainerCreateResponse{}, args.Error(0)
	case "":
		return entities.ContainerCreateResponse{ID: ""}, nil
	default:
		return entities.ContainerCreateResponse{ID: testAtomID}, nil
	}
}

func (m *mockPodmanBackend) ContainerStart(id string) error {
	args := m.Called(id)
	if id == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockPodmanBackend) ContainerStop(id string, timeout *time.Duration) error {
	args := m.Called(id)
	if id == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockPodmanBackend) ContainerRemove(id string, force *bool, removeVolumes *bool) error {
	args := m.Called(id)
	if id == "" {
		return args.Error(0)
	}
	return nil
}

func (m *mockPodmanBackend) ContainerLogs(id string, opts containers.LogOptions) (io.ReadCloser, error) {
	args := m.Called(id)
	if id == "" {
		return nil, args.Error(0)
	}
	return io.NopCloser(bytes.NewReader([]byte("logs"))), nil
}

func (m *mockPodmanBackend) ImagePull(image string, opts *images.PullOptions) (io.ReadCloser, error) {
	args := m.Called(image)
	if image == "" {
		return nil, args.Error(0)
	}
	return io.NopCloser(bytes.NewReader([]byte("pull"))), nil
}

func newContainer(id string, state *define.InspectContainerState) *define.InspectContainerData {
	return &define.InspectContainerData{
		ID:      id,
		State:   state,
		Created: time.Now(),
	}
}

func (s *PodmanTestSuite) SetupTest() {
	s.engine = &podmanEngine{
		backend: &mockPodmanBackend{},
		ctx:     context.Background(),
	}
}

func TestPodmanTestSuite(t *testing.T) {
	suite.Run(t, new(PodmanTestSuite))
}

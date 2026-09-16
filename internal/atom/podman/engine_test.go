package podman

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/domain/entities"
	"github.com/containers/podman/v5/pkg/errorhandling"
	"github.com/containers/podman/v5/pkg/specgen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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
		On("ImageExists", testImage).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", testImage).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
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

func (s *PodmanTestSuite) TestCreateAppliesSpec() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			Env:     map[string]string{"FOO": "bar"},
			WorkDir: "/app",
			Mounts: []container.Mount{{
				Type:   container.MountTypeBind,
				Source: "/host",
				Target: "/data",
			}},
		},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return()

	specMatcher := mock.MatchedBy(func(spec *specgen.SpecGenerator) bool {
		return spec.WorkDir == "/app" &&
			len(spec.Env) == 1 &&
			spec.Env["FOO"] == "bar" &&
			len(spec.Mounts) == 1 &&
			spec.Mounts[0].Source == "/host" &&
			spec.Mounts[0].Destination == "/data"
	})

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", specMatcher).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreateAppliesResolvedVolumeMounts() {
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
					SubPath:  "objects",
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

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", "caesium-cache").
		Return(true, nil)

	specMatcher := mock.MatchedBy(func(spec *specgen.SpecGenerator) bool {
		if len(spec.Volumes) != 1 || len(spec.Mounts) != 1 {
			return false
		}
		vol := spec.Volumes[0]
		tmpfs := spec.Mounts[0]
		return vol.Name == "caesium-cache" &&
			vol.Dest == "/cache" &&
			vol.SubPath == "objects" &&
			len(vol.Options) == 1 &&
			vol.Options[0] == "ro" &&
			tmpfs.Type == string(container.MountTypeTmpfs) &&
			tmpfs.Source == string(container.MountTypeTmpfs) &&
			tmpfs.Destination == "/scratch"
	})

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", specMatcher).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreateSkipsPullWhenImageAlreadyPresent() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
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
	s.engine.backend.(*mockPodmanBackend).AssertNotCalled(s.T(), "ImagePull", req.Image)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreatePullsDigestWhenTagIsAlreadyPresent is the engine half of the
// warm-tag/moved-tag case: ImageExists(tag) would return true for the stale
// local content, but Create is given the digest-pinned ref, so presence is
// checked (and pulled) against that identity.
func (s *PodmanTestSuite) TestCreatePullsDigestWhenTagIsAlreadyPresent() {
	const digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pinned := testImage + "@" + digest
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   pinned,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", pinned).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", pinned).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.MatchedBy(func(spec *specgen.SpecGenerator) bool {
			return spec != nil && spec.Image == pinned
		})).
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
	s.engine.backend.(*mockPodmanBackend).AssertNotCalled(s.T(), "ImageExists", testImage)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateSetsHealthLogDestination guards a podman API trap: the specgen
// field carries no `omitempty` (upstream defers that to v6.0), so an unset
// HealthLogDestination is serialized as "" rather than omitted. Podman servers
// validate it and stat("") fails, rejecting every container create with:
//
//	HealthCheck Log '' destination error: stat : no such file or directory
//
// Sending the documented default keeps creates working across podman versions.
func (s *PodmanTestSuite) TestCreateSetsHealthLogDestination() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	specMatcher := mock.MatchedBy(func(spec *specgen.SpecGenerator) bool {
		return spec.HealthLogDestination == define.DefaultHealthCheckLocalDestination
	})

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", specMatcher).
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
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreateError() {
	req := &atom.EngineCreateRequest{
		Name:    "fail",
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
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

func (s *PodmanTestSuite) TestCreateExistsError() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(false, fmt.Errorf("exists failed"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertNotCalled(s.T(), "ImagePull", req.Image)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreatePullErrorWhenImageMissing() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return(fmt.Errorf("invalid image"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateStartError also proves the #480/round-3 orphan-cleanup fix: once
// ContainerCreate has allocated a container, a subsequent failure (here,
// ContainerStart) must stop+remove it rather than just returning the error
// with no handle to clean up later.
func (s *PodmanTestSuite) TestCreateStartError() {
	req := &atom.EngineCreateRequest{
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", req.Name).
		Return(fmt.Errorf("invalid container id"))
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStop", req.Name).
		Return(nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerRemove", req.Name).
		Return(nil)

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateGetError covers the exact scenario a round-3 adversarial review
// caught: ContainerStart succeeds but the immediately following Get
// (ContainerInspect) fails — the shape of a SIGINT landing in that window
// during `caesium dev --once`. Before the fix, Create returned the error
// with no atom.Atom handle, so nothing else in the system ever learned the
// already-started container's ID to stop it — an orphan despite #480.
func (s *PodmanTestSuite) TestCreateGetError() {
	req := &atom.EngineCreateRequest{
		Image:   testImage,
		Command: []string{"test"},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", req.Image).
		Return(false, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ImagePull", req.Image).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", req.Name).
		Return(nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", req.Name).
		Return(fmt.Errorf("context canceled"))
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStop", req.Name).
		Return(nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerRemove", req.Name).
		Return(nil)

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateCancelledDuringCreateRequest covers a round-4 adversarial-review
// finding: even after TestCreateStartError/TestCreateGetError's fix, a
// SIGINT landing WHILE the ContainerCreate request itself is in flight
// could still orphan a container — the server may commit it before the
// client sees a cancellation error, and Create would return with no ID at
// all to clean up. The fake backend cancels the engine's context from
// inside its ContainerCreate handler (simulating the server completing the
// request at the exact moment SIGINT arrives) and then reports success,
// proving Create still definitively completes the allocation call, notices
// the cancellation afterward, and removes the container it just learned
// about rather than leaking it.
func (s *PodmanTestSuite) TestCreateCancelledDuringCreateRequest() {
	ctx, cancel := context.WithCancel(context.Background())
	backend := &mockPodmanBackend{}
	engine := &podmanEngine{backend: backend, ctx: ctx}

	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"test"},
	}

	backend.On("ImageExists", req.Image).Return(false, nil)
	backend.On("ImagePull", req.Image).Return()
	backend.
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
		Run(func(mock.Arguments) { cancel() }).
		Return()
	backend.On("ContainerStop", testAtomID).Return(nil)
	backend.On("ContainerRemove", testAtomID).Return(nil)

	c, err := engine.Create(req)
	assert.Nil(s.T(), c)
	assert.ErrorIs(s.T(), err, context.Canceled,
		"Create must report the cancellation, not a spurious success, once it notices the caller gave up")
	backend.AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestWait() {
	req := &atom.EngineWaitRequest{
		ID:      testAtomID,
		Context: context.Background(),
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerWait", testAtomID, req.Context).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	c, err := s.engine.Wait(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.Equal(s.T(), testAtomID, c.ID())
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestWaitError() {
	req := &atom.EngineWaitRequest{
		ID:      "",
		Context: context.Background(),
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerWait", "", req.Context).
		Return(fmt.Errorf("container wait error"))

	c, err := s.engine.Wait(req)
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

	buf, err := io.ReadAll(logs)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "logs", string(buf))
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// namedVolumeCreateRequest builds a create request that mounts one named
// volume, the shape two sibling tasks race on when they first-mount a shared
// volume (caesium#443).
func namedVolumeCreateRequest(volume string) *atom.EngineCreateRequest {
	return &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			ResolvedVolumeMounts: []container.VolumeMount{{
				Name:     "shared",
				Type:     container.VolumeMountTypeVolume,
				Source:   volume,
				Target:   "/data",
				ReadOnly: true,
				SubPath:  "unit-a",
			}},
		},
	}
}

// wireVolumeRaceContainerCreate sets up the image + container expectations a
// successful Create needs, and asserts the container spec still carries the
// named volume with its mount options intact — pre-creating the volume must
// not disturb what the container asks for.
func (s *PodmanTestSuite) wireVolumeRaceContainerCreate(volume string) {
	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", testImage).
		Return(true, nil)

	specMatcher := mock.MatchedBy(func(spec *specgen.SpecGenerator) bool {
		if len(spec.Volumes) != 1 {
			return false
		}
		vol := spec.Volumes[0]
		return vol.Name == volume &&
			vol.Dest == "/data" &&
			vol.SubPath == "unit-a" &&
			len(vol.Options) == 1 &&
			vol.Options[0] == "ro"
	})

	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", specMatcher).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()
}

// TestCreateEnsuresNamedVolumeBeforeContainer is the primary guard for
// caesium#443: podman's inline named-volume create (inside ContainerCreate) is
// not concurrency-safe, so the engine creates the volume itself first.
func (s *PodmanTestSuite) TestCreateEnsuresNamedVolumeBeforeContainer() {
	const volume = "caesium-shared"

	s.wireVolumeRaceContainerCreate(volume)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, nil).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeCreate", entities.VolumeCreateOptions{Name: volume, IgnoreIfExists: true}).
		Return(nil).
		Once()

	c, err := s.engine.Create(namedVolumeCreateRequest(volume))
	s.Require().NoError(err)
	s.Require().NotNil(c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreateSkipsVolumeCreateWhenVolumeExists() {
	const volume = "caesium-shared"

	s.wireVolumeRaceContainerCreate(volume)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(true, nil).
		Once()

	c, err := s.engine.Create(namedVolumeCreateRequest(volume))
	s.Require().NoError(err)
	s.Require().NotNil(c)
	s.engine.backend.(*mockPodmanBackend).AssertNotCalled(s.T(), "VolumeCreate", mock.Anything)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateToleratesConcurrentNamedVolumeCreate is the race itself: the
// sibling task won the create between this task's existence check and its own
// create. That is the outcome this task wanted, so once the volume verifies as
// present the container create proceeds.
func (s *PodmanTestSuite) TestCreateToleratesConcurrentNamedVolumeCreate() {
	const volume = "caesium-shared"

	s.wireVolumeRaceContainerCreate(volume)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, nil).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeCreate", entities.VolumeCreateOptions{Name: volume, IgnoreIfExists: true}).
		// Verbatim podman remote-bindings shape: the sentinel does not survive
		// the wire, only the server's rendered message does.
		Return(&errorhandling.ErrorModel{
			Message: fmt.Sprintf(
				"adding volume to state: name %q is in use: volume already exists", volume),
			ResponseCode: 500,
		}).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(true, nil).
		Once()

	c, err := s.engine.Create(namedVolumeCreateRequest(volume))
	s.Require().NoError(err)
	s.Require().NotNil(c)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateFailsWhenConcurrentVolumeCannotBeVerified proves the conflict is
// not swallowed on faith: an "already exists" that does not verify is still an
// error, and no container is created.
func (s *PodmanTestSuite) TestCreateFailsWhenConcurrentVolumeCannotBeVerified() {
	const volume = "caesium-shared"

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", testImage).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, nil).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeCreate", entities.VolumeCreateOptions{Name: volume, IgnoreIfExists: true}).
		Return(fmt.Errorf("adding volume to state: name %q is in use: %w", volume, define.ErrVolumeExists)).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, nil).
		Once()

	c, err := s.engine.Create(namedVolumeCreateRequest(volume))
	s.Require().Error(err)
	s.Nil(c)
	s.Contains(err.Error(), "cannot be found")
	s.engine.backend.(*mockPodmanBackend).
		AssertNotCalled(s.T(), "ContainerCreate", mock.Anything)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateFailsWhenVolumeVerificationErrors covers the other verification
// failure: the follow-up existence check itself errors out.
func (s *PodmanTestSuite) TestCreateFailsWhenVolumeVerificationErrors() {
	const volume = "caesium-shared"

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", testImage).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, nil).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeCreate", entities.VolumeCreateOptions{Name: volume, IgnoreIfExists: true}).
		Return(define.ErrVolumeExists).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, fmt.Errorf("connection refused")).
		Once()

	c, err := s.engine.Create(namedVolumeCreateRequest(volume))
	s.Require().Error(err)
	s.Nil(c)
	s.Contains(err.Error(), "connection refused")
	s.engine.backend.(*mockPodmanBackend).
		AssertNotCalled(s.T(), "ContainerCreate", mock.Anything)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateSurfacesUnrelatedVolumeCreateError proves the tolerance is narrow:
// anything that is not "volume already exists" still fails the task.
func (s *PodmanTestSuite) TestCreateSurfacesUnrelatedVolumeCreateError() {
	const volume = "caesium-shared"

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", testImage).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, nil).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeCreate", entities.VolumeCreateOptions{Name: volume, IgnoreIfExists: true}).
		Return(fmt.Errorf("no space left on device")).
		Once()

	c, err := s.engine.Create(namedVolumeCreateRequest(volume))
	s.Require().Error(err)
	s.Nil(c)
	s.Contains(err.Error(), "no space left on device")
	s.engine.backend.(*mockPodmanBackend).
		AssertNotCalled(s.T(), "ContainerCreate", mock.Anything)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

func (s *PodmanTestSuite) TestCreateSurfacesVolumeExistenceCheckError() {
	const volume = "caesium-shared"

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", testImage).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(false, fmt.Errorf("connection refused")).
		Once()

	c, err := s.engine.Create(namedVolumeCreateRequest(volume))
	s.Require().Error(err)
	s.Nil(c)
	s.Contains(err.Error(), "connection refused")
	s.engine.backend.(*mockPodmanBackend).
		AssertNotCalled(s.T(), "VolumeCreate", mock.Anything)
	s.engine.backend.(*mockPodmanBackend).
		AssertNotCalled(s.T(), "ContainerCreate", mock.Anything)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateEnsuresEachNamedVolumeOnce guards against a step that mounts the
// same volume at several targets (or several subPaths) issuing a redundant
// existence check per mount.
func (s *PodmanTestSuite) TestCreateEnsuresEachNamedVolumeOnce() {
	const volume = "caesium-shared"

	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			ResolvedVolumeMounts: []container.VolumeMount{
				{Name: "a", Type: container.VolumeMountTypeVolume, Source: volume, Target: "/a", SubPath: "a"},
				{Name: "b", Type: container.VolumeMountTypeVolume, Source: volume, Target: "/b", SubPath: "b"},
				{Name: "c", Type: container.VolumeMountTypeVolume, Source: "other", Target: "/c"},
			},
		},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", testImage).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", volume).
		Return(true, nil).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("VolumeExists", "other").
		Return(true, nil).
		Once()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockPodmanBackend).
		AssertNumberOfCalls(s.T(), "VolumeExists", 2)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestCreateIgnoresNonVolumeMountsWhenEnsuringVolumes keeps bind and tmpfs
// mounts out of the volume-ensure path entirely.
func (s *PodmanTestSuite) TestCreateIgnoresNonVolumeMountsWhenEnsuringVolumes() {
	req := &atom.EngineCreateRequest{
		Name:    testContainerName,
		Image:   testImage,
		Command: []string{"run"},
		Spec: container.Spec{
			Mounts: []container.Mount{{
				Type:   container.MountTypeBind,
				Source: "/host",
				Target: "/data",
			}},
			ResolvedVolumeMounts: []container.VolumeMount{{
				Name:   "scratch",
				Type:   container.VolumeMountTypeTmpfs,
				Target: "/scratch",
			}},
		},
	}

	s.engine.backend.(*mockPodmanBackend).
		On("ImageExists", testImage).
		Return(true, nil)
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerCreate", mock.AnythingOfType("*specgen.SpecGenerator")).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerStart", testAtomID).
		Return()
	s.engine.backend.(*mockPodmanBackend).
		On("ContainerInspect", testAtomID).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockPodmanBackend).
		AssertNotCalled(s.T(), "VolumeExists", mock.Anything)
	s.engine.backend.(*mockPodmanBackend).AssertExpectations(s.T())
}

// TestIsVolumeExistsError pins the classifier that decides which create
// failure is a lost race and which is a real fault. Both podman message
// shapes must be recognised; nothing else may be.
func (s *PodmanTestSuite) TestIsVolumeExistsError() {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "sentinel", err: define.ErrVolumeExists, want: true},
		{
			name: "wrapped sentinel",
			err:  fmt.Errorf("creating named volume %q: %w", "vol", define.ErrVolumeExists),
			want: true,
		},
		{
			name: "remote state-race message",
			err: &errorhandling.ErrorModel{
				Message: `creating named volume "vol": adding volume to state: name "vol" is in use: volume already exists`,
			},
			want: true,
		},
		{
			name: "remote pre-check message",
			err: &errorhandling.ErrorModel{
				Message: "volume with name vol already exists: volume already exists",
			},
			want: true,
		},
		{
			name: "unrelated failure",
			err:  fmt.Errorf("no space left on device"),
			want: false,
		},
		{
			name: "container name conflict is not a volume conflict",
			err:  fmt.Errorf("container already exists"),
			want: false,
		},
	} {
		s.Run(tc.name, func() {
			s.Equal(tc.want, isVolumeExistsError(tc.err))
		})
	}
}

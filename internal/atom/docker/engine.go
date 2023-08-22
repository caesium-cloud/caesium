package docker

import (
	"context"
	"io"
	"io/ioutil"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Engine defines the interface for treating the
// Docker API as a atom.Engine.
type Engine interface {
	atom.Engine
}

type dockerEngine struct {
	ctx     context.Context
	backend dockerBackend
}

// NewEngine creates a new instance of docker.Engine
// for interacting with docker.Atoms.
func NewEngine(ctx context.Context) Engine {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		panic(err)
	}

	return &dockerEngine{
		ctx:     ctx,
		backend: cli,
	}
}

// Get a Caesium Docker container and its corresponding metadata.
func (e *dockerEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	metadata, err := e.backend.ContainerInspect(e.ctx, req.ID)
	if err != nil {
		return nil, err
	}

	return &Atom{metadata: metadata}, nil
}

// List all of Caesium's Docker containers. Note that List is a
// relatively heavy request because it does not only a LIST request
// to the Docker API, but also an INSPECT for each of the containers
// because the default LIST response does not include enough data.
// This should be fine since DockerEngine.List should only really
// be needed on start-up or in the event of a crash.
func (e *dockerEngine) List(req *atom.EngineListRequest) ([]atom.Atom, error) {
	opts := types.ContainerListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.KeyValuePair{Key: atom.Label}),
	}

	if !req.Since.IsZero() {
		opts.Since = req.Since.Format(time.RFC3339Nano)
	}

	if !req.Before.IsZero() {
		opts.Before = req.Before.Format(time.RFC3339Nano)
	}

	containers, err := e.backend.ContainerList(e.ctx, opts)
	if err != nil {
		return nil, err
	}

	atoms := make([]atom.Atom, len(containers))

	for i, container := range containers {
		atoms[i], err = e.Get(&atom.EngineGetRequest{ID: container.ID})
		if err != nil {
			return nil, err
		}
	}

	return atoms, nil
}

// Create and start a Caesium Docker container. Caesium has
// no concept of a creating a Atom without it also starting,
// so we encapsulate both functions inside docker.Atom.Create.
func (e *dockerEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	log.Info("pulling docker image", "image", req.Image)

	r, err := e.backend.ImagePull(e.ctx, req.Image, types.ImagePullOptions{})
	if err != nil {
		return nil, err
	}

	if _, err = ioutil.ReadAll(r); err != nil {
		return nil, err
	}

	log.Info("docker image pulled", "image", req.Image)

	cfg := &container.Config{
		Image: req.Image,
		Cmd:   req.Command,
	}

	log.Info("creating docker container", "image", req.Image)

	created, err := e.backend.ContainerCreate(e.ctx, cfg, nil, nil, nil, req.Name)
	if err != nil {
		return nil, err
	}

	opts := types.ContainerStartOptions{}

	log.Info(
		"starting docker container",
		"image", req.Image,
		"cmd", req.Command,
		"id", created.ID,
	)

	if err = e.backend.ContainerStart(e.ctx, created.ID, opts); err != nil {
		return nil, err
	}

	return e.Get(&atom.EngineGetRequest{ID: created.ID})
}

// Stop and remove a Caesium Docker container. Since Caesium
// doesn't distinguish between a "stopped" and a "removed"
// container, we encapsulate both functions inside
// docker.Atom.Stop.
func (e *dockerEngine) Stop(req *atom.EngineStopRequest) error {
	log.Info("stopping docker container", "id", req.ID)

	timeout := int(req.Timeout.Seconds())
	if err := e.backend.ContainerStop(e.ctx, req.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return err
	}

	log.Info("removing docker container", "id", req.ID)

	opts := types.ContainerRemoveOptions{
		Force:         req.Force,
		RemoveVolumes: true,
	}

	return e.backend.ContainerRemove(e.ctx, req.ID, opts)
}

// Logs streams the log output from a Caesium Docker container
// based on the request input.
func (e *dockerEngine) Logs(req *atom.EngineLogsRequest) (io.ReadCloser, error) {
	opts := types.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Timestamps: true,
		Follow:     true,
	}

	if !req.Since.IsZero() {
		opts.Since = req.Since.Format(time.RFC3339Nano)
	}

	return e.backend.ContainerLogs(e.ctx, req.ID, opts)
}

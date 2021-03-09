package docker

import (
	"context"
	"io"
	"time"

	"github.com/caesium-dev/caesium/internal/capsule"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// Engine defines the interface for treating the
// Docker API as a capsule.Engine.
type Engine interface {
	capsule.Engine
}

type dockerEngine struct {
	ctx     context.Context
	backend dockerBackend
}

// NewEngine creates a new instance of docker.Engine
// for interacting with docker.Capsules.
func NewEngine(ctx context.Context) Engine {
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	return &dockerEngine{
		ctx:     ctx,
		backend: cli,
	}
}

// Get a Caesium Docker container and its corresponding metadata.
func (e *dockerEngine) Get(req *capsule.EngineGetRequest) (capsule.Capsule, error) {
	metadata, err := e.backend.ContainerInspect(e.ctx, req.ID)
	if err != nil {
		return nil, err
	}

	return &Capsule{metadata: metadata}, nil
}

// List all of Caesium's docker containers. Note that List is a
// relatively heavy request because it does not only a LIST request
// to the Docker API, but also an INSPECT for each of the containers
// because the default LIST response does not include enough data.
// This should be fine since DockerEngine.List should only really
// be needed on start-up or in the event of a crash.
func (e *dockerEngine) List(req *capsule.EngineListRequest) ([]capsule.Capsule, error) {
	opts := types.ContainerListOptions{
		All:     true,
		Filters: filters.NewArgs(filters.KeyValuePair{Key: label}),
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

	capsules := make([]capsule.Capsule, len(containers))

	for i, container := range containers {
		capsules[i], err = e.Get(&capsule.EngineGetRequest{ID: container.ID})
		if err != nil {
			return nil, err
		}
	}

	return capsules, nil
}

// Create and start a Caesium Docker container. Caesium has
// no concept of a creating a Capsule without it also starting,
// so we encapsulate both functions inside docker.Capsule.Create.
func (e *dockerEngine) Create(req *capsule.EngineCreateRequest) (capsule.Capsule, error) {
	cfg := &container.Config{Image: req.Image, Cmd: req.Command}

	created, err := e.backend.ContainerCreate(e.ctx, cfg, nil, nil, nil, req.Name)
	if err != nil {
		return nil, err
	}

	opts := types.ContainerStartOptions{}

	if err = e.backend.ContainerStart(e.ctx, created.ID, opts); err != nil {
		return nil, err
	}

	return e.Get(&capsule.EngineGetRequest{ID: created.ID})
}

// Stop and remove a Caesium Docker container. Since Caesium
// doesn't distinguish between a "stopped" and a "removed"
// container, we encapsulate both functions inside
// docker.Capsule.Stop.
func (e *dockerEngine) Stop(req *capsule.EngineStopRequest) error {
	if err := e.backend.ContainerStop(e.ctx, req.ID, &req.Timeout); err != nil {
		return err
	}

	opts := types.ContainerRemoveOptions{
		Force:         req.Force,
		RemoveVolumes: true,
		RemoveLinks:   true,
	}

	return e.backend.ContainerRemove(e.ctx, req.ID, opts)
}

// Logs streams the log output from a Caesium Docker container
// based on the request input.
func (e *dockerEngine) Logs(req *capsule.EngineLogsRequest) (io.ReadCloser, error) {
	opts := types.ContainerLogsOptions{
		ShowStdout: req.Stdout,
		ShowStderr: req.Stderr,
		Since:      req.Since.Format(time.RFC3339Nano),
		Until:      req.Until.Format(time.RFC3339Nano),
		Timestamps: true,
		Follow:     true,
	}

	return e.backend.ContainerLogs(e.ctx, req.ID, opts)
}

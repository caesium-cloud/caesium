package docker

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/log"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
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
	opts := dockercontainer.ListOptions{
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

	r, err := e.backend.ImagePull(e.ctx, req.Image, image.PullOptions{})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := r.Close(); err != nil {
			log.Error("close docker pull reader", "error", err)
		}
	}()

	if _, err = io.ReadAll(r); err != nil {
		return nil, err
	}

	log.Info("docker image pulled", "image", req.Image)

	cfg := &dockercontainer.Config{
		Image: req.Image,
		Cmd:   req.Command,
		Env:   formatEnv(req.Spec.Env),
	}
	if req.Spec.WorkDir != "" {
		cfg.WorkingDir = req.Spec.WorkDir
	}

	var hostCfg *dockercontainer.HostConfig
	if mounts := convertMounts(req.Spec.Mounts); len(mounts) > 0 {
		hostCfg = &dockercontainer.HostConfig{Mounts: mounts}
	}

	log.Info("creating docker container", "image", req.Image)

	created, err := e.backend.ContainerCreate(e.ctx, cfg, hostCfg, nil, nil, req.Name)
	if err != nil {
		return nil, err
	}

	opts := dockercontainer.StartOptions{}

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
	if err := e.backend.ContainerStop(e.ctx, req.ID, dockercontainer.StopOptions{Timeout: &timeout}); err != nil {
		return err
	}

	// TODO: implement garbage collection policy
	// log.Info("removing docker container", "id", req.ID)
	//
	// opts := dockercontainer.RemoveOptions{
	// 	Force:         req.Force,
	// 	RemoveVolumes: true,
	// }
	//
	// return e.backend.ContainerRemove(e.ctx, req.ID, opts)
	return nil
}

// Logs streams the log output from a Caesium Docker container
// based on the request input.
func (e *dockerEngine) Logs(req *atom.EngineLogsRequest) (io.ReadCloser, error) {
	opts := dockercontainer.LogsOptions{
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

func formatEnv(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, fmt.Sprintf("%s=%s", k, values[k]))
	}
	return env
}

func convertMounts(specMounts []container.Mount) []mount.Mount {
	if len(specMounts) == 0 {
		return nil
	}
	result := make([]mount.Mount, 0, len(specMounts))
	for _, mnt := range specMounts {
		if mnt.Source == "" || mnt.Target == "" {
			continue
		}
		switch mnt.Type {
		case container.MountTypeBind, "":
			result = append(result, mount.Mount{
				Type:     mount.TypeBind,
				Source:   mnt.Source,
				Target:   mnt.Target,
				ReadOnly: mnt.ReadOnly,
			})
		}
	}
	return result
}

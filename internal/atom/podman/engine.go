package podman

import (
	"context"
	"io"
	"io/ioutil"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/containers/podman/v4/pkg/bindings"
	"github.com/containers/podman/v4/pkg/bindings/containers"
	"github.com/containers/podman/v4/pkg/bindings/images"
	"github.com/containers/podman/v4/pkg/specgen"
)

type Engine interface {
	atom.Engine
}

type podmanEngine struct {
	ctx     context.Context
	backend podmanBackend
}

func NewEngine(ctx context.Context) Engine {
	ctx, err := bindings.NewConnection(
		context.Background(),
		env.Variables().PodmanURI,
	)
	if err != nil {
		panic(err)
	}

	return &podmanEngine{
		ctx:     ctx,
		backend: &podmanClient{ctx: ctx},
	}
}

func (e *podmanEngine) Get(req *atom.EngineGetRequest) (atom.Atom, error) {
	metadata, err := e.backend.ContainerInspect(req.ID)
	if err != nil {
		return nil, err
	}

	return &Atom{metadata: metadata}, nil
}

func (e *podmanEngine) List(req *atom.EngineListRequest) ([]atom.Atom, error) {
	filters := map[string][]string{}

	if !req.Since.IsZero() {
		filters["since"] = []string{req.Since.Format(time.RFC3339)}
	}
	if !req.Before.IsZero() {
		filters["before"] = []string{req.Before.Format(time.RFC3339)}
	}

	containers, err := e.backend.ContainerList(filters, true)
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

func (e *podmanEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	log.Info("pulling podman image", "image", req.Image)

	r, err := e.backend.ImagePull(req.Image, &images.PullOptions{})
	if err != nil {
		return nil, err
	}

	if _, err = ioutil.ReadAll(r); err != nil {
		return nil, err
	}

	log.Info("podman image pulled", "image", req.Image)

	spec := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			RawImageName: req.Image,
			Name:         req.Name,
			Command:      req.Command,
		},
	}

	created, err := e.backend.ContainerCreate(spec)
	if err != nil {
		return nil, err
	}

	log.Info(
		"starting podman container",
		"image", req.Image,
		"cmd", req.Command,
		"id", created.ID,
	)

	if err = e.backend.ContainerStart(created.ID); err != nil {
		return nil, err
	}

	return e.Get(&atom.EngineGetRequest{ID: created.ID})
}

func (e *podmanEngine) Stop(req *atom.EngineStopRequest) error {
	log.Info("stopping podman container", "id", req.ID)

	if err := e.backend.ContainerStop(req.ID, &req.Timeout); err != nil {
		return err
	}

	log.Info("removing podman container", "id", req.ID)

	removeVolumes := true

	return e.backend.ContainerRemove(req.ID, &req.Force, &removeVolumes)
}

func (e *podmanEngine) Logs(req *atom.EngineLogsRequest) (io.ReadCloser, error) {
	var (
		stdout     = true
		stderr     = true
		timestamps = true
		follow     = true
	)

	opts := containers.LogOptions{
		Stdout:     &stdout,
		Stderr:     &stderr,
		Timestamps: &timestamps,
		Follow:     &follow,
	}

	return e.backend.ContainerLogs(req.ID, opts)
}

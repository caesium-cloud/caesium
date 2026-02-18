package podman

import (
	"context"
	"io"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/containers/podman/v5/pkg/specgen"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type Engine interface {
	atom.Engine
}

type podmanEngine struct {
	ctx     context.Context
	backend podmanBackend
}

func NewEngine(ctx context.Context) Engine {
	conn, err := bindings.NewConnection(
		ctx,
		env.Variables().PodmanURI,
	)
	if err != nil {
		panic(err)
	}

	return &podmanEngine{
		ctx:     conn,
		backend: &podmanClient{ctx: conn},
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
	defer func() {
		if err := r.Close(); err != nil {
			log.Error("close podman pull reader", "error", err)
		}
	}()

	if _, err = io.ReadAll(r); err != nil {
		return nil, err
	}

	log.Info("podman image pulled", "image", req.Image)

	spec := &specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Name:    req.Name,
			Command: req.Command,
			Env:     req.Spec.Env,
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: req.Image,
		},
	}
	if req.Spec.WorkDir != "" {
		spec.WorkDir = req.Spec.WorkDir
	}
	if mounts := convertPodmanMounts(req.Spec.Mounts); len(mounts) > 0 {
		spec.Mounts = mounts
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

	// TODO: implement garbage collection policy
	// log.Info("removing podman container", "id", req.ID)
	//
	// removeVolumes := true
	//
	// return e.backend.ContainerRemove(req.ID, &req.Force, &removeVolumes)
	return nil
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

func convertPodmanMounts(specMounts []container.Mount) []specs.Mount {
	if len(specMounts) == 0 {
		return nil
	}
	result := make([]specs.Mount, 0, len(specMounts))
	for _, mnt := range specMounts {
		if mnt.Source == "" || mnt.Target == "" {
			continue
		}
		switch mnt.Type {
		case container.MountTypeBind, "":
			mount := specs.Mount{
				Type:        string(container.MountTypeBind),
				Source:      mnt.Source,
				Destination: mnt.Target,
			}
			if mnt.ReadOnly {
				mount.Options = append(mount.Options, "ro")
			}
			result = append(result, mount)
		}
	}
	return result
}

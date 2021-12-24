package podman

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/containers/podman/v2/libpod/define"
	"github.com/containers/podman/v2/pkg/bindings/containers"
	"github.com/containers/podman/v2/pkg/bindings/images"
	"github.com/containers/podman/v2/pkg/domain/entities"
	"github.com/containers/podman/v2/pkg/specgen"
)

var (
	stateMap = map[string]atom.State{
		"created":    atom.Created,
		"running":    atom.Running,
		"paused":     atom.Invalid, // a container should never be paused
		"restarting": atom.Invalid, // a container should never be restarting
		"removing":   atom.Stopping,
		"exited":     atom.Stopped,
		"dead":       atom.Stopped,
	}
	resultMap = map[int]atom.Result{
		0:   atom.Success,
		1:   atom.Failure,
		125: atom.StartupFailure,
		126: atom.StartupFailure,
		127: atom.StartupFailure,
		137: atom.Killed,
		143: atom.Terminated,
	}
)

type podmanBackend interface {
	ContainerInspect(string) (*define.InspectContainerData, error)
	ContainerList(map[string][]string, bool) ([]entities.ListContainer, error)
	ContainerCreate(*specgen.SpecGenerator) (entities.ContainerCreateResponse, error)
	ContainerStart(string) error
	ContainerStop(string, *time.Duration) error
	ContainerRemove(string, *bool, *bool) error
	ContainerLogs(string, containers.LogOptions) (io.ReadCloser, error)
	ImagePull(string, entities.ImagePullOptions) (io.ReadCloser, error)
}

type podmanClient struct {
	ctx context.Context
}

func (cli *podmanClient) ContainerInspect(id string) (*define.InspectContainerData, error) {
	return containers.Inspect(cli.ctx, id, nil)
}

func (cli *podmanClient) ContainerList(filters map[string][]string, all bool) ([]entities.ListContainer, error) {
	return containers.List(cli.ctx, filters, &all, nil, nil, nil, nil)
}

func (cli *podmanClient) ContainerCreate(spec *specgen.SpecGenerator) (entities.ContainerCreateResponse, error) {
	return containers.CreateWithSpec(cli.ctx, spec)
}

func (cli *podmanClient) ContainerStart(id string) error {
	return containers.Start(cli.ctx, id, nil)
}

func (cli *podmanClient) ContainerStop(id string, timeout *time.Duration) error {
	var t uint

	if timeout != nil {
		t = uint(timeout.Seconds())
	}
	return containers.Stop(cli.ctx, id, &t)
}

func (cli *podmanClient) ContainerRemove(id string, force *bool, removeVolumes *bool) error {
	return containers.Remove(cli.ctx, id, force, removeVolumes)
}

func (cli *podmanClient) ContainerLogs(id string, opts containers.LogOptions) (io.ReadCloser, error) {
	var (
		pr, pw = io.Pipe()
		stdout chan (string)
		stderr chan (string)
	)

	err := containers.Logs(
		cli.ctx,
		id,
		containers.LogOptions{},
		stdout,
		stderr,
	)

	if err != nil {
		return nil, err
	}

	go func() {
		for {
			select {
			case line, ok := <-stdout:
				pw.Write([]byte(line))
				if !ok {
					stdout = nil
				}
			case line, ok := <-stderr:
				pw.Write([]byte(line))
				if !ok {
					stderr = nil
				}
			}

			if stdout == nil && stderr != nil {
				return
			}
		}
	}()

	return pr, nil
}

func (cli *podmanClient) ImagePull(image string, opts entities.ImagePullOptions) (io.ReadCloser, error) {
	if _, err := images.Pull(cli.ctx, image, opts); err != nil {
		return nil, err
	}

	return os.Stderr, nil
}

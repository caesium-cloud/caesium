package docker

import (
	"context"
	"io"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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

type dockerBackend interface {
	ContainerInspect(context.Context, string) (container.InspectResponse, error)
	ContainerList(context.Context, container.ListOptions) ([]container.Summary, error)
	ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerRemove(context.Context, string, container.RemoveOptions) error
	ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error)
	ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error)
}

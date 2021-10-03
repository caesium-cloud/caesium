package docker

import (
	"context"
	"io"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	networktypes "github.com/docker/docker/api/types/network"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

type dockerBackend interface {
	ContainerInspect(context.Context, string) (types.ContainerJSON, error)
	ContainerList(context.Context, types.ContainerListOptions) ([]types.Container, error)
	ContainerCreate(context.Context, *containertypes.Config, *containertypes.HostConfig, *networktypes.NetworkingConfig, *specs.Platform, string) (containertypes.ContainerCreateCreatedBody, error)
	ContainerStart(context.Context, string, types.ContainerStartOptions) error
	ContainerStop(context.Context, string, *time.Duration) error
	ContainerRemove(context.Context, string, types.ContainerRemoveOptions) error
	ContainerLogs(context.Context, string, types.ContainerLogsOptions) (io.ReadCloser, error)
	ImagePull(context.Context, string, types.ImagePullOptions) (io.ReadCloser, error)
}

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

package docker

import (
	"context"
	"io"
	"time"

	"github.com/caesium-dev/caesium/internal/capsule"
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
}

var (
	stateMap = map[string]capsule.State{
		"created":    capsule.Created,
		"running":    capsule.Running,
		"paused":     capsule.Invalid, // a container should never be paused
		"restarting": capsule.Invalid, // a container should never be restarting
		"removing":   capsule.Stopping,
		"exited":     capsule.Stopped,
		"dead":       capsule.Stopped,
	}
	resultMap = map[int]capsule.Result{
		0:   capsule.Success,
		1:   capsule.Failure,
		125: capsule.StartupFailure,
		126: capsule.StartupFailure,
		127: capsule.StartupFailure,
		137: capsule.Killed,
		143: capsule.Terminated,
	}
)

const label = "dev.caesium"

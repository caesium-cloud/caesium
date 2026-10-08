package docker

import (
	"context"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type dockerBackend interface {
	ContainerStatsOneShot(context.Context, string) (container.StatsResponseReader, error)
	ContainerInspect(context.Context, string) (container.InspectResponse, error)
	ContainerList(context.Context, container.ListOptions) ([]container.Summary, error)
	ContainerCreate(context.Context, *container.Config, *container.HostConfig, *network.NetworkingConfig, *ocispec.Platform, string) (container.CreateResponse, error)
	ContainerStart(context.Context, string, container.StartOptions) error
	ContainerWait(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerStop(context.Context, string, container.StopOptions) error
	ContainerRemove(context.Context, string, container.RemoveOptions) error
	ContainerLogs(context.Context, string, container.LogsOptions) (io.ReadCloser, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (image.InspectResponse, error)
	ImagePull(context.Context, string, image.PullOptions) (io.ReadCloser, error)
	// ClientVersion returns the negotiated Docker Engine API version. It is
	// only meaningful after at least one request has been made against the
	// backend (API version negotiation happens lazily on the first call), so
	// callers that need it before any other request should trigger one first.
	ClientVersion() string
}

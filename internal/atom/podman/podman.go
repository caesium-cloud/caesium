package podman

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/containers/podman/v5/pkg/domain/entities"
	"github.com/containers/podman/v5/pkg/specgen"
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
	ContainerWait(string, context.Context) error
	ContainerStop(string, *time.Duration) error
	ContainerRemove(string, *bool, *bool) error
	ContainerLogs(string, containers.LogOptions) (io.ReadCloser, error)
	ImagePull(string, *images.PullOptions) (io.ReadCloser, error)
}

type podmanClient struct {
	ctx context.Context
}

func (cli *podmanClient) ContainerInspect(id string) (*define.InspectContainerData, error) {
	return containers.Inspect(cli.ctx, id, nil)
}

func (cli *podmanClient) ContainerList(filters map[string][]string, all bool) ([]entities.ListContainer, error) {
	return containers.List(cli.ctx, &containers.ListOptions{All: &all, Filters: filters})
}

func (cli *podmanClient) ContainerCreate(spec *specgen.SpecGenerator) (entities.ContainerCreateResponse, error) {
	return containers.CreateWithSpec(cli.ctx, spec, nil)
}

func (cli *podmanClient) ContainerStart(id string) error {
	return containers.Start(cli.ctx, id, nil)
}

func (cli *podmanClient) ContainerWait(id string, _ context.Context) error {
	// Always use cli.ctx (the Podman connection context) rather than the
	// caller-supplied context. cli.ctx was derived from the task context via
	// bindings.NewConnection, so it already carries the Podman HTTP client AND
	// is cancelled when the task context is cancelled/times out. Replacing it
	// with the raw task context loses the embedded client and causes every
	// containers.Wait call to return "Client not set in context".
	_, err := containers.Wait(cli.ctx, id, nil)
	return err
}

func (cli *podmanClient) ContainerStop(id string, timeout *time.Duration) error {
	var t uint

	if timeout != nil {
		t = uint(timeout.Seconds())
	}
	return containers.Stop(cli.ctx, id, &containers.StopOptions{Timeout: &t})
}

func (cli *podmanClient) ContainerRemove(id string, force *bool, removeVolumes *bool) error {
	_, err := containers.Remove(cli.ctx, id, &containers.RemoveOptions{
		Force:   force,
		Volumes: removeVolumes,
	})
	return err
}

func (cli *podmanClient) ContainerLogs(id string, opts containers.LogOptions) (io.ReadCloser, error) {
	pr, pw := io.Pipe()
	// Channels must be non-nil: a nil channel in a select case is permanently
	// disabled, so the goroutine below would block forever and nothing would
	// ever write to pw, causing CaptureMarkers to hang. Unbuffered is fine
	// since containers.Logs writes asynchronously via its own goroutines.
	stdoutCh := make(chan string)
	stderrCh := make(chan string)

	if err := containers.Logs(cli.ctx, id, &opts, stdoutCh, stderrCh); err != nil {
		return nil, err
	}

	go func() {
		defer pw.Close()
		for stdoutCh != nil || stderrCh != nil {
			select {
			case line, ok := <-stdoutCh:
				if !ok {
					stdoutCh = nil
					continue
				}
				if _, err := pw.Write([]byte(line + "\n")); err != nil {
					pw.CloseWithError(err)
					return
				}
			case line, ok := <-stderrCh:
				if !ok {
					stderrCh = nil
					continue
				}
				if _, err := pw.Write([]byte(line + "\n")); err != nil {
					pw.CloseWithError(err)
					return
				}
			}
		}
	}()

	return pr, nil
}

func (cli *podmanClient) ImagePull(image string, opts *images.PullOptions) (io.ReadCloser, error) {
	if _, err := images.Pull(cli.ctx, image, opts); err != nil {
		return nil, err
	}

	return io.NopCloser(strings.NewReader("")), nil
}

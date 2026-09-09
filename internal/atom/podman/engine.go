package podman

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	"github.com/containers/podman/v5/pkg/domain/entities"
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
	if err := e.ensureImagePresent(req.Image); err != nil {
		return nil, err
	}

	spec := &specgen.SpecGenerator{
		Name:    req.Name,
		Command: req.Command,
		Env:     req.Spec.Env,
		Image:   req.Image,
		// HealthLogDestination carries no `omitempty` in specgen (upstream
		// defers that to v6.0), so leaving it unset serializes as "" rather
		// than omitting it. Podman servers validate the field and stat("")
		// fails, rejecting every create with "HealthCheck Log '' destination
		// error". Send the documented default explicitly.
		HealthLogDestination: define.DefaultHealthCheckLocalDestination,
	}
	if req.Spec.WorkDir != "" {
		spec.WorkDir = req.Spec.WorkDir
	}
	if mounts, volumes := convertPodmanMounts(req.Spec.Mounts, req.Spec.ResolvedVolumeMounts); len(mounts) > 0 || len(volumes) > 0 {
		spec.Mounts = mounts
		spec.Volumes = volumes
	}

	if err := e.ensureNamedVolumes(spec.Volumes); err != nil {
		return nil, err
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

func (e *podmanEngine) ensureImagePresent(imageRef string) error {
	if imageRef != "" {
		exists, err := e.backend.ImageExists(imageRef)
		if err != nil {
			return err
		}
		if exists {
			log.Info("podman image already present", "image", imageRef)
			return nil
		}
	}

	log.Info("pulling podman image", "image", imageRef)

	r, err := e.backend.ImagePull(imageRef, &images.PullOptions{})
	if err != nil {
		return err
	}
	defer func() {
		if err := r.Close(); err != nil {
			log.Error("close podman pull reader", "error", err)
		}
	}()

	if _, err = io.ReadAll(r); err != nil {
		return err
	}

	log.Info("podman image pulled", "image", imageRef)
	return nil
}

func (e *podmanEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	if err := e.backend.ContainerWait(req.ID, req.Context); err != nil {
		return nil, err
	}
	return e.Get(&atom.EngineGetRequest{ID: req.ID})
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

func convertPodmanMounts(specMounts []container.Mount, resolvedMounts []container.VolumeMount) ([]specs.Mount, []*specgen.NamedVolume) {
	if len(specMounts) == 0 && len(resolvedMounts) == 0 {
		return nil, nil
	}
	result := make([]specs.Mount, 0, len(specMounts)+len(resolvedMounts))
	volumes := make([]*specgen.NamedVolume, 0)
	for _, mnt := range specMounts {
		if mnt.Target == "" {
			continue
		}
		switch mnt.Type {
		case container.MountTypeBind, "":
			if mnt.Source == "" {
				continue
			}
			mount := specs.Mount{
				Type:        string(container.MountTypeBind),
				Source:      mnt.Source,
				Destination: mnt.Target,
			}
			if mnt.ReadOnly {
				mount.Options = append(mount.Options, "ro")
			}
			result = append(result, mount)
		case container.MountTypeVolume:
			if mnt.Source == "" {
				continue
			}
			options := []string{}
			if mnt.ReadOnly {
				options = append(options, "ro")
			}
			volumes = append(volumes, &specgen.NamedVolume{
				Name:    mnt.Source,
				Dest:    mnt.Target,
				Options: options,
			})
		case container.MountTypeTmpfs:
			mount := specs.Mount{
				Type:        string(container.MountTypeTmpfs),
				Source:      string(container.MountTypeTmpfs),
				Destination: mnt.Target,
			}
			if mnt.ReadOnly {
				mount.Options = append(mount.Options, "ro")
			}
			result = append(result, mount)
		}
	}
	for _, mnt := range resolvedMounts {
		if mnt.Target == "" {
			continue
		}
		switch mnt.Type {
		case container.VolumeMountTypeBind:
			if mnt.Source == "" {
				continue
			}
			mount := specs.Mount{
				Type:        string(container.MountTypeBind),
				Source:      mnt.Source,
				Destination: mnt.Target,
			}
			if mnt.ReadOnly {
				mount.Options = append(mount.Options, "ro")
			}
			result = append(result, mount)
		case container.VolumeMountTypeVolume:
			if mnt.Source == "" {
				continue
			}
			options := []string{}
			if mnt.ReadOnly {
				options = append(options, "ro")
			}
			volumes = append(volumes, &specgen.NamedVolume{
				Name:    mnt.Source,
				Dest:    mnt.Target,
				Options: options,
				SubPath: mnt.SubPath,
			})
		case container.VolumeMountTypeTmpfs:
			mount := specs.Mount{
				Type:        string(container.MountTypeTmpfs),
				Source:      string(container.MountTypeTmpfs),
				Destination: mnt.Target,
			}
			if mnt.ReadOnly {
				mount.Options = append(mount.Options, "ro")
			}
			if mnt.Tmpfs != nil {
				if mnt.Tmpfs.SizeBytes > 0 {
					mount.Options = append(mount.Options, "size="+strconv.FormatInt(mnt.Tmpfs.SizeBytes, 10))
				}
				if mnt.Tmpfs.Mode != nil {
					mount.Options = append(mount.Options, "mode="+strconv.FormatInt(int64(*mnt.Tmpfs.Mode), 8))
				}
			}
			result = append(result, mount)
		}
	}
	return result, volumes
}

// ensureNamedVolumes creates every explicitly named volume a container spec
// mounts, before that container is created.
//
// Podman's own container-create path creates a missing named volume inline,
// but that create is not concurrency-safe: its "does the volume already
// exist?" check and the state write that registers the volume are not atomic,
// so two sibling tasks that first-mount the same shared volume can both find
// it absent and race. The loser's ENTIRE container create then fails with
//
//	creating named volume "x": adding volume to state: name "x" is in use: volume already exists
//
// and the task fails even though the only thing that happened is that its
// sibling created the volume it wanted (caesium#443). Docker has no such
// problem — its daemon treats a named-volume create during container create as
// idempotent — which is why this helper has no counterpart in
// internal/atom/docker.
//
// Hoisting the create out of ContainerCreate turns an unrecoverable container
// failure into a volume create the engine can reason about: by the time the
// spec reaches ContainerCreate the volume exists, so podman never attempts the
// racy inline create at all. Mount and volume options are left untouched — the
// volume is created with podman's defaults, exactly what podman's own inline
// create would have produced for a named (non-anonymous) volume — and only the
// volume's existence is ensured here.
func (e *podmanEngine) ensureNamedVolumes(vols []*specgen.NamedVolume) error {
	seen := make(map[string]struct{}, len(vols))
	for _, vol := range vols {
		if vol == nil || vol.Name == "" {
			continue
		}
		if _, ok := seen[vol.Name]; ok {
			continue
		}
		seen[vol.Name] = struct{}{}
		if err := e.ensureNamedVolume(vol.Name); err != nil {
			return err
		}
	}
	return nil
}

// ensureNamedVolume makes one named volume exist. A concurrent winner is an
// acceptable outcome — it produced the very volume this call wanted — but it
// is accepted only after re-verifying that the volume really is there, so a
// misreported conflict can never be mistaken for a usable volume. Every other
// error is returned unchanged.
func (e *podmanEngine) ensureNamedVolume(name string) error {
	exists, err := e.backend.VolumeExists(name)
	if err != nil {
		return fmt.Errorf("check podman volume %q: %w", name, err)
	}
	if exists {
		return nil
	}

	log.Info("creating podman named volume", "volume", name)

	// IgnoreIfExists closes the window podman itself can close (a volume that
	// already exists when the request lands); the conflict branch below closes
	// the one it cannot (a concurrent writer landing between podman's own
	// existence check and its state write).
	createErr := e.backend.VolumeCreate(entities.VolumeCreateOptions{
		Name:           name,
		IgnoreIfExists: true,
	})
	if createErr == nil {
		return nil
	}
	if !isVolumeExistsError(createErr) {
		return fmt.Errorf("create podman volume %q: %w", name, createErr)
	}

	exists, err = e.backend.VolumeExists(name)
	if err != nil {
		return fmt.Errorf("verify podman volume %q after create conflict: %w", name, err)
	}
	if !exists {
		return fmt.Errorf(
			"podman reported volume %q already exists but it cannot be found: %w",
			name, createErr,
		)
	}

	log.Info("podman named volume was created concurrently; reusing it", "volume", name)
	return nil
}

// isVolumeExistsError reports whether err is podman's "volume already exists"
// outcome. Over the remote bindings the sentinel does not survive the wire:
// the server renders it into an *errorhandling.ErrorModel that carries only
// the message string, so errors.Is cannot see define.ErrVolumeExists and the
// message has to be matched as well. Both shapes podman produces —
// `volume with name x already exists: volume already exists` and the racing
// `adding volume to state: name "x" is in use: volume already exists` — end in
// the sentinel's text.
func isVolumeExistsError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, define.ErrVolumeExists) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), define.ErrVolumeExists.Error())
}

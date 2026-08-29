package docker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	cerrdefs "github.com/containerd/errdefs"
	dockercontainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/google/uuid"
)

// subPathMinAPIVersion is the first Docker Engine API version that supports
// mount.VolumeOptions.Subpath (a named-volume mount scoped to a
// sub-directory of the volume rather than its root).
const subPathMinAPIVersion = "1.45"

// subPathHelperImage is the canonical default for the CAESIUM_DOCKER_SUBPATH_HELPER_IMAGE
// override (env.Environment.DockerSubpathHelperImage): the minimal image used
// to create the sub-directory a VolumeMount.SubPath addresses before the real
// container mounts it with VolumeOptions.Subpath. Docker refuses to start a
// container whose subpath does not already exist on the volume, and the
// target step's own image is not guaranteed to carry a shell or mkdir.
// Pinned per the repo's image-pin guardrail
// (internal/guardrails/guardrails_test.go); the env default keeps that pin
// while letting an air-gapped or private-registry install point the helper
// at a mirrored reference instead.
const subPathHelperImage = "alpine:3.23"

// subPathHelperMountDir is where the helper container mounts the volume
// (whole, unscoped) so it can create the sub-directory beneath it.
const subPathHelperMountDir = "/caesium-subpath"

// subPathHelperScript is the helper container's entire job: create the
// sub-directory named by $1 if (and only if) it does not already exist, and
// make it world-writable so a non-root step image that does not already own
// the mount target can still write into it (see ensureVolumeSubPath). A
// pre-existing sub-directory — one an earlier ordinary mount already
// materialized and possibly chowned — is left completely untouched.
const subPathHelperScript = `test -d "$1" || (mkdir -p "$1" && chmod 0777 "$1")`

// Engine defines the interface for treating the
// Docker API as a atom.Engine.
type Engine interface {
	atom.Engine
}

type dockerEngine struct {
	ctx     context.Context
	backend dockerBackend
	// subpathHelperImage is the image ensureVolumeSubPath uses for its
	// sub-directory-creation helper container. Defaults to subPathHelperImage
	// but is overridable via CAESIUM_DOCKER_SUBPATH_HELPER_IMAGE for
	// air-gapped or private-registry-only installs (I-3).
	subpathHelperImage string
}

// NewEngine creates a new instance of docker.Engine
// for interacting with docker.Atoms.
func NewEngine(ctx context.Context) Engine {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	return &dockerEngine{
		ctx:                ctx,
		backend:            cli,
		subpathHelperImage: resolveSubPathHelperImage(),
	}
}

// resolveSubPathHelperImage returns the configured
// CAESIUM_DOCKER_SUBPATH_HELPER_IMAGE override, falling back to the pinned
// canonical default when it is unset (envconfig already applies that same
// default, so this only guards a dockerEngine built without env.Process()
// having run first).
func resolveSubPathHelperImage() string {
	if img := env.Variables().DockerSubpathHelperImage; img != "" {
		return img
	}
	return subPathHelperImage
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
	if err := e.ensureImagePresent(req.Image); err != nil {
		return nil, err
	}

	cfg := &dockercontainer.Config{
		Image: req.Image,
		Cmd:   req.Command,
		Env:   formatEnv(req.Spec.Env),
	}
	if req.Spec.WorkDir != "" {
		cfg.WorkingDir = req.Spec.WorkDir
	}

	var subpathSupported bool
	if hasVolumeSubPath(req.Spec.ResolvedVolumeMounts) {
		subpathSupported = versions.GreaterThanOrEqualTo(e.backend.ClientVersion(), subPathMinAPIVersion)
	}

	mounts, err := convertMounts(req.Spec.Mounts, req.Spec.ResolvedVolumeMounts, subpathSupported)
	if err != nil {
		return nil, err
	}

	if err := e.ensureVolumeSubPaths(req.Spec.ResolvedVolumeMounts); err != nil {
		return nil, err
	}

	var hostCfg *dockercontainer.HostConfig
	if len(mounts) > 0 {
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

func (e *dockerEngine) ensureImagePresent(imageRef string) error {
	if imageRef != "" {
		if _, err := e.backend.ImageInspect(e.ctx, imageRef); err == nil {
			log.Info("docker image already present", "image", imageRef)
			return nil
		} else if !cerrdefs.IsNotFound(err) {
			return err
		}
	}

	log.Info("pulling docker image", "image", imageRef)

	r, err := e.backend.ImagePull(e.ctx, imageRef, image.PullOptions{})
	if err != nil {
		return err
	}
	defer func() {
		if err := r.Close(); err != nil {
			log.Error("close docker pull reader", "error", err)
		}
	}()

	if _, err = io.ReadAll(r); err != nil {
		return err
	}

	log.Info("docker image pulled", "image", imageRef)
	return nil
}

func (e *dockerEngine) Wait(req *atom.EngineWaitRequest) (atom.Atom, error) {
	waitCtx := e.ctx
	if req != nil && req.Context != nil {
		waitCtx = req.Context
	}
	resultC, errC := e.backend.ContainerWait(waitCtx, req.ID, dockercontainer.WaitConditionNotRunning)
	select {
	case err := <-errC:
		if err != nil {
			return nil, err
		}
	case <-resultC:
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	}
	return e.Get(&atom.EngineGetRequest{ID: req.ID})
}

// Stop and remove a Caesium Docker container. Since Caesium
// doesn't distinguish between a "stopped" and a "removed"
// container, we encapsulate both functions inside
// docker.Atom.Stop.
//
// We use context.Background() for the Docker API calls so that
// cleanup succeeds even when the parent context has been
// cancelled (e.g. by a run-level timeout).
func (e *dockerEngine) Stop(req *atom.EngineStopRequest) error {
	log.Info("stopping docker container", "id", req.ID)

	// Use a detached context so that container cleanup is not
	// short-circuited by a cancelled parent (run timeout, etc.).
	cleanupCtx := context.Background()

	timeout := int(req.Timeout.Seconds())
	if err := e.backend.ContainerStop(cleanupCtx, req.ID, dockercontainer.StopOptions{Timeout: &timeout}); err != nil {
		return err
	}

	log.Info("removing docker container", "id", req.ID)

	opts := dockercontainer.RemoveOptions{
		Force:         req.Force,
		RemoveVolumes: true,
	}

	return e.backend.ContainerRemove(cleanupCtx, req.ID, opts)
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

	raw, err := e.backend.ContainerLogs(e.ctx, req.ID, opts)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()

	go func() {
		defer func() { _ = raw.Close() }()
		defer func() { _ = pw.Close() }()

		// multiplexed logs need to be demultiplexed using StdCopy
		if _, err := stdcopy.StdCopy(pw, pw, raw); err != nil {
			log.Error("failed to demultiplex docker logs", "error", err)
		}
	}()

	return pr, nil
}

func formatEnv(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, fmt.Sprintf("%s=%s", k, values[k]))
	}
	return env
}

// hasVolumeSubPath reports whether any resolved named-volume mount declares a
// SubPath. Bind-mount SubPath is a pure client-side path join and never needs
// the Docker Engine API version check below.
func hasVolumeSubPath(resolvedMounts []container.VolumeMount) bool {
	for _, mnt := range resolvedMounts {
		if mnt.Type == container.VolumeMountTypeVolume && mnt.SubPath != "" {
			return true
		}
	}
	return false
}

// validatedSubPath validates that subPath is a relative path contained
// within the mount it addresses and returns its cleaned form. Both
// path.Join (used for bind mounts) and Docker's own VolumeOptions.Subpath
// RESOLVE ".." rather than rejecting it — path.Join("/srv/data", "../../..")
// is "/" — so without this check "subPath: ../../etc" would silently escape
// the declared bind source, and the equivalent on a named volume would only
// be caught late (and after the helper container had already created a
// stray directory) by Docker's own refusal. filepath.IsLocal matches what
// Docker itself enforces for VolumeOptions.Subpath ("subpath must be a
// relative path within the volume"). "." (the mount root itself) cleans to
// the empty string, i.e. "no subPath declared", so callers only need to
// branch on cleanedSubPath != "".
func validatedSubPath(target, subPath string) (string, error) {
	if subPath == "" {
		return "", nil
	}
	if !filepath.IsLocal(subPath) {
		return "", fmt.Errorf("docker mount %q: subPath %q must be a relative path within the volume", target, subPath)
	}
	cleaned := path.Clean(subPath)
	if cleaned == "." {
		return "", nil
	}
	return cleaned, nil
}

// convertMounts maps Caesium's runtime-neutral mount types onto Docker's
// mount.Mount. VolumeMount.SubPath is validated once per mount (see
// validatedSubPath) and, once validated, is honoured for both mount kinds it
// can apply to:
//   - bind: SubPath is joined onto the host source path directly (Docker
//     bind mounts have no separate subpath option).
//   - volume: SubPath is mapped onto mount.VolumeOptions.Subpath, which
//     requires Docker Engine API >= 1.45 and the sub-directory to already
//     exist on the volume (see ensureVolumeSubPath). If the negotiated API
//     is older, an error is returned rather than silently mounting the whole
//     volume — Docker never gets to see (and ignore) the field.
func convertMounts(specMounts []container.Mount, resolvedMounts []container.VolumeMount, subpathSupported bool) ([]mount.Mount, error) {
	if len(specMounts) == 0 && len(resolvedMounts) == 0 {
		return nil, nil
	}
	result := make([]mount.Mount, 0, len(specMounts)+len(resolvedMounts))
	for _, mnt := range specMounts {
		if mnt.Target == "" {
			continue
		}
		switch mnt.Type {
		case container.MountTypeBind, "":
			if mnt.Source == "" {
				continue
			}
			result = append(result, mount.Mount{
				Type:     mount.TypeBind,
				Source:   mnt.Source,
				Target:   mnt.Target,
				ReadOnly: mnt.ReadOnly,
			})
		case container.MountTypeVolume:
			if mnt.Source == "" {
				continue
			}
			result = append(result, mount.Mount{
				Type:     mount.TypeVolume,
				Source:   mnt.Source,
				Target:   mnt.Target,
				ReadOnly: mnt.ReadOnly,
			})
		case container.MountTypeTmpfs:
			result = append(result, mount.Mount{
				Type:     mount.TypeTmpfs,
				Target:   mnt.Target,
				ReadOnly: mnt.ReadOnly,
			})
		}
	}
	for _, mnt := range resolvedMounts {
		if mnt.Target == "" {
			continue
		}
		cleanedSubPath, err := validatedSubPath(mnt.Target, mnt.SubPath)
		if err != nil {
			return nil, err
		}
		switch mnt.Type {
		case container.VolumeMountTypeBind:
			if mnt.Source == "" {
				continue
			}
			source := mnt.Source
			if cleanedSubPath != "" {
				source = path.Join(mnt.Source, cleanedSubPath)
			}
			result = append(result, mount.Mount{
				Type:     mount.TypeBind,
				Source:   source,
				Target:   mnt.Target,
				ReadOnly: mnt.ReadOnly,
			})
		case container.VolumeMountTypeVolume:
			if mnt.Source == "" {
				continue
			}
			dockerMount := mount.Mount{
				Type:     mount.TypeVolume,
				Source:   mnt.Source,
				Target:   mnt.Target,
				ReadOnly: mnt.ReadOnly,
			}
			if cleanedSubPath != "" {
				if !subpathSupported {
					return nil, fmt.Errorf(
						"docker mount %q on volume %q: subPath %q requires Docker Engine API >= %s "+
							"(VolumeOptions.Subpath); refusing to silently mount the whole volume instead",
						mnt.Target, mnt.Source, cleanedSubPath, subPathMinAPIVersion)
				}
				dockerMount.VolumeOptions = &mount.VolumeOptions{Subpath: cleanedSubPath}
			}
			result = append(result, dockerMount)
		case container.VolumeMountTypeTmpfs:
			dockerMount := mount.Mount{
				Type:     mount.TypeTmpfs,
				Target:   mnt.Target,
				ReadOnly: mnt.ReadOnly,
			}
			if mnt.Tmpfs != nil && (mnt.Tmpfs.SizeBytes > 0 || mnt.Tmpfs.Mode != nil) {
				opts := &mount.TmpfsOptions{SizeBytes: mnt.Tmpfs.SizeBytes}
				if mnt.Tmpfs.Mode != nil {
					opts.Mode = os.FileMode(*mnt.Tmpfs.Mode)
				}
				dockerMount.TmpfsOptions = opts
			}
			result = append(result, dockerMount)
		}
	}
	return result, nil
}

// ensureVolumeSubPaths creates the sub-directory of every SubPath-scoped
// named-volume mount before the real container is created. Docker requires a
// VolumeOptions.Subpath directory to already exist; it never creates one on
// its own, unlike Kubernetes' kubelet or Podman's runtime. SubPath is
// re-validated here through the same validatedSubPath helper convertMounts
// uses, so the helper's mkdir target and the real mount's
// VolumeOptions.Subpath can never disagree on the value (by the time
// Create() reaches this call convertMounts has already validated every
// mount, so this can only fail if a caller invokes it directly).
func (e *dockerEngine) ensureVolumeSubPaths(resolvedMounts []container.VolumeMount) error {
	seen := make(map[string]struct{}, len(resolvedMounts))
	for _, mnt := range resolvedMounts {
		if mnt.Type != container.VolumeMountTypeVolume || mnt.Source == "" {
			continue
		}
		cleanedSubPath, err := validatedSubPath(mnt.Target, mnt.SubPath)
		if err != nil {
			return err
		}
		if cleanedSubPath == "" {
			continue
		}
		key := mnt.Source + "\x00" + cleanedSubPath
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if err := e.ensureVolumeSubPath(mnt.Source, cleanedSubPath); err != nil {
			return err
		}
	}
	return nil
}

// ensureVolumeSubPath creates cleanedSubPath's directory on the named volume
// volumeName using a short-lived helper container: it mounts the volume in
// full (no Subpath) at subPathHelperMountDir and runs subPathHelperScript,
// which creates the sub-directory only if it does not already exist and, in
// that case only, chmods it 0777 — otherwise Caesium's own root-running
// helper would be the sub-directory's first (and permission-deciding)
// mount, and a non-root step image that does not already own the mount
// target would get EACCES writing into it (see docs/infrastructure-deployment.md's
// "Volume ownership" section). cleanedSubPath must already be validated
// (see validatedSubPath) — this is the exact value the real container's
// mount.VolumeOptions.Subpath receives (see convertMounts), so the two can
// never disagree on what "the sub-directory" means.
func (e *dockerEngine) ensureVolumeSubPath(volumeName, cleanedSubPath string) error {
	if err := e.ensureImagePresent(e.subpathHelperImage); err != nil {
		return fmt.Errorf("pull subPath helper image for volume %q: %w", volumeName, err)
	}

	cfg := &dockercontainer.Config{
		Image: e.subpathHelperImage,
		Cmd:   []string{"sh", "-c", subPathHelperScript, "sh", path.Join(subPathHelperMountDir, cleanedSubPath)},
	}
	hostCfg := &dockercontainer.HostConfig{
		Mounts: []mount.Mount{{
			Type:   mount.TypeVolume,
			Source: volumeName,
			Target: subPathHelperMountDir,
		}},
	}
	name := fmt.Sprintf("caesium-subpath-init-%s", uuid.NewString())

	log.Info("creating docker subPath helper container", "volume", volumeName, "subPath", cleanedSubPath)

	created, err := e.backend.ContainerCreate(e.ctx, cfg, hostCfg, nil, nil, name)
	if err != nil {
		return fmt.Errorf("create subPath helper container for volume %q: %w", volumeName, err)
	}

	defer func() {
		// Detached context: cleanup must not be short-circuited by the
		// caller's context the way Stop() already avoids it.
		if rmErr := e.backend.ContainerRemove(context.Background(), created.ID, dockercontainer.RemoveOptions{Force: true}); rmErr != nil {
			log.Error("remove subPath helper container", "volume", volumeName, "error", rmErr)
		}
	}()

	if err := e.backend.ContainerStart(e.ctx, created.ID, dockercontainer.StartOptions{}); err != nil {
		return fmt.Errorf("start subPath helper container for volume %q: %w", volumeName, err)
	}

	resultC, errC := e.backend.ContainerWait(e.ctx, created.ID, dockercontainer.WaitConditionNotRunning)
	select {
	case waitErr := <-errC:
		if waitErr != nil {
			return fmt.Errorf("wait for subPath helper container for volume %q: %w", volumeName, waitErr)
		}
	case res := <-resultC:
		if res.StatusCode != 0 {
			return fmt.Errorf("subPath helper container for volume %q exited %d creating %q", volumeName, res.StatusCode, cleanedSubPath)
		}
	case <-e.ctx.Done():
		return e.ctx.Err()
	}

	return nil
}

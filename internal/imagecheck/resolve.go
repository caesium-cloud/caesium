package imagecheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// ErrDigestUnavailable is returned when a digest cannot be resolved for an
// image — e.g. the engine does not expose one at task-spec construction time
// (Kubernetes resolves digests in the kubelet pull status, not before the pod
// exists). Callers treat this as "fall back to the literal tag": a cache miss
// is always safe, so an unresolved digest never produces a stale hit, it only
// declines the extra tamper-evidence for that step.
var ErrDigestUnavailable = errors.New("imagecheck: image digest unavailable")

// DigestFunc resolves a single image reference to its content digest
// (sha256:...). Implementations may perform network I/O (a registry pull or
// inspect); the Resolver wraps them with a short-TTL cache so steady-state runs
// pay the cost at most once per TTL window.
type DigestFunc func(ctx context.Context, imageRef string) (string, error)

type cachedDigest struct {
	digest    string
	expiresAt time.Time
}

// Resolver resolves image tags to content digests and caches the mapping with a
// short TTL. It is safe for concurrent use. Resolution is engine-aware: each
// supported engine supplies a DigestFunc; engines without one (or with a nil
// func) report ErrDigestUnavailable so the caller falls back to the tag.
type Resolver struct {
	ttl     time.Duration
	now     func() time.Time
	mu      sync.Mutex
	entries map[string]cachedDigest
	byEngine map[models.AtomEngine]DigestFunc
}

// ResolverOption configures a Resolver.
type ResolverOption func(*Resolver)

var (
	defaultResolver     *Resolver
	defaultResolverOnce sync.Once
)

// Default returns a process-wide shared Resolver, built lazily from the given
// tag->digest cache TTL on first call. The TTL is fixed at first call (the
// shared cache outlives any single run); pass the configured CAESIUM_CACHE_*
// value. Sharing one instance means the tag->digest cache is warm across runs,
// so steady-state execution pays no per-task registry round-trip.
func Default(ttl time.Duration) *Resolver {
	defaultResolverOnce.Do(func() {
		defaultResolver = NewResolver(ttl)
	})
	return defaultResolver
}

// WithEngineDigestFunc registers (or overrides) the DigestFunc for an engine.
// Primarily a test seam; production wiring uses NewResolver's defaults.
func WithEngineDigestFunc(engine models.AtomEngine, fn DigestFunc) ResolverOption {
	return func(r *Resolver) { r.byEngine[engine] = fn }
}

// WithClock overrides the clock used for TTL expiry (test seam).
func WithClock(now func() time.Time) ResolverOption {
	return func(r *Resolver) {
		if now != nil {
			r.now = now
		}
	}
}

// NewResolver builds a Resolver with the given tag->digest cache TTL. By
// default the Docker engine resolves via the local Docker daemon (inspecting
// RepoDigests, pulling the image if it is not already present so the digest is
// available). Private-registry auth flows through the daemon's existing
// credential store exactly as the normal pull path does — no new credential
// configuration is introduced. Podman and Kubernetes have no pre-run digest
// source wired here yet, so they fall back to the literal tag.
func NewResolver(ttl time.Duration, opts ...ResolverOption) *Resolver {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	r := &Resolver{
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]cachedDigest),
		byEngine: make(map[models.AtomEngine]DigestFunc),
	}
	r.byEngine[models.AtomEngineDocker] = dockerDigestFunc
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve returns the content digest (sha256:...) for the image run by the
// given engine. On any resolution failure it returns ErrDigestUnavailable (the
// underlying cause is logged), so callers can fall back to the tag without
// special-casing every engine. A returned digest is cached for the Resolver's
// TTL, keyed by engine + image, so repeated resolutions within the window are
// free.
func (r *Resolver) Resolve(ctx context.Context, engine models.AtomEngine, imageRef string) (string, error) {
	imageRef = strings.TrimSpace(imageRef)
	if imageRef == "" {
		return "", ErrDigestUnavailable
	}

	// If the reference is already digest-pinned (foo@sha256:...), trust it
	// verbatim — no network round-trip, and the user has already opted into
	// immutability.
	if digest, ok := digestFromReference(imageRef); ok {
		return digest, nil
	}

	key := string(engine) + "|" + imageRef

	r.mu.Lock()
	if entry, ok := r.entries[key]; ok && r.now().Before(entry.expiresAt) {
		digest := entry.digest
		r.mu.Unlock()
		return digest, nil
	}
	fn := r.byEngine[engine]
	r.mu.Unlock()

	if fn == nil {
		return "", ErrDigestUnavailable
	}

	digest, err := fn(ctx, imageRef)
	if err != nil {
		log.Warn("image digest resolution failed; falling back to tag",
			"engine", engine, "image", imageRef, "error", err)
		return "", ErrDigestUnavailable
	}
	digest = strings.TrimSpace(digest)
	if !strings.HasPrefix(digest, "sha256:") {
		log.Warn("image digest resolution returned a non-sha256 value; falling back to tag",
			"engine", engine, "image", imageRef, "digest", digest)
		return "", ErrDigestUnavailable
	}

	r.mu.Lock()
	r.entries[key] = cachedDigest{digest: digest, expiresAt: r.now().Add(r.ttl)}
	r.mu.Unlock()

	return digest, nil
}

// dockerDigestFunc resolves a digest via the local Docker daemon. It inspects
// the image, pulling it first if it is not already present so RepoDigests are
// populated (a freshly built/untagged local image may have none).
func dockerDigestFunc(ctx context.Context, imageRef string) (string, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", fmt.Errorf("docker client: %w", err)
	}
	defer func() { _ = cli.Close() }()

	digest, err := dockerInspectDigest(ctx, cli, imageRef)
	if err == nil && digest != "" {
		return digest, nil
	}

	// Not present (or no usable digest yet): pull, then inspect again.
	if pullErr := dockerPull(ctx, cli, imageRef); pullErr != nil {
		// Prefer surfacing the inspect error if we had one; otherwise the pull error.
		if err != nil {
			return "", err
		}
		return "", pullErr
	}

	digest, err = dockerInspectDigest(ctx, cli, imageRef)
	if err != nil {
		return "", err
	}
	if digest == "" {
		return "", fmt.Errorf("docker: no RepoDigest for %s after pull", imageRef)
	}
	return digest, nil
}

func dockerInspectDigest(ctx context.Context, cli client.ImageAPIClient, imageRef string) (string, error) {
	inspect, err := cli.ImageInspect(ctx, imageRef)
	if err != nil {
		return "", err
	}
	return repoDigest(imageRef, inspect.RepoDigests), nil
}

func dockerPull(ctx context.Context, cli client.ImageAPIClient, imageRef string) error {
	r, err := cli.ImagePull(ctx, imageRef, image.PullOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	// Drain the pull progress stream so the pull completes before we re-inspect.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return nil
}

// repoDigest picks the sha256 digest from a list of RepoDigests entries (each
// of the form "repo@sha256:..."). When several entries are present it prefers
// the one whose repository matches the requested image, falling back to the
// first parseable digest.
func repoDigest(imageRef string, repoDigests []string) string {
	if len(repoDigests) == 0 {
		return ""
	}
	wantRepo := repositoryOf(imageRef)
	var fallback string
	for _, rd := range repoDigests {
		digest, ok := digestFromReference(rd)
		if !ok {
			continue
		}
		if fallback == "" {
			fallback = digest
		}
		if wantRepo != "" && strings.HasPrefix(rd, wantRepo+"@") {
			return digest
		}
	}
	return fallback
}

// digestFromReference extracts the sha256:... component from a reference of the
// form "name@sha256:...". Returns false when the reference carries no digest.
func digestFromReference(ref string) (string, bool) {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return "", false
	}
	digest := ref[at+1:]
	if !strings.HasPrefix(digest, "sha256:") || len(digest) <= len("sha256:") {
		return "", false
	}
	return digest, true
}

// repositoryOf strips the tag/digest from an image reference, leaving the
// repository (registry/name) so RepoDigests entries can be matched. It is
// digest-aware: it only treats a trailing ":tag" as a tag when it appears after
// the last "/" (so registry ports like "host:5000/img" are not mistaken for a
// tag).
func repositoryOf(imageRef string) string {
	if at := strings.LastIndex(imageRef, "@"); at >= 0 {
		imageRef = imageRef[:at]
	}
	slash := strings.LastIndex(imageRef, "/")
	colon := strings.LastIndex(imageRef, ":")
	if colon > slash {
		return imageRef[:colon]
	}
	return imageRef
}

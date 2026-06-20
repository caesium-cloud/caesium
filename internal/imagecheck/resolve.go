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
	// digest is the resolved sha256:... value, or "" for a negative entry
	// (resolution failed/unavailable). Negative entries are cached with a
	// shorter TTL so a transiently unreachable registry is retried promptly
	// without being hammered on every check.
	digest    string
	expiresAt time.Time
}

// negativeTTL bounds how long a failed resolution is remembered, given the
// caller's positive TTL. It is capped well below the positive TTL so a registry
// blip or a freshly pushed tag is re-checked soon, while still absorbing a
// burst of lookups for the same image. A zero (or negative) positive TTL means
// "always re-resolve", so the negative window is zero too.
func negativeTTL(ttl time.Duration) time.Duration {
	const cap = time.Minute
	if ttl <= 0 {
		return 0
	}
	if ttl < cap {
		return ttl
	}
	return cap
}

// Resolver resolves image tags to content digests and caches the mapping with a
// short, per-call TTL. It is safe for concurrent use. Resolution is
// engine-aware: each supported engine supplies a DigestFunc; engines without
// one (or with a nil func) report ErrDigestUnavailable so the caller falls back
// to the tag.
//
// The TTL is supplied per Resolve call (not fixed on the Resolver) so different
// jobs can demand different freshness against the same warm cache — e.g. a job
// with digestTTL: 0 re-resolves every check (immediate moved-tag detection)
// while others reuse the steady-state default and pay no registry round-trip.
type Resolver struct {
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

// Default returns a process-wide shared Resolver, built lazily on first call.
// Sharing one instance means the tag->digest cache is warm across runs, so
// steady-state execution pays no per-task registry round-trip. The cache TTL is
// supplied per Resolve call, so the shared instance can serve jobs with
// different freshness requirements.
func Default() *Resolver {
	defaultResolverOnce.Do(func() {
		defaultResolver = NewResolver()
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

// NewResolver builds a Resolver. By default the Docker engine resolves via the
// local Docker daemon: it inspects the image (using its content config digest /
// RepoDigests) and, only if the image is not already present locally, pulls it
// first so a digest is available. The cache TTL is supplied per Resolve call.
//
// Registry auth limitation: the pull-if-absent path uses an anonymous pull
// (no RegistryAuth is sent), so digest resolution for an image that must be
// pulled from a *private* registry will fail and the step falls back to the
// literal tag — which is always safe (a cache miss is never a stale hit).
// Images already present locally (the steady-state case, and any image the
// runtime already pulled) resolve without auth. Wiring RegistryAuth from the
// secret providers is a tracked follow-up. Podman and Kubernetes have no
// pre-run digest source wired here yet, so they also fall back to the tag.
func NewResolver(opts ...ResolverOption) *Resolver {
	r := &Resolver{
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
// special-casing every engine.
//
// ttl bounds how long a resolved tag->digest mapping is reused. It is a perf
// cache: within the window a moved tag is NOT re-detected (the prior digest is
// served). A ttl of 0 (or negative) disables the cache for this call — the
// digest is re-resolved every time, so a moved tag is detected immediately at
// the cost of a registry round-trip per check. Different callers may pass
// different TTLs against the same shared, warm cache.
func (r *Resolver) Resolve(ctx context.Context, engine models.AtomEngine, imageRef string, ttl time.Duration) (string, error) {
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

	// ttl <= 0 means "always re-resolve": skip the positive-cache read so a
	// moved tag is detected on the very next check. (Resolved values are also
	// not stored below, so the cache cannot mask a later move either.)
	if ttl > 0 {
		r.mu.Lock()
		if entry, ok := r.entries[key]; ok && r.now().Before(entry.expiresAt) {
			digest := entry.digest
			r.mu.Unlock()
			// A cached entry with an empty digest is a remembered failure: fall
			// back to the tag without re-hitting the backend (negative caching).
			if digest == "" {
				return "", ErrDigestUnavailable
			}
			return digest, nil
		}
		r.mu.Unlock()
	}

	fn := r.byEngine[engine]
	if fn == nil {
		// No backend for this engine is a stable condition (e.g. k8s/podman),
		// so cache it negatively to avoid re-evaluating on every check.
		r.cacheNegative(key, ttl)
		return "", ErrDigestUnavailable
	}

	digest, err := fn(ctx, imageRef)
	if err != nil {
		log.Warn("image digest resolution failed; falling back to tag",
			"engine", engine, "image", imageRef, "error", err)
		r.cacheNegative(key, ttl)
		return "", ErrDigestUnavailable
	}
	digest = strings.TrimSpace(digest)
	if !strings.HasPrefix(digest, "sha256:") {
		log.Warn("image digest resolution returned a non-sha256 value; falling back to tag",
			"engine", engine, "image", imageRef, "digest", digest)
		r.cacheNegative(key, ttl)
		return "", ErrDigestUnavailable
	}

	if ttl > 0 {
		r.mu.Lock()
		r.entries[key] = cachedDigest{digest: digest, expiresAt: r.now().Add(ttl)}
		r.mu.Unlock()
	}

	return digest, nil
}

// cacheNegative remembers a failed/unavailable resolution for a bounded window
// so an unreachable registry (or an unsupported engine) is not re-probed and
// the logs are not flooded on every task check. With ttl <= 0 the failure is
// not cached (the caller opted into always re-resolving).
func (r *Resolver) cacheNegative(key string, ttl time.Duration) {
	negTTL := negativeTTL(ttl)
	if negTTL <= 0 {
		return
	}
	r.mu.Lock()
	r.entries[key] = cachedDigest{digest: "", expiresAt: r.now().Add(negTTL)}
	r.mu.Unlock()
}

var (
	dockerCli     *client.Client
	dockerCliOnce sync.Once
	dockerCliErr  error
)

// getDockerClient returns a process-wide shared Docker client, created lazily.
// The Docker client is thread-safe and intended to be long-lived, so a single
// instance is reused across all digest resolutions rather than re-dialed (and
// re-FD'd) per call. It is intentionally never Closed — it lives for the
// process.
func getDockerClient() (*client.Client, error) {
	dockerCliOnce.Do(func() {
		dockerCli, dockerCliErr = client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	})
	return dockerCli, dockerCliErr
}

// dockerDigestFunc resolves a digest via the local Docker daemon. It inspects
// the image first; if the image is already present (the common case) no network
// I/O happens. Only when the image is absent does it pull it (anonymously —
// see the NewResolver doc for the private-registry caveat) and re-inspect.
func dockerDigestFunc(ctx context.Context, imageRef string) (string, error) {
	cli, err := getDockerClient()
	if err != nil {
		return "", fmt.Errorf("docker client: %w", err)
	}

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
		return "", fmt.Errorf("docker: no digest for %s after pull", imageRef)
	}
	return digest, nil
}

// dockerInspectDigest returns a content-addressed digest for an image. It
// prefers a RepoDigest (the registry manifest digest, stable across hosts), and
// falls back to the image's own config digest (inspect.ID) when there are no
// RepoDigests — which is the case for locally built or never-pushed images.
// Using the config digest there gives a valid, content-addressed cache key and
// avoids a doomed registry pull for an image that exists only locally.
func dockerInspectDigest(ctx context.Context, cli client.ImageAPIClient, imageRef string) (string, error) {
	inspect, err := cli.ImageInspect(ctx, imageRef)
	if err != nil {
		return "", err
	}
	if digest := repoDigest(imageRef, inspect.RepoDigests); digest != "" {
		return digest, nil
	}
	if strings.HasPrefix(inspect.ID, "sha256:") {
		return inspect.ID, nil
	}
	return "", nil
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

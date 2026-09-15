package imagecheck

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeImageAPIClient implements just the ImageInspect surface dockerInspectDigest
// uses; the embedded nil interface panics if any other method is called, which
// keeps the fake honest about what the code under test actually touches.
type fakeImageAPIClient struct {
	client.ImageAPIClient
	inspect image.InspectResponse
	err     error
}

func (f fakeImageAPIClient) ImageInspect(_ context.Context, _ string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	return f.inspect, f.err
}

func TestResolver_CachesWithinTTL(t *testing.T) {
	var calls atomic.Int32
	fn := func(_ context.Context, _ string) (string, error) {
		calls.Add(1)
		return "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", nil
	}
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineDocker, fn))

	for range 3 {
		got, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", got)
	}
	assert.Equal(t, int32(1), calls.Load(), "resolution should be cached within the TTL")
}

func TestResolver_ZeroTTLAlwaysReresolves(t *testing.T) {
	// The core fix for the moved-tag integration failure: a ttl of 0 must skip
	// the positive cache so each check re-resolves and a moved tag is detected
	// immediately.
	var calls atomic.Int32
	digests := []string{"sha256:a7937b64b8caa58f03721bb6bacf5c78cb235febe0e70b1b84cd99541461a08e", "sha256:16367aacb67a4a017c8da8ab95682ccb390863780f7114dda0a0e0c55644c7c4", "sha256:b1e99324505bd32da0e1f85dcf5e19a09db0481e8a15f62c41eb320304a8e927"}
	fn := func(_ context.Context, _ string) (string, error) {
		n := calls.Add(1)
		return digests[n-1], nil
	}
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineDocker, fn))

	for i, want := range digests {
		got, err := r.Resolve(context.Background(), models.AtomEngineDocker, "app:latest", 0)
		require.NoError(t, err)
		assert.Equal(t, want, got, "call %d must re-resolve, not serve a cached digest", i+1)
	}
	assert.Equal(t, int32(3), calls.Load(), "ttl=0 must re-resolve on every call")
}

func TestResolver_ReresolvesAfterTTL(t *testing.T) {
	var calls atomic.Int32
	digests := []string{"sha256:a7937b64b8caa58f03721bb6bacf5c78cb235febe0e70b1b84cd99541461a08e", "sha256:16367aacb67a4a017c8da8ab95682ccb390863780f7114dda0a0e0c55644c7c4"}
	fn := func(_ context.Context, _ string) (string, error) {
		n := calls.Add(1)
		return digests[n-1], nil
	}

	now := time.Unix(0, 0)
	r := NewResolver(
		WithEngineDigestFunc(models.AtomEngineDocker, fn),
		WithClock(func() time.Time { return now }),
	)

	got, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "sha256:a7937b64b8caa58f03721bb6bacf5c78cb235febe0e70b1b84cd99541461a08e", got)

	// Advance the clock past the TTL: a fresh resolution must happen, and a
	// moved tag must surface its new digest (the correctness invariant).
	now = now.Add(2 * time.Minute)
	got, err = r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "sha256:16367aacb67a4a017c8da8ab95682ccb390863780f7114dda0a0e0c55644c7c4", got)
	assert.Equal(t, int32(2), calls.Load())
}

func TestResolver_AlreadyPinnedReferenceSkipsResolution(t *testing.T) {
	fn := func(_ context.Context, _ string) (string, error) {
		t.Fatal("resolver must not call the backend for an already-digest-pinned reference")
		return "", nil
	}
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineDocker, fn))

	ref := "example.com/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	got, err := r.Resolve(context.Background(), models.AtomEngineDocker, ref, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", got)
}

func TestResolver_BackendErrorFallsBack(t *testing.T) {
	fn := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("registry unreachable")
	}
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineDocker, fn))

	_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", time.Minute)
	assert.ErrorIs(t, err, ErrDigestUnavailable, "a backend error must surface as ErrDigestUnavailable so callers fall back to the tag")
}

func TestResolver_NegativeCachingAvoidsReprobe(t *testing.T) {
	var calls atomic.Int32
	fn := func(_ context.Context, _ string) (string, error) {
		calls.Add(1)
		return "", errors.New("registry unreachable")
	}
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineDocker, fn))

	// Several checks in quick succession must hit the backend only once: the
	// failure is negatively cached so an unreachable registry is not hammered.
	for range 5 {
		_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", time.Minute)
		assert.ErrorIs(t, err, ErrDigestUnavailable)
	}
	assert.Equal(t, int32(1), calls.Load(), "a failed resolution must be negatively cached, not re-probed every check")
}

func TestResolver_NegativeCacheExpires(t *testing.T) {
	var calls atomic.Int32
	results := []struct {
		digest string
		err    error
	}{
		{"", errors.New("registry blip")},
		{"sha256:f6e09cc89f85dcd21d987a4c4af142fe5bbb741de93d375af548f1d4f1d2063b", nil},
	}
	fn := func(_ context.Context, _ string) (string, error) {
		n := calls.Add(1)
		res := results[n-1]
		return res.digest, res.err
	}

	now := time.Unix(0, 0)
	r := NewResolver(
		WithEngineDigestFunc(models.AtomEngineDocker, fn),
		WithClock(func() time.Time { return now }),
	)

	// Positive TTL 1h -> negative window caps at 1m.
	const posTTL = time.Hour
	_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", posTTL)
	assert.ErrorIs(t, err, ErrDigestUnavailable)

	// Within the (capped) negative window: no re-probe.
	now = now.Add(30 * time.Second)
	_, err = r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", posTTL)
	assert.ErrorIs(t, err, ErrDigestUnavailable)
	assert.Equal(t, int32(1), calls.Load(), "negative entry must still be valid at 30s")

	// Past the 1m negative cap: re-resolve, and a recovered registry now hits.
	now = now.Add(2 * time.Minute)
	got, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", posTTL)
	require.NoError(t, err)
	assert.Equal(t, "sha256:f6e09cc89f85dcd21d987a4c4af142fe5bbb741de93d375af548f1d4f1d2063b", got)
	assert.Equal(t, int32(2), calls.Load())
}

func TestNegativeTTL(t *testing.T) {
	assert.Equal(t, 30*time.Second, negativeTTL(30*time.Second), "below the cap the full TTL is used")
	assert.Equal(t, time.Minute, negativeTTL(time.Hour), "above the cap the negative TTL is clamped to 1m")
	assert.Equal(t, time.Duration(0), negativeTTL(0), "a zero TTL yields no negative window")
}

func TestResolver_NonSha256Rejected(t *testing.T) {
	fn := func(_ context.Context, _ string) (string, error) {
		return "not-a-digest", nil
	}
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineDocker, fn))

	_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23", time.Minute)
	assert.ErrorIs(t, err, ErrDigestUnavailable)
}

func TestResolver_AcceptsLocalImageIDDigest(t *testing.T) {
	const configID = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	fn := func(_ context.Context, _ string) (string, error) {
		return MarkImageIDDigest(configID), nil
	}
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineDocker, fn))

	got, err := r.Resolve(context.Background(), models.AtomEngineDocker, "locally-built:dev", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, MarkImageIDDigest(configID), got)
	assert.Equal(t, configID, PinReference("locally-built:dev", got))
}

func TestResolver_UnwiredEngineUnavailable(t *testing.T) {
	// An engine explicitly unwired (nil DigestFunc) must report unavailable
	// rather than panic or return an empty digest.
	r := NewResolver(WithEngineDigestFunc(models.AtomEngineKubernetes, nil))
	_, err := r.Resolve(context.Background(), models.AtomEngineKubernetes, "alpine:3.23", time.Minute)
	assert.ErrorIs(t, err, ErrDigestUnavailable)

	// And an engine the resolver has never heard of.
	_, err = r.Resolve(context.Background(), models.AtomEngine("firecracker"), "alpine:3.23", time.Minute)
	assert.ErrorIs(t, err, ErrDigestUnavailable)
}

func TestResolver_EmptyImageUnavailable(t *testing.T) {
	r := NewResolver()
	_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "   ", time.Minute)
	assert.ErrorIs(t, err, ErrDigestUnavailable)
}

func TestRepoDigest_PrefersMatchingRepository(t *testing.T) {
	repoDigests := []string{
		"other/img@sha256:61be55a8e2f6b4e172338bddf184d6dbee29c98853e0a0485ecee7f27b9af0b4",
		"library/app@sha256:81cc5b17018674b401b42f35ba07bb79e211239c23bffe658da1577e3e646877",
	}
	assert.Equal(t, "sha256:81cc5b17018674b401b42f35ba07bb79e211239c23bffe658da1577e3e646877", repoDigest("library/app:1.0", repoDigests))
}

func TestRepoDigest_FallsBackToFirst(t *testing.T) {
	repoDigests := []string{"some/other@sha256:b6fbd675f98e2abd22d4ed29fdc83150fedc48597e92dd1a7a24381d44a27451"}
	assert.Equal(t, "sha256:b6fbd675f98e2abd22d4ed29fdc83150fedc48597e92dd1a7a24381d44a27451", repoDigest("library/app:1.0", repoDigests))
}

func TestRepoDigest_EmptyWhenNone(t *testing.T) {
	assert.Equal(t, "", repoDigest("library/app:1.0", nil))
}

func TestRepositoryOf(t *testing.T) {
	cases := map[string]string{
		"app:1.0":                          "app",
		"library/app:1.0":                  "library/app",
		"registry.example.com:5000/img":    "registry.example.com:5000/img",
		"registry.example.com:5000/img:v1": "registry.example.com:5000/img",
		"app@sha256:abcd":                  "app",
		"app":                              "app",
	}
	for in, want := range cases {
		assert.Equal(t, want, repositoryOf(in), "repositoryOf(%q)", in)
	}
}

func TestDigestFromReference(t *testing.T) {
	d, ok := digestFromReference("repo@sha256:28a3a5e81d1e89f0efc70b63bf717b921373fc7fac70bc1b7e4d466799c0c6b0")
	assert.True(t, ok)
	assert.Equal(t, "sha256:28a3a5e81d1e89f0efc70b63bf717b921373fc7fac70bc1b7e4d466799c0c6b0", d)

	_, ok = digestFromReference("repo:tag")
	assert.False(t, ok)

	_, ok = digestFromReference("repo@sha256:")
	assert.False(t, ok, "empty digest body must be rejected")
}

func TestDockerInspectDigest_PrefersRepoDigest(t *testing.T) {
	cli := fakeImageAPIClient{inspect: image.InspectResponse{
		ID:          "sha256:e0d6810644e01c3aef72e8c6d63392c4f35ea9605a00a441b5271455d24e43eb",
		RepoDigests: []string{"library/app@sha256:3ee3e96d3d9b72b92f396bf32b8221eb3f7a771a882f85ceccf70aaebdb8df2e"},
	}}
	got, err := dockerInspectDigest(context.Background(), cli, "library/app:1.0")
	require.NoError(t, err)
	assert.Equal(t, "sha256:3ee3e96d3d9b72b92f396bf32b8221eb3f7a771a882f85ceccf70aaebdb8df2e", got, "a RepoDigest should win over the config ID")
}

func TestDockerInspectDigest_FallsBackToImageID(t *testing.T) {
	// Locally built / never-pushed images have no RepoDigests; the config
	// digest (inspect.ID) is a valid content-addressed key and is marked so
	// PinReference executes the image ID rather than a name@digest pin.
	const configID = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	cli := fakeImageAPIClient{inspect: image.InspectResponse{
		ID:          configID,
		RepoDigests: nil,
	}}
	got, err := dockerInspectDigest(context.Background(), cli, "locally-built:dev")
	require.NoError(t, err)
	assert.Equal(t, MarkImageIDDigest(configID), got)
	assert.Equal(t, configID, PinReference("locally-built:dev", got),
		"Create must address the config ID, not locally-built:dev@<configID>")
}

func TestDockerInspectDigest_EmptyWhenNoUsableDigest(t *testing.T) {
	cli := fakeImageAPIClient{inspect: image.InspectResponse{ID: "not-a-sha", RepoDigests: nil}}
	got, err := dockerInspectDigest(context.Background(), cli, "weird:tag")
	require.NoError(t, err)
	assert.Equal(t, "", got, "no RepoDigest and a non-sha256 ID yields an empty digest -> caller falls back to the tag")
}

func TestDockerInspectDigest_PropagatesInspectError(t *testing.T) {
	cli := fakeImageAPIClient{err: errors.New("no such image")}
	_, err := dockerInspectDigest(context.Background(), cli, "missing:tag")
	assert.Error(t, err)
}

func TestResolverRejectsMalformedDigestBeforePinning(t *testing.T) {
	for _, value := range []string{"sha256:abc", "id:sha256:abc", "sha256:", "sha256:" + strings.Repeat("g", 64)} {
		r := NewResolver(WithEngineDigestFunc(models.AtomEngineKubernetes, func(context.Context, string) (string, error) { return value, nil }))
		_, err := r.Resolve(context.Background(), models.AtomEngineKubernetes, "app:mutable", 0)
		require.ErrorIs(t, err, ErrDigestUnavailable, value)
	}
}

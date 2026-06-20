package imagecheck

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolver_CachesWithinTTL(t *testing.T) {
	var calls int32
	fn := func(_ context.Context, _ string) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "sha256:abc", nil
	}
	r := NewResolver(time.Minute, WithEngineDigestFunc(models.AtomEngineDocker, fn))

	for i := 0; i < 3; i++ {
		got, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23")
		require.NoError(t, err)
		assert.Equal(t, "sha256:abc", got)
	}
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "resolution should be cached within the TTL")
}

func TestResolver_ReresolvesAfterTTL(t *testing.T) {
	var calls int32
	digests := []string{"sha256:first", "sha256:second"}
	fn := func(_ context.Context, _ string) (string, error) {
		n := atomic.AddInt32(&calls, 1)
		return digests[n-1], nil
	}

	now := time.Unix(0, 0)
	r := NewResolver(time.Minute,
		WithEngineDigestFunc(models.AtomEngineDocker, fn),
		WithClock(func() time.Time { return now }),
	)

	got, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23")
	require.NoError(t, err)
	assert.Equal(t, "sha256:first", got)

	// Advance the clock past the TTL: a fresh resolution must happen, and a
	// moved tag must surface its new digest (the correctness invariant).
	now = now.Add(2 * time.Minute)
	got, err = r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23")
	require.NoError(t, err)
	assert.Equal(t, "sha256:second", got)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

func TestResolver_AlreadyPinnedReferenceSkipsResolution(t *testing.T) {
	fn := func(_ context.Context, _ string) (string, error) {
		t.Fatal("resolver must not call the backend for an already-digest-pinned reference")
		return "", nil
	}
	r := NewResolver(time.Minute, WithEngineDigestFunc(models.AtomEngineDocker, fn))

	ref := "example.com/app@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	got, err := r.Resolve(context.Background(), models.AtomEngineDocker, ref)
	require.NoError(t, err)
	assert.Equal(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", got)
}

func TestResolver_BackendErrorFallsBack(t *testing.T) {
	fn := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("registry unreachable")
	}
	r := NewResolver(time.Minute, WithEngineDigestFunc(models.AtomEngineDocker, fn))

	_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23")
	assert.ErrorIs(t, err, ErrDigestUnavailable, "a backend error must surface as ErrDigestUnavailable so callers fall back to the tag")
}

func TestResolver_NonSha256Rejected(t *testing.T) {
	fn := func(_ context.Context, _ string) (string, error) {
		return "not-a-digest", nil
	}
	r := NewResolver(time.Minute, WithEngineDigestFunc(models.AtomEngineDocker, fn))

	_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "alpine:3.23")
	assert.ErrorIs(t, err, ErrDigestUnavailable)
}

func TestResolver_UnsupportedEngineUnavailable(t *testing.T) {
	// Kubernetes has no DigestFunc wired, so it must report unavailable rather
	// than panic or return an empty digest.
	r := NewResolver(time.Minute)
	_, err := r.Resolve(context.Background(), models.AtomEngineKubernetes, "alpine:3.23")
	assert.ErrorIs(t, err, ErrDigestUnavailable)
}

func TestResolver_EmptyImageUnavailable(t *testing.T) {
	r := NewResolver(time.Minute)
	_, err := r.Resolve(context.Background(), models.AtomEngineDocker, "   ")
	assert.ErrorIs(t, err, ErrDigestUnavailable)
}

func TestRepoDigest_PrefersMatchingRepository(t *testing.T) {
	repoDigests := []string{
		"other/img@sha256:aaaa",
		"library/app@sha256:bbbb",
	}
	assert.Equal(t, "sha256:bbbb", repoDigest("library/app:1.0", repoDigests))
}

func TestRepoDigest_FallsBackToFirst(t *testing.T) {
	repoDigests := []string{"some/other@sha256:cccc"}
	assert.Equal(t, "sha256:cccc", repoDigest("library/app:1.0", repoDigests))
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
	d, ok := digestFromReference("repo@sha256:dead")
	assert.True(t, ok)
	assert.Equal(t, "sha256:dead", d)

	_, ok = digestFromReference("repo:tag")
	assert.False(t, ok)

	_, ok = digestFromReference("repo@sha256:")
	assert.False(t, ok, "empty digest body must be rejected")
}

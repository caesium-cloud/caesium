package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baseInput() HashInput {
	return HashInput{
		JobAlias: "my-job",
		TaskName: "my-task",
		Image:    "alpine:3.23",
		Command:  []string{"echo", "hello"},
		Env:      map[string]string{"FOO": "bar", "BAZ": "qux"},
		WorkDir:  "/app",
		Mounts: []container.Mount{
			{Type: container.MountTypeBind, Source: "/host", Target: "/container", ReadOnly: true},
		},
		PredecessorHashes:  []string{"abc123", "def456"},
		PredecessorOutputs: map[string]map[string]string{"step1": {"key": "val"}},
		RunParams:          map[string]string{"param1": "value1"},
		CacheVersion:       1,
	}
}

func TestCompute_Deterministic(t *testing.T) {
	h1 := baseInput().Compute()
	h2 := baseInput().Compute()
	assert.Equal(t, h1, h2, "same input should produce same hash")
	assert.Len(t, h1, 64, "SHA-256 hex digest should be 64 characters")
}

func TestCompute_DifferentJobAlias(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.JobAlias = "other-job"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentTaskName(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.TaskName = "other-task"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentImage(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Image = "ubuntu:22.04"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

// TestCompute_PinDigestsOffPreservesLegacyHash asserts that when no digest is
// resolved (pinDigests off), the hash is byte-identical to the pre-pinning
// behavior — i.e. an empty ResolvedImageDigest contributes nothing. This keeps
// existing cache entries valid across the rollout.
func TestCompute_PinDigestsOffPreservesLegacyHash(t *testing.T) {
	withField := baseInput() // ResolvedImageDigest is "" by default
	withField.ResolvedImageDigest = ""
	assert.Equal(t, baseInput().Compute(), withField.Compute(),
		"empty resolved digest must not change the hash")
}

// TestCompute_ResolvedDigestChangesHash asserts that folding a resolved digest
// into the input changes the cache key. A pinned tag is no longer hashed by its
// mutable name alone.
func TestCompute_ResolvedDigestChangesHash(t *testing.T) {
	tagOnly := baseInput()
	pinned := baseInput()
	pinned.ResolvedImageDigest = "sha256:aaaa"
	assert.NotEqual(t, tagOnly.Compute(), pinned.Compute(),
		"adding a resolved digest must change the key")
}

// TestCompute_MovingTagMisses is the core correctness invariant for digest
// pinning: the same image tag resolving to two different content digests must
// produce two different cache keys, so a moving :latest is a cache miss rather
// than a stale hit.
func TestCompute_MovingTagMisses(t *testing.T) {
	old := baseInput()
	old.Image = "app:latest"
	old.ResolvedImageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

	moved := baseInput()
	moved.Image = "app:latest" // identical mutable tag
	moved.ResolvedImageDigest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	assert.NotEqual(t, old.Compute(), moved.Compute(),
		"a tag that moves to a new digest must miss the cache")
}

// TestCompute_SameDigestHits asserts the steady-state path: the same tag
// re-resolving to the same digest yields the same key (a cache hit), so a
// stable pinned image pays no correctness penalty.
func TestCompute_SameDigestHits(t *testing.T) {
	first := baseInput()
	first.Image = "app:latest"
	first.ResolvedImageDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

	second := baseInput()
	second.Image = "app:latest"
	second.ResolvedImageDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

	assert.Equal(t, first.Compute(), second.Compute(),
		"an unchanged pinned digest must keep hitting the cache")
}

func TestCompute_DifferentCommand(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Command = []string{"echo", "world"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentEnv(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Env = map[string]string{"FOO": "changed", "BAZ": "qux"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentWorkDir(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.WorkDir = "/other"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentMounts(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Mounts = []container.Mount{
		{Type: container.MountTypeBind, Source: "/other", Target: "/container", ReadOnly: false},
	}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentPredecessorHashes(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.PredecessorHashes = []string{"abc123", "zzz999"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentRunParams(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.RunParams = map[string]string{"param1": "changed"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentCacheVersion(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.CacheVersion = 2
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_EnvOrderDoesNotMatter(t *testing.T) {
	a := HashInput{
		Env: map[string]string{"A": "1", "B": "2", "C": "3"},
	}
	b := HashInput{
		Env: map[string]string{"C": "3", "A": "1", "B": "2"},
	}
	assert.Equal(t, a.Compute(), b.Compute(), "env var order should not affect hash")
}

func TestCompute_PredecessorHashOrderDoesNotMatter(t *testing.T) {
	a := HashInput{
		PredecessorHashes: []string{"hash1", "hash2", "hash3"},
	}
	b := HashInput{
		PredecessorHashes: []string{"hash3", "hash1", "hash2"},
	}
	assert.Equal(t, a.Compute(), b.Compute(), "predecessor hash order should not affect hash")
}

func TestCompute_EmptyAndNilInputs(t *testing.T) {
	a := HashInput{}
	b := HashInput{
		Env:                nil,
		Mounts:             nil,
		PredecessorHashes:  nil,
		PredecessorOutputs: nil,
		RunParams:          nil,
		Command:            nil,
	}
	h1 := a.Compute()
	h2 := b.Compute()
	require.Equal(t, h1, h2, "empty and nil inputs should produce same hash")
	assert.Len(t, h1, 64)
}

func TestCompute_EmptyVsPopulatedDiffers(t *testing.T) {
	a := HashInput{}
	b := baseInput()
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DifferentPredecessorOutputs(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.PredecessorOutputs = map[string]map[string]string{"step1": {"key": "different"}}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

// --- CanonicalJSON (persisted decomposed HashInput blob) ---

// unmarshalBlob decodes a CanonicalJSON blob, failing the test on error.
func unmarshalBlob(t *testing.T, data []byte) HashInputBlob {
	t.Helper()
	var blob HashInputBlob
	require.NoError(t, json.Unmarshal(data, &blob))
	return blob
}

// canonicalBlob serializes in, supplying its own Compute() digest the way the
// production write-path does (Compute is called once, then reused).
func canonicalBlob(t *testing.T, in HashInput) ([]byte, error) {
	t.Helper()
	return in.CanonicalJSON(in.Compute())
}

// TestCanonicalJSON_Deterministic asserts the serialization is stable: the same
// input (including unordered maps) yields byte-identical blobs, so two runs are
// comparable and dedup/diff logic is sound.
func TestCanonicalJSON_Deterministic(t *testing.T) {
	a, err := canonicalBlob(t, baseInput())
	require.NoError(t, err)
	b, err := canonicalBlob(t, baseInput())
	require.NoError(t, err)
	assert.Equal(t, a, b, "same input must serialize to byte-identical blobs")

	// Reorder the map literals; encoding/json sorts keys, so output is identical.
	reordered := baseInput()
	reordered.Env = map[string]string{"BAZ": "qux", "FOO": "bar"}
	reordered.RunParams = map[string]string{"param1": "value1"}
	c, err := canonicalBlob(t, reordered)
	require.NoError(t, err)
	assert.Equal(t, a, c, "map ordering must not affect the canonical blob")
}

// TestCanonicalJSON_BlobHashMatchesCompute asserts the blob carries the same
// digest Compute() produces, so a reader can confirm a blob decomposes the
// persisted TaskRun.Hash before trusting it.
func TestCanonicalJSON_BlobHashMatchesCompute(t *testing.T) {
	in := baseInput()
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)
	blob := unmarshalBlob(t, data)
	assert.Equal(t, in.Compute(), blob.Hash)
	assert.Equal(t, HashInputBlobVersion, blob.BlobVersion)
}

// TestCanonicalJSON_FieldsRoundTrip asserts the non-redacted fields survive
// serialization verbatim — these are what `caesium why` diffs field-by-field.
func TestCanonicalJSON_FieldsRoundTrip(t *testing.T) {
	in := baseInput()
	in.ResolvedImageDigest = "sha256:abc"
	in.WorkDir = "/app"
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)
	blob := unmarshalBlob(t, data)

	assert.Equal(t, in.JobAlias, blob.JobAlias)
	assert.Equal(t, in.TaskName, blob.TaskName)
	assert.Equal(t, in.Image, blob.Image)
	assert.Equal(t, in.ResolvedImageDigest, blob.ResolvedImageDigest)
	assert.Equal(t, in.Command, blob.Command)
	assert.Equal(t, in.WorkDir, blob.WorkDir)
	assert.Equal(t, in.Mounts, blob.Mounts)
	assert.Equal(t, in.CacheVersion, blob.CacheVersion)
	// Predecessor outputs are typed data-contract values (not secrets) and are
	// stored verbatim so `why` can show the before/after.
	assert.Equal(t, in.PredecessorOutputs, blob.PredecessorOutputs)
	// Predecessor hashes are stored sorted (matching Compute's fold order).
	assert.Equal(t, []string{"abc123", "def456"}, blob.PredecessorHashes)
}

// TestCanonicalJSON_MountOrderMatchesHash is the P1 correctness invariant: the
// blob must list mounts and volume mounts in the SAME canonical (sorted) order
// Compute() hashes them, so two runs whose mounts differ only by insertion
// order produce identical blobs — `caesium why` must never report a spurious
// mount-reorder that had no effect on the hash.
func TestCanonicalJSON_MountOrderMatchesHash(t *testing.T) {
	mountsA := []container.Mount{
		{Type: container.MountTypeBind, Source: "/a", Target: "/x", ReadOnly: true},
		{Type: container.MountTypeBind, Source: "/b", Target: "/y", ReadOnly: false},
	}
	mountsB := []container.Mount{ // same set, reversed insertion order
		{Type: container.MountTypeBind, Source: "/b", Target: "/y", ReadOnly: false},
		{Type: container.MountTypeBind, Source: "/a", Target: "/x", ReadOnly: true},
	}
	volA := []container.VolumeMount{
		{Name: "v1", Type: container.VolumeMountTypeVolume, Target: "/d1"},
		{Name: "v2", Type: container.VolumeMountTypeVolume, Target: "/d2"},
	}
	volB := []container.VolumeMount{
		{Name: "v2", Type: container.VolumeMountTypeVolume, Target: "/d2"},
		{Name: "v1", Type: container.VolumeMountTypeVolume, Target: "/d1"},
	}

	a := baseInput()
	a.Mounts, a.ResolvedVolumeMounts = mountsA, volA
	b := baseInput()
	b.Mounts, b.ResolvedVolumeMounts = mountsB, volB

	// Compute() already treats them as equal; the blob must agree.
	require.Equal(t, a.Compute(), b.Compute(), "precondition: mount order must not affect the hash")

	da, err := canonicalBlob(t, a)
	require.NoError(t, err)
	db, err := canonicalBlob(t, b)
	require.NoError(t, err)
	assert.Equal(t, da, db, "mount insertion order must not change the canonical blob")

	// The stored order is the sorted order, not the insertion order.
	blob := unmarshalBlob(t, db)
	require.Len(t, blob.Mounts, 2)
	assert.Equal(t, "/a", blob.Mounts[0].Source, "mounts must be stored in sorted order")
	assert.Equal(t, "/b", blob.Mounts[1].Source)
	require.Len(t, blob.ResolvedVolumeMounts, 2)
	assert.Equal(t, "v1", blob.ResolvedVolumeMounts[0].Name, "volume mounts must be stored in sorted order")
	assert.Equal(t, "v2", blob.ResolvedVolumeMounts[1].Name)
}

// TestCanonicalJSON_RedactsNonSecretEnvValues is the core guardrail: a plain env
// value (which could be a credential injected as a literal) is never persisted
// verbatim — only a digest of it appears, and the digest matches sha256(value).
func TestCanonicalJSON_RedactsNonSecretEnvValues(t *testing.T) {
	in := baseInput()
	in.Env = map[string]string{"API_TOKEN": "super-secret-literal-value"}
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)

	assert.NotContains(t, string(data), "super-secret-literal-value",
		"a literal env value must never appear in the persisted blob")

	blob := unmarshalBlob(t, data)
	ev, ok := blob.Env["API_TOKEN"]
	require.True(t, ok, "env key must be retained so `why` can name the changed var")
	require.NotNil(t, ev.Redacted)
	assert.True(t, ev.Redacted.Redacted)
	assert.Empty(t, ev.Secret)

	sum := sha256.Sum256([]byte("super-secret-literal-value"))
	assert.Equal(t, "sha256:"+hex.EncodeToString(sum[:]), ev.Redacted.Digest)
}

// TestCanonicalJSON_SecretReferencesStoredVerbatim asserts a secret:// reference
// (a non-secret pointer resolved after hashing) is kept verbatim — it is the
// informative thing to show and carries no credential material.
func TestCanonicalJSON_SecretReferencesStoredVerbatim(t *testing.T) {
	in := baseInput()
	in.Env = map[string]string{"DB_PASSWORD": "secret://vault/db/password"}
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)
	blob := unmarshalBlob(t, data)

	ev, ok := blob.Env["DB_PASSWORD"]
	require.True(t, ok)
	assert.Equal(t, "secret://vault/db/password", ev.Secret)
	assert.Nil(t, ev.Redacted)
}

// TestCanonicalJSON_RedactionDistinguishesValues asserts two different non-secret
// env values produce different redacted digests, so `caesium why` can still
// detect an env change field-by-field without seeing the plaintext.
func TestCanonicalJSON_RedactionDistinguishesValues(t *testing.T) {
	a := baseInput()
	a.Env = map[string]string{"FOO": "value-1"}
	b := baseInput()
	b.Env = map[string]string{"FOO": "value-2"}

	da, err := canonicalBlob(t, a)
	require.NoError(t, err)
	db, err := canonicalBlob(t, b)
	require.NoError(t, err)
	assert.NotEqual(t, da, db, "a changed env value must change the redacted blob")

	// ...and an unchanged value yields an identical digest (a stable diff).
	c := baseInput()
	c.Env = map[string]string{"FOO": "value-1"}
	dc, err := canonicalBlob(t, c)
	require.NoError(t, err)
	assert.Equal(t, da, dc)
}

// TestCanonicalJSON_OversizedDegrades asserts that a blob exceeding the size
// bound degrades to a compact marker (identity + counts, verbatim fields
// dropped) rather than persisting an unbounded payload into dqlite.
func TestCanonicalJSON_OversizedDegrades(t *testing.T) {
	in := baseInput()
	// Build a predecessor-output set large enough to blow past the bound: 200
	// distinct steps, each emitting a ~1 KB value (~200 KB total > 64 KB).
	in.PredecessorOutputs = map[string]map[string]string{}
	big := strings.Repeat("x", 1024)
	for i := 0; i < 200; i++ {
		step := "step-" + strconv.Itoa(i)
		in.PredecessorOutputs[step] = map[string]string{"out": big}
	}

	data, err := canonicalBlob(t, in)
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), maxHashInputBlobBytes,
		"oversized blob must be bounded")

	blob := unmarshalBlob(t, data)
	require.NotNil(t, blob.Oversized, "an over-bound blob must carry the oversized marker")
	assert.Equal(t, in.Compute(), blob.Hash, "identity digest survives degradation")
	assert.Equal(t, len(in.PredecessorOutputs), blob.Oversized.PredecessorOutputCount)
	// Verbatim fields are dropped on degradation.
	assert.Nil(t, blob.PredecessorOutputs)
	assert.Nil(t, blob.Env)
}

// TestCanonicalJSON_EmptyInput asserts the empty input serializes cleanly to a
// minimal, parseable blob (version + zero-value identity), never an error.
func TestCanonicalJSON_EmptyInput(t *testing.T) {
	data, err := canonicalBlob(t, HashInput{})
	require.NoError(t, err)
	blob := unmarshalBlob(t, data)
	assert.Equal(t, HashInputBlobVersion, blob.BlobVersion)
	assert.Equal(t, HashInput{}.Compute(), blob.Hash)
	assert.Nil(t, blob.Env)
	assert.Nil(t, blob.Oversized)
}

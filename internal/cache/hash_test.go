package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/container"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
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

// --- Large-object reference digest folds into the hash (design Component 5/D1) ---
//
// A reference output is carried in PredecessorOutputs as a pkg/task.OutputRef
// encoded value, whose content digest is part of the encoding. These tests pin
// the invariant that makes value-verified skip sound: the digest, and only the
// digest's content, decides cache equality.

// refValue builds the encoded reference value exactly as the producer-side
// parser stores it, so the hash sees the same bytes production would.
func refValue(t *testing.T, path, digest string) string {
	t.Helper()
	return pkgtask.OutputRef{Ref: 1, Path: path, Digest: digest, Size: 1 << 20}.Encode()
}

// TestCompute_ReferenceDigestChangesHash: a changed payload digest in a
// predecessor reference output must change the consuming task's hash (a miss),
// so a changed large object never serves a stale downstream result.
func TestCompute_ReferenceDigestChangesHash(t *testing.T) {
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)

	a := baseInput()
	a.PredecessorOutputs = map[string]map[string]string{"extract": {"frame": refValue(t, "/data/out.bin", digestA)}}
	b := baseInput()
	b.PredecessorOutputs = map[string]map[string]string{"extract": {"frame": refValue(t, "/data/out.bin", digestB)}}

	assert.NotEqual(t, a.Compute(), b.Compute(), "different reference digest must change the hash")
}

// TestCompute_ByteIdenticalReferenceSameHash: a byte-identical payload (same
// digest) yields an identical hash even if the path differs — equality tracks
// content, not location. This is the substrate for the value-verified skip.
func TestCompute_ByteIdenticalReferenceSameHash(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)

	a := baseInput()
	a.PredecessorOutputs = map[string]map[string]string{"extract": {"frame": refValue(t, "/data/run-1/out.bin", digest)}}
	b := baseInput()
	b.PredecessorOutputs = map[string]map[string]string{"extract": {"frame": refValue(t, "/data/run-1/out.bin", digest)}}

	assert.Equal(t, a.Compute(), b.Compute(), "identical reference must produce identical hash")
}

// TestCompute_ReferenceVsScalarDiffers: a reference output and a scalar output
// under the same key are not interchangeable — the encoded reference is a
// distinct value, so the hash distinguishes them.
func TestCompute_ReferenceVsScalarDiffers(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)

	ref := baseInput()
	ref.PredecessorOutputs = map[string]map[string]string{"extract": {"frame": refValue(t, "/data/out.bin", digest)}}
	scalar := baseInput()
	scalar.PredecessorOutputs = map[string]map[string]string{"extract": {"frame": "small-value"}}

	assert.NotEqual(t, ref.Compute(), scalar.Compute())
}

// TestCompute_NoReferenceHashUnchanged: the reference machinery is inert on the
// default (scalar-only) path. The scalar-only hash is deterministic, and adding
// a reference output is what — and the only thing that — perturbs it, so a job
// that never emits a reference hashes exactly as it did pre-D1.
func TestCompute_NoReferenceHashUnchanged(t *testing.T) {
	got := baseInput().Compute()
	assert.Equal(t, baseInput().Compute(), got, "scalar-only hash must be deterministic")

	withRef := baseInput()
	digest := "sha256:" + strings.Repeat("e", 64)
	withRef.PredecessorOutputs = map[string]map[string]string{
		"step1":   {"key": "val"}, // identical to baseInput's scalar output
		"extract": {"frame": refValue(t, "/data/out.bin", digest)},
	}
	assert.NotEqual(t, got, withRef.Compute(), "adding a reference output must change the hash")
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

// --- Kueue queue name (scheduling metadata, excluded from identity) ---

// TestCompute_KueueQueueNameExcluded is the B1 cache-identity guarantee: the
// Kueue queue is scheduling metadata, not an execution input, so two otherwise
// identical tasks that differ ONLY in queue name must produce the SAME hash —
// changing the queue must never bust the cache.
func TestCompute_KueueQueueNameExcluded(t *testing.T) {
	a := baseInput()
	a.Kubernetes = &container.KubernetesSpec{QueueName: "team-a"}
	b := baseInput()
	b.Kubernetes = &container.KubernetesSpec{QueueName: "team-b"}
	assert.Equal(t, a.Compute(), b.Compute(),
		"queue name is scheduling metadata and must not change the cache hash")
}

// TestCompute_KueueQueueOnlyEqualsNoKubernetes asserts that a KubernetesSpec
// whose only populated field is QueueName hashes byte-identically to a task with
// no KubernetesSpec at all. Without this, setting a queue on an otherwise
// non-k8s-identity task would silently bust its cache.
func TestCompute_KueueQueueOnlyEqualsNoKubernetes(t *testing.T) {
	noK8s := baseInput()
	queueOnly := baseInput()
	queueOnly.Kubernetes = &container.KubernetesSpec{QueueName: "team-a"}
	assert.Equal(t, noK8s.Compute(), queueOnly.Compute(),
		"a queue-only KubernetesSpec carries no identity and must match an absent one")
}

// TestCompute_KueueQueueNameDoesNotMaskIdentityFields guards the inverse: adding
// a queue must not erase the contribution of real identity fields. The hash with
// identity fields present must still differ from one without them, whether or not
// a queue is also set.
func TestCompute_KueueQueueNameDoesNotMaskIdentityFields(t *testing.T) {
	plain := baseInput()
	withSA := baseInput()
	withSA.Kubernetes = &container.KubernetesSpec{ServiceAccountName: "deployer"}
	withSAAndQueue := baseInput()
	withSAAndQueue.Kubernetes = &container.KubernetesSpec{ServiceAccountName: "deployer", QueueName: "team-a"}

	assert.NotEqual(t, plain.Compute(), withSA.Compute(),
		"service account is an identity field and must change the hash")
	assert.Equal(t, withSA.Compute(), withSAAndQueue.Compute(),
		"adding a queue on top of identity fields must not change the hash")
}

// TestCanonicalJSON_KueueQueueNameStrippedFromBlob asserts the persisted
// decomposed blob (the basis of `caesium why`) never records the queue name, so
// the identity record stays in lockstep with the hash — a queue change must not
// even appear as a field-level diff.
func TestCanonicalJSON_KueueQueueNameStrippedFromBlob(t *testing.T) {
	in := baseInput()
	in.Kubernetes = &container.KubernetesSpec{ServiceAccountName: "deployer", QueueName: "team-a"}
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)

	// The raw JSON must not mention the queue at all.
	assert.NotContains(t, string(data), "team-a", "queue name must not appear in the blob")
	assert.NotContains(t, string(data), "queueName", "queueName key must not appear in the blob")

	blob := unmarshalBlob(t, data)
	require.NotNil(t, blob.Kubernetes, "identity-bearing k8s fields must still be recorded")
	assert.Equal(t, "deployer", blob.Kubernetes.ServiceAccountName)
	assert.Empty(t, blob.Kubernetes.QueueName, "queue name must be stripped from the persisted blob")
}

// TestCanonicalJSON_KueueQueueOnlyOmitsKubernetes asserts that when the queue was
// the only reason a KubernetesSpec existed, the blob omits the Kubernetes object
// entirely — matching Compute(), which skips it unless HasIdentityFields.
func TestCanonicalJSON_KueueQueueOnlyOmitsKubernetes(t *testing.T) {
	in := baseInput()
	in.Kubernetes = &container.KubernetesSpec{QueueName: "team-a"}
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)
	blob := unmarshalBlob(t, data)
	assert.Nil(t, blob.Kubernetes, "a queue-only KubernetesSpec must not appear in the blob")
}

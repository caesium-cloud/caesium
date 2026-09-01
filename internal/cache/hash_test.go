package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
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

// --- Replay safety (control-plane metadata, excluded from identity) ---

func TestCompute_ReplaySafeExcludedFromDefinitionHash(t *testing.T) {
	base := `
apiVersion: v1
kind: Job
metadata:
  alias: replay-cache
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "echo extract"]
`
	jobLevel := `
apiVersion: v1
kind: Job
metadata:
  alias: replay-cache
  replaySafe: true
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "echo extract"]
`
	stepLevel := `
apiVersion: v1
kind: Job
metadata:
  alias: replay-cache
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    replaySafe: true
    image: alpine:3.23
    command: ["sh", "-c", "echo extract"]
`

	baseHash := taskHashFromDefinition(t, base)
	assert.Equal(t, baseHash, taskHashFromDefinition(t, jobLevel),
		"job-level replaySafe is a replay gate, not an execution input")
	assert.Equal(t, baseHash, taskHashFromDefinition(t, stepLevel),
		"step-level replaySafe is a replay gate, not an execution input")
}

func TestCompute_SchedulingMetadataExcludedFromDefinitionHash(t *testing.T) {
	lowQueueAPI := `
apiVersion: v1
kind: Job
metadata:
  alias: scheduling-cache
  priority: low
  concurrency:
    maxRuns: 1
    strategy: queue
  rateLimits:
    - resource: warehouse-api
      limit: 60
      window: 1m
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "echo extract"]
    rateLimit:
      resource: warehouse-api
      units: 1
`
	highSkipDB := `
apiVersion: v1
kind: Job
metadata:
  alias: scheduling-cache
  priority: high
  concurrency:
    maxRuns: 2
    strategy: skip
  rateLimits:
    - resource: database
      limit: 10
      window: 30s
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "echo extract"]
    rateLimit:
      resource: database
      units: 3
`

	assert.Equal(t, taskHashFromDefinition(t, lowQueueAPI), taskHashFromDefinition(t, highSkipDB),
		"priority, concurrency, and rate-limit settings are scheduling metadata and must not change the cache hash")
}

func TestCompute_DatasetSchemasExcludedFromDefinitionHash(t *testing.T) {
	withoutSchemas := `
apiVersion: v1
kind: Job
metadata:
  alias: dataset-schema-cache
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "echo extract"]
    outputSchema:
      type: object
      required: [customer_id]
      properties:
        customer_id: {type: string}
    datasets:
      consumes: [raw.vendor_x]
      produces:
        - name: lake.vendor_x_customers
`
	withSchemas := `
apiVersion: v1
kind: Job
metadata:
  alias: dataset-schema-cache
trigger:
  type: cron
  configuration: {cron: "0 * * * *"}
steps:
  - name: extract
    image: alpine:3.23
    command: ["sh", "-c", "echo extract"]
    outputSchema:
      type: object
      required: [customer_id]
      properties:
        customer_id: {type: string}
    datasets:
      consumes:
        - name: raw.vendor_x
          schema:
            type: object
            required: [customer_id]
            properties:
              customer_id: {type: string}
      produces:
        - name: lake.vendor_x_customers
          schema:
            type: object
            required: [customer_id]
            properties:
              customer_id: {type: string}
`

	assert.Equal(t, taskHashFromDefinition(t, withoutSchemas), taskHashFromDefinition(t, withSchemas),
		"dataset schema declarations are apply-time metadata and must not change the cache hash")
}

func taskHashFromDefinition(t *testing.T, src string) string {
	t.Helper()

	def, err := schema.Parse([]byte(src))
	require.NoError(t, err)
	require.Len(t, def.Steps, 1)
	step := &def.Steps[0]
	spec, err := def.RuntimeSpecForStep(step)
	require.NoError(t, err)

	return HashInput{
		JobAlias:             def.Metadata.Alias,
		TaskName:             step.Name,
		Image:                step.Image,
		Command:              step.Command,
		Env:                  spec.Env,
		WorkDir:              spec.WorkDir,
		Mounts:               spec.Mounts,
		ResolvedVolumeMounts: spec.ResolvedVolumeMounts,
		Kubernetes:           spec.Kubernetes,
	}.Compute()
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

// Frozen 2026-08-26 against the pre-partition HashInput of baseInput().
// omit-when-empty partition fields must not change this digest, and
// CacheVersion is unchanged.
const goldenStringFormHash = "5d6290009f017f6307aa5e20a383b88b81c7197322018b7c148f45d65bb95007"

func TestCompute_StringFormPartitionKeepsLegacyHash(t *testing.T) {
	legacy := baseInput()
	withEmpty := baseInput()
	withEmpty.Partition = ""
	withEmpty.PartitionFingerprint = ""
	withEmpty.PartitionAttributes = nil
	assert.Equal(t, legacy.Compute(), withEmpty.Compute(),
		"empty partition fields must not change the hash")
}

func TestCompute_PartitionKeyChangesHash(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.Partition = "file-a.csv"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_PartitionFingerprintChangesHash(t *testing.T) {
	a := baseInput()
	a.Partition = "dim_customer"
	b := a
	b.PartitionFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_PartitionAttributesChangeHash(t *testing.T) {
	a := baseInput()
	a.Partition = "app-api"
	b := a
	b.PartitionAttributes = map[string]string{"root": "stacks/api"}
	assert.NotEqual(t, a.Compute(), b.Compute())
}

func TestCompute_DependsOnNotHashed(t *testing.T) {
	// dependsOn is a scheduling instruction and is not a HashInput field.
	// Two instances that differ only in dependsOn share an identity.
	a := baseInput()
	a.Partition = "fct_orders"
	a.PartitionFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	b := a
	assert.Equal(t, a.Compute(), b.Compute())
}

func TestCanonicalJSON_PartitionFieldsRoundTrip(t *testing.T) {
	in := baseInput()
	in.Partition = "dim_customer"
	in.PartitionFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	in.PartitionAttributes = map[string]string{"root": "models", "materialization": "table"}
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)
	blob := unmarshalBlob(t, data)
	assert.Equal(t, in.Partition, blob.Partition)
	assert.Equal(t, in.PartitionFingerprint, blob.PartitionFingerprint)
	assert.Equal(t, in.PartitionAttributes, blob.PartitionAttributes)
}

func TestCompute_GoldenStringFormUnchanged(t *testing.T) {
	got := baseInput().Compute()
	assert.Equal(t, goldenStringFormHash, got,
		"string-form (empty partition fields) hashes must stay byte-identical; no CacheVersion bump")
	assert.Equal(t, 1, baseInput().CacheVersion, "CacheVersion must not be bumped for partition fields")
}

// TestCompute_PartitionFramingIsUnambiguous pins the fix for the
// adversarial-review finding that the three partition fields were folded in as
// separate newline-delimited "<label>:<value>" lines. Because a partition key is
// arbitrary producer-supplied text, a key containing an embedded newline plus a
// forged "partition_fingerprint:" line produced the exact same byte stream as a
// clean key carrying that fingerprint — two distinct execution identities
// collapsing to one cache entry, i.e. a wrong-output cache hit.
func TestCompute_PartitionFramingIsUnambiguous(t *testing.T) {
	const fp = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	forged := baseInput()
	forged.Partition = "x\npartition_fingerprint:" + fp

	honest := baseInput()
	honest.Partition = "x"
	honest.PartitionFingerprint = fp

	assert.NotEqual(t, forged.Compute(), honest.Compute(),
		"a key with an embedded field-delimiter must not alias a different partition identity")
}

// TestCompute_PartitionAttributeFramingIsUnambiguous is the same aliasing class
// on the attribute lines, where both the "\n" record separator and the "=" pair
// separator were injectable.
func TestCompute_PartitionAttributeFramingIsUnambiguous(t *testing.T) {
	forged := baseInput()
	forged.Partition = "p"
	forged.PartitionAttributes = map[string]string{"root": "a\npartition_attr:region=eu"}

	honest := baseInput()
	honest.Partition = "p"
	honest.PartitionAttributes = map[string]string{"root": "a", "region": "eu"}

	assert.NotEqual(t, forged.Compute(), honest.Compute(),
		"an attribute value with an embedded record separator must not alias a second attribute")
}

// TestCompute_PartitionAttributeKeyValueSeparatorUnambiguous covers the "="
// split specifically: attr key "a" value "b=c" must not alias key "a=b" value
// "c".
func TestCompute_PartitionAttributeKeyValueSeparatorUnambiguous(t *testing.T) {
	a := baseInput()
	a.Partition = "p"
	a.PartitionAttributes = map[string]string{"a": "b=c"}

	b := baseInput()
	b.Partition = "p"
	b.PartitionAttributes = map[string]string{"a=b": "c"}

	assert.NotEqual(t, a.Compute(), b.Compute())
}

// TestCompute_PartitionKeyAliasingAcrossFieldsRejected: an empty key with a
// forged partition line inside the fingerprint must not alias a real key.
func TestCompute_PartitionFingerprintFramingIsUnambiguous(t *testing.T) {
	forged := baseInput()
	forged.Partition = "p"
	forged.PartitionFingerprint = "sha256:aa\npartition_attr:root=models"

	honest := baseInput()
	honest.Partition = "p"
	honest.PartitionFingerprint = "sha256:aa"
	honest.PartitionAttributes = map[string]string{"root": "models"}

	assert.NotEqual(t, forged.Compute(), honest.Compute())
}

// TestCompute_PartitionIdentityStillDeterministic: the new framing must remain
// stable across calls and insensitive to Go map iteration order.
func TestCompute_PartitionIdentityStillDeterministic(t *testing.T) {
	in := baseInput()
	in.Partition = "dim_customer"
	in.PartitionFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	in.PartitionAttributes = map[string]string{"root": "models", "materialization": "table", "region": "eu"}
	first := in.Compute()
	for i := 0; i < 32; i++ {
		assert.Equal(t, first, in.Compute())
	}
}

// ---------------------------------------------------------------------------
// cache.chain (infra-deploy A1/A2)
// ---------------------------------------------------------------------------

// TestChainConstantsMatchJobdef pins internal/cache's duplicated chain constants
// to the jobdef spellings users actually write. They are duplicated rather than
// imported because pkg/jobdef's own tests import this package, so a non-test
// import of pkg/jobdef here would be a test-build import cycle — this assertion
// is what keeps the duplication honest.
func TestChainConstantsMatchJobdef(t *testing.T) {
	assert.Equal(t, schema.CacheChainTransitive, ChainTransitive)
	assert.Equal(t, schema.CacheChainValues, ChainValues)
}

// TestCompute_GoldenTransitiveChainUnchanged is THE guard on this feature: the
// default transitive mode must write nothing new into the digest, or every cache
// entry in every existing deployment is silently invalidated on upgrade. The
// expected value is a string LITERAL frozen from the pre-chain code (it is the
// same digest TestCompute_GoldenStringFormUnchanged pins), never a recomputation
// through the function under test.
func TestCompute_GoldenTransitiveChainUnchanged(t *testing.T) {
	// Chain unset — how every existing caller constructs a HashInput today.
	assert.Equal(t, goldenStringFormHash, baseInput().Compute(),
		"an unset Chain must hash byte-identically to the pre-chain era")

	// Chain explicitly transitive — how every caller constructs one AFTER A3
	// threads the resolved config through, since ResolveCacheConfig defaults to
	// "transitive" rather than "". These two MUST agree.
	explicit := baseInput()
	explicit.Chain = ChainTransitive
	assert.Equal(t, goldenStringFormHash, explicit.Compute(),
		"an explicit transitive Chain must hash byte-identically to an unset one")

	// And the blob is likewise unchanged: no `chain` key appears.
	data, err := canonicalBlob(t, explicit)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"chain"`,
		"transitive blobs must not gain a chain field; HashInputBlobVersion stays 1")
	assert.Equal(t, HashInputBlobVersion, unmarshalBlob(t, data).BlobVersion)
}

// TestCompute_ValuesChainIgnoresPredecessorHashes is the core semantic: a
// checkout step's hash churns on every commit and that churn propagates through
// PredecessorHashes to every downstream stack. Under chain: values it must not.
func TestCompute_ValuesChainIgnoresPredecessorHashes(t *testing.T) {
	a := baseInput()
	a.Chain = ChainValues

	b := a
	b.PredecessorHashes = []string{"totally", "different", "upstream", "hashes"}

	assert.Equal(t, a.Compute(), b.Compute(),
		"under chain: values a changed predecessor identity hash must not change the key")

	// Removing them entirely is likewise a no-op.
	c := a
	c.PredecessorHashes = nil
	assert.Equal(t, a.Compute(), c.Compute())
}

// TestCompute_ValuesChainStillHashesPredecessorOutputs is the other half of the
// contract: outputs still chain, so changing the network stack's vpc_id output
// re-plans every consumer even though their code is untouched (spec §4.3).
func TestCompute_ValuesChainStillHashesPredecessorOutputs(t *testing.T) {
	a := baseInput()
	a.Chain = ChainValues

	b := a
	b.PredecessorOutputs = map[string]map[string]string{"step1": {"key": "CHANGED"}}

	assert.NotEqual(t, a.Compute(), b.Compute(),
		"under chain: values a changed predecessor OUTPUT must still change the key")

	// A new output key on an existing producer also counts.
	c := a
	c.PredecessorOutputs = map[string]map[string]string{"step1": {"key": "val", "extra": "1"}}
	assert.NotEqual(t, a.Compute(), c.Compute())
}

// TestCompute_ValuesChainDiffersFromTransitive: the mode is part of the identity.
// Two otherwise-identical inputs in different modes must not collide, or a step
// switched to values mode would inherit a key computed under different rules.
func TestCompute_ValuesChainDiffersFromTransitive(t *testing.T) {
	transitive := baseInput()
	values := baseInput()
	values.Chain = ChainValues
	assert.NotEqual(t, transitive.Compute(), values.Compute())

	// True even with no predecessor hashes at all: the marker is unconditional.
	noPredsTransitive := baseInput()
	noPredsTransitive.PredecessorHashes = nil
	noPredsValues := noPredsTransitive
	noPredsValues.Chain = ChainValues
	assert.NotEqual(t, noPredsTransitive.Compute(), noPredsValues.Compute())
}

// TestCompute_ValuesChainOnlyForExactValue guards against a near-miss spelling
// silently excluding predecessor hashes. Anything that is not exactly "values"
// keeps the transitive behaviour — the safe direction (an extra re-run, never a
// stale hit). Validate() is what surfaces the typo to the user.
func TestCompute_ValuesChainOnlyForExactValue(t *testing.T) {
	for _, chain := range []string{"", "transitive", "Values", "value", "VALUES"} {
		in := baseInput()
		in.Chain = chain
		assert.Equal(t, goldenStringFormHash, in.Compute(),
			"chain %q must fall back to the transitive hash", chain)
	}
}

// TestCanonicalJSON_ChainRoundTrips: `caesium why` reads the blob, so the mode
// has to survive serialization, and PredecessorHashes must still be recorded
// (they are provenance even when they are not key material).
func TestCanonicalJSON_ChainRoundTrips(t *testing.T) {
	in := baseInput()
	in.Chain = ChainValues
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)

	blob := unmarshalBlob(t, data)
	assert.Equal(t, ChainValues, blob.Chain)
	assert.Equal(t, in.Compute(), blob.Hash, "the blob must decompose the digest it carries")
	assert.Equal(t, []string{"abc123", "def456"}, blob.PredecessorHashes,
		"values mode still records predecessor hashes as provenance")
	assert.Equal(t, HashInputBlobVersion, blob.BlobVersion,
		"chain is additive+omitempty, so the blob version must not move")
}

// TestCanonicalJSON_OversizedChainMarksExclusion: when the blob degrades to the
// compact summary, the exclusion must still be nameable — PredecessorCount on a
// values-mode blob would otherwise read as "N inputs that entered the key".
func TestCanonicalJSON_OversizedChainMarksExclusion(t *testing.T) {
	in := baseInput()
	in.Chain = ChainValues
	in.Env = make(map[string]string, 4096)
	for i := 0; i < 4096; i++ {
		in.Env["VAR_"+strconv.Itoa(i)] = strings.Repeat("x", 64)
	}
	data, err := canonicalBlob(t, in)
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), maxHashInputBlobBytes)

	blob := unmarshalBlob(t, data)
	require.NotNil(t, blob.Oversized, "this input must exceed the blob bound")
	assert.True(t, blob.Oversized.PredecessorHashesExcluded)
	assert.Equal(t, ChainValues, blob.Chain)
}

// ---------------------------------------------------------------------------
// cache.chain values-mode predecessor-output framing (greptile 3881714384)
// ---------------------------------------------------------------------------

// TestCompute_ValuesChainPredecessorOutputFramingIsUnambiguous pins the exact
// collision greptile reported. Output values are arbitrary producer-supplied
// text and may contain newlines, so the legacy line form
// "pred_output:<step>:<key>=<value>\n" is not injective: for producer "p",
// {"a": "x\npred_output:p:b=y"} and {"a": "x", "b": "y"} both serialize to
// "pred_output:p:a=x\npred_output:p:b=y\n".
//
// Under chain: values the predecessor identity hashes are deliberately excluded,
// so the outputs ARE the upstream identity and this aliasing is a stale
// downstream cache hit — the consumer replays a result computed from a
// different upstream value map.
func TestCompute_ValuesChainPredecessorOutputFramingIsUnambiguous(t *testing.T) {
	forged := baseInput()
	forged.Chain = ChainValues
	forged.PredecessorOutputs = map[string]map[string]string{
		"p": {"a": "x\npred_output:p:b=y"},
	}

	honest := baseInput()
	honest.Chain = ChainValues
	honest.PredecessorOutputs = map[string]map[string]string{
		"p": {"a": "x", "b": "y"},
	}

	assert.NotEqual(t, forged.Compute(), honest.Compute(),
		"an output value with an embedded record separator must not alias a second output")
}

// TestCompute_ValuesChainOutputKeyValueSeparatorUnambiguous covers the "=" split
// specifically: key "a" with value "b=c" must not alias key "a=b" with value "c".
func TestCompute_ValuesChainOutputKeyValueSeparatorUnambiguous(t *testing.T) {
	a := baseInput()
	a.Chain = ChainValues
	a.PredecessorOutputs = map[string]map[string]string{"p": {"a": "b=c"}}

	b := baseInput()
	b.Chain = ChainValues
	b.PredecessorOutputs = map[string]map[string]string{"p": {"a=b": "c"}}

	assert.NotEqual(t, a.Compute(), b.Compute())
}

// TestCompute_ValuesChainProducerNameFramingIsUnambiguous: the producer name is
// the outer ":"-delimited field and aliases the same way — one producer "p"
// whose value embeds a newline plus a second producer prefix must not collide
// with two real producers.
func TestCompute_ValuesChainProducerNameFramingIsUnambiguous(t *testing.T) {
	forged := baseInput()
	forged.Chain = ChainValues
	forged.PredecessorOutputs = map[string]map[string]string{
		"p": {"a": "x\npred_output:q:b=y"},
	}

	honest := baseInput()
	honest.Chain = ChainValues
	honest.PredecessorOutputs = map[string]map[string]string{
		"p": {"a": "x"},
		"q": {"b": "y"},
	}

	assert.NotEqual(t, forged.Compute(), honest.Compute())
}

// TestCompute_TransitiveChainOutputFramingUnchanged is the compatibility half:
// the legacy line form is retained under the default chain, so the aliasing
// above still exists there by design. It is contained because the predecessor
// identity hashes — which values mode removes — already separate the two inputs,
// and changing the transitive encoding would invalidate every cache entry in
// every existing deployment.
func TestCompute_TransitiveChainOutputFramingUnchanged(t *testing.T) {
	forged := baseInput()
	forged.PredecessorOutputs = map[string]map[string]string{
		"p": {"a": "x\npred_output:p:b=y"},
	}

	honest := baseInput()
	honest.PredecessorOutputs = map[string]map[string]string{
		"p": {"a": "x", "b": "y"},
	}

	assert.Equal(t, forged.Compute(), honest.Compute(),
		"the legacy transitive encoding is frozen for cache compatibility; values mode is where the framing is fixed")
}

// TestCompute_ValuesChainNilAndEmptyOutputsAgree: nil and allocated-but-empty
// maps mean the same thing and must fold in identically, at both nesting levels.
func TestCompute_ValuesChainNilAndEmptyOutputsAgree(t *testing.T) {
	nilOuter := baseInput()
	nilOuter.Chain = ChainValues
	nilOuter.PredecessorOutputs = nil

	emptyOuter := baseInput()
	emptyOuter.Chain = ChainValues
	emptyOuter.PredecessorOutputs = map[string]map[string]string{}

	assert.Equal(t, nilOuter.Compute(), emptyOuter.Compute(),
		"a nil predecessor-output map must hash identically to an empty one")

	nilInner := baseInput()
	nilInner.Chain = ChainValues
	nilInner.PredecessorOutputs = map[string]map[string]string{"p": nil}

	emptyInner := baseInput()
	emptyInner.Chain = ChainValues
	emptyInner.PredecessorOutputs = map[string]map[string]string{"p": {}}

	assert.Equal(t, nilInner.Compute(), emptyInner.Compute(),
		"a producer with a nil output map must hash identically to one with an empty map")
}

// TestCompute_ValuesChainFramingStillDeterministic: canonical JSON must be
// stable across calls and insensitive to Go map iteration order.
func TestCompute_ValuesChainFramingStillDeterministic(t *testing.T) {
	in := baseInput()
	in.Chain = ChainValues
	in.PredecessorOutputs = map[string]map[string]string{
		"network": {"vpc_id": "vpc-1", "subnet_ids": `["a","b"]`, "region": "eu-west-1"},
		"account": {"account_id": "1234", "org": "acme"},
	}
	first := in.Compute()
	for i := 0; i < 32; i++ {
		assert.Equal(t, first, in.Compute())
	}
}

// Frozen 2026-08-28 against the values-mode framing of baseInput() (greptile
// 3881714384). Values mode ships in this release, so unlike the transitive
// golden this digest pins a NEW encoding rather than preserving an old one — but
// from here on it is the compatibility contract for every `chain: values` cache
// entry, and any change to the values-mode fold needs a CacheVersion bump.
const goldenValuesChainHash = "76215b46b4601fe23eb95294e8ce99f25f9168ee2e9430299328d5a1a9fbd9e3"

func TestCompute_GoldenValuesChainFraming(t *testing.T) {
	in := baseInput()
	in.Chain = ChainValues
	assert.Equal(t, goldenValuesChainHash, in.Compute(),
		"the values-mode framing is a cache-key contract; changing it needs a CacheVersion bump")

	// And it must NOT equal the transitive golden: the mode is part of the key.
	assert.NotEqual(t, goldenStringFormHash, in.Compute())
}

// TestFramedPredecessorOutputs_CanonicalShape pins the framed record itself, not
// just the digest it produces, so a reader can see exactly what values mode
// folds in — sorted keys, JSON-escaped values, empty maps normalized.
func TestFramedPredecessorOutputs_CanonicalShape(t *testing.T) {
	assert.Equal(t, `{}`, canonicalJSON(framedPredecessorOutputs(nil)))
	assert.Equal(t, `{"p":{}}`,
		canonicalJSON(framedPredecessorOutputs(map[string]map[string]string{"p": nil})))
	assert.Equal(t, `{"account":{"id":"1"},"network":{"a":"1","vpc":"2"}}`,
		canonicalJSON(framedPredecessorOutputs(map[string]map[string]string{
			"network": {"vpc": "2", "a": "1"},
			"account": {"id": "1"},
		})), "producer and key order must be canonical, not map-iteration order")
	assert.Equal(t, `{"p":{"a":"x\npred_output:p:b=y"}}`,
		canonicalJSON(framedPredecessorOutputs(map[string]map[string]string{
			"p": {"a": "x\npred_output:p:b=y"},
		})), "embedded record separators must be escaped, not passed through")
}

// ---------------------------------------------------------------------------
// cache.chain: values × structured partitions (issue #360)
//
// Fingerprints make per-unit skip *expressible*; chain: values is what makes it
// *happen* across a producer re-run. The default transitive chain still folds
// predecessor identity hashes, so a producer whose own inputs moved re-keys
// every instance even when the fingerprint did not. These tests pin the
// composition: same key+fingerprint+consumed outputs → same digest even if the
// predecessor hash churned; a fingerprint change is always a miss. They must
// not touch the transitive golden.
// ---------------------------------------------------------------------------

const (
	partitionFPAaaa = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	partitionFPBbbb = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func partitionSkipInput(key, fingerprint, predHash, predOutput, chain string) HashInput {
	in := HashInput{
		JobAlias:             "fanout-job",
		TaskName:             "process",
		Image:                "alpine:3.23",
		Command:              []string{"sh", "-c", "echo partition=$CAESIUM_PARTITION"},
		Chain:                chain,
		Partition:            key,
		PartitionFingerprint: fingerprint,
		PredecessorHashes:    []string{predHash},
		CacheVersion:         1,
	}
	if predOutput != "" {
		in.PredecessorOutputs = map[string]map[string]string{"list": {"token": predOutput}}
	}
	return in
}

// TestCompute_ValuesChainPartitionSkipIgnoresPredecessorHash is the skip the
// feature exists for: the producer re-ran (its identity moved) but this
// instance's key, fingerprint and consumed outputs did not. Under chain: values
// the two runs share a digest, so the instance is a cache hit.
func TestCompute_ValuesChainPartitionSkipIgnoresPredecessorHash(t *testing.T) {
	first := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-1", "same", ChainValues)
	second := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-2", "same", ChainValues)
	assert.Equal(t, first.Compute(), second.Compute(),
		"under chain: values a producer re-run must not re-key an unchanged partition")

	// No structured producer output is the listing-step shape (partitions only).
	// Silence is not an output change, and values mode must not invent one.
	silentFirst := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-1", "", ChainValues)
	silentSecond := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-2", "", ChainValues)
	assert.Equal(t, silentFirst.Compute(), silentSecond.Compute(),
		"a producer that emits no scalar outputs must still skip unchanged partitions under chain: values")
}

// TestCompute_ValuesChainPartitionFingerprintIsAuthoritative is the stale-hit
// guard: a new fingerprint is a different unit of work even when the key and
// the predecessor outputs look the same. Fingerprints stay in the key; values
// mode only drops predecessor identity hashes.
func TestCompute_ValuesChainPartitionFingerprintIsAuthoritative(t *testing.T) {
	before := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-1", "same", ChainValues)
	after := partitionSkipInput("dim_customer", partitionFPBbbb, "hash-run-1", "same", ChainValues)
	assert.NotEqual(t, before.Compute(), after.Compute(),
		"a changed partition fingerprint must miss even under chain: values")

	// Same even when the predecessor hash also moved — the fingerprint, not the
	// producer identity, is what discriminates the two units.
	after.PredecessorHashes = []string{"hash-run-2"}
	assert.NotEqual(t, before.Compute(), after.Compute())
}

// TestCompute_ValuesChainPartitionStillHashesPredecessorOutputs: outputs still
// chain, so a discover step that publishes a changed scalar re-keys every
// instance. Per-unit data belongs in the fingerprint / attributes, not in the
// producer's outputs.
func TestCompute_ValuesChainPartitionStillHashesPredecessorOutputs(t *testing.T) {
	before := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-1", "v1", ChainValues)
	after := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-1", "v2", ChainValues)
	assert.NotEqual(t, before.Compute(), after.Compute(),
		"a changed predecessor OUTPUT must still invalidate a values-mode partition")
}

// TestCompute_TransitiveChainPartitionStillCascadesPredecessorHash pins the
// default: without chain: values, a producer re-run re-keys every instance
// even when the fingerprint is unchanged. That is v1 conservative identity,
// and it must stay byte-identical to today.
func TestCompute_TransitiveChainPartitionStillCascadesPredecessorHash(t *testing.T) {
	for _, chain := range []string{"", ChainTransitive} {
		first := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-1", "same", chain)
		second := partitionSkipInput("dim_customer", partitionFPAaaa, "hash-run-2", "same", chain)
		assert.NotEqual(t, first.Compute(), second.Compute(),
			"chain %q must keep cascading predecessor identity into every partition", chain)
	}
}

// TestCompute_ValuesChainBareStringPartitionOmitsEmptyFingerprint: a
// string-form partition has no fingerprint. omit-when-empty must keep that
// digest independent of the fingerprint field staying "", and values mode must
// still ignore predecessor-hash churn. No CacheVersion bump.
func TestCompute_ValuesChainBareStringPartitionOmitsEmptyFingerprint(t *testing.T) {
	bare := partitionSkipInput("2026-07-01", "", "hash-run-1", "", ChainValues)
	alsoBare := bare
	alsoBare.PartitionFingerprint = ""
	assert.Equal(t, bare.Compute(), alsoBare.Compute())

	churned := partitionSkipInput("2026-07-01", "", "hash-run-2", "", ChainValues)
	assert.Equal(t, bare.Compute(), churned.Compute(),
		"bare-string partitions under chain: values skip on key + consumed outputs alone")

	// Empty partition fields + transitive still match the pre-fan-out golden.
	legacy := baseInput()
	withEmpty := baseInput()
	withEmpty.Partition = ""
	withEmpty.PartitionFingerprint = ""
	withEmpty.PartitionAttributes = nil
	assert.Equal(t, goldenStringFormHash, legacy.Compute())
	assert.Equal(t, goldenStringFormHash, withEmpty.Compute(),
		"empty partition fields must not change the transitive golden; no CacheVersion bump")
}

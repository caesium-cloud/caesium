package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/container"
)

// HashInputBlobVersion is the schema version of the persisted decomposed
// HashInput blob (see CanonicalJSON). Bump it whenever the on-disk shape
// changes so a reader (e.g. `caesium why`) can tell two blobs were produced by
// different serializers and avoid diffing incompatible layouts. It is
// independent of CacheVersion, which keys the cache itself.
const HashInputBlobVersion = 1

// maxHashInputBlobBytes bounds the size of a single persisted blob. dqlite
// writes serialize through Raft, so an unbounded blob (e.g. a task with
// thousands of predecessor outputs) would amplify write pressure. When the
// canonical encoding exceeds this, CanonicalJSON returns a compact "oversized"
// marker carrying only the digest + counts instead of the full decomposition —
// the field-level explanation degrades gracefully rather than bloating dqlite.
const maxHashInputBlobBytes = 64 * 1024

// redactedEnvValue is the placeholder stored in place of a non-secret-reference
// env value. The value is replaced by a digest so `caesium why` can still
// detect that an env var changed (the digest differs) without ever persisting
// the plaintext value, which may contain a credential that was injected as a
// literal rather than through a secret:// reference.
type redactedEnvValue struct {
	// Digest is "sha256:" + the hex SHA-256 of the raw value. A changed value
	// yields a changed digest, so two blobs remain diffable field-by-field.
	Digest string `json:"digest"`
	// Redacted is always true; it documents in the persisted JSON that the
	// literal value was intentionally withheld.
	Redacted bool `json:"redacted"`
}

// envBlobValue is one entry in the persisted env map. Exactly one of Secret or
// Redacted is set: a secret:// reference is stored verbatim (it is a non-secret
// pointer and is the informative thing to show), and every other value is
// redacted to a digest.
type envBlobValue struct {
	// Secret holds the literal secret:// URI when the value is a secret
	// reference. Secret references resolve through internal/jobdef/secret and
	// are excluded from the hash; they are safe to store and carry no
	// credential material.
	Secret string `json:"secret,omitempty"`
	// Redacted holds the digest of a non-reference value. nil when Secret is
	// set.
	Redacted *redactedEnvValue `json:"redacted,omitempty"`
}

// HashInputBlob is the canonical, secret-redacted, field-by-field
// representation of a HashInput that is persisted alongside the opaque digest.
// It exists so the system can answer *which* input changed between two runs
// (the basis of `caesium why`), not merely that "the hashes differ". Every
// field that contributes to Compute() is represented here; env values are
// redacted (see envBlobValue) but all other fields — including predecessor
// outputs, which are typed data-contract values, not secrets — are stored
// verbatim so a reader can show the before/after.
type HashInputBlob struct {
	// BlobVersion is HashInputBlobVersion at serialization time.
	BlobVersion int `json:"blobVersion"`
	// Hash is the Compute() digest this blob decomposes. Storing it inline lets
	// a reader confirm the blob matches the persisted TaskRun.Hash before
	// trusting the decomposition.
	Hash string `json:"hash"`

	JobAlias             string                       `json:"jobAlias,omitempty"`
	TaskName             string                       `json:"taskName,omitempty"`
	Image                string                       `json:"image,omitempty"`
	ResolvedImageDigest  string                       `json:"resolvedImageDigest,omitempty"`
	Command              []string                     `json:"command,omitempty"`
	Env                  map[string]envBlobValue      `json:"env,omitempty"`
	WorkDir              string                       `json:"workDir,omitempty"`
	Mounts               []container.Mount            `json:"mounts,omitempty"`
	ResolvedVolumeMounts []container.VolumeMount      `json:"resolvedVolumeMounts,omitempty"`
	Kubernetes           *container.KubernetesSpec    `json:"kubernetes,omitempty"`
	PredecessorHashes    []string                     `json:"predecessorHashes,omitempty"`
	PredecessorOutputs   map[string]map[string]string `json:"predecessorOutputs,omitempty"`
	RunParams            map[string]string            `json:"runParams,omitempty"`
	CacheVersion         int                          `json:"cacheVersion"`

	// Oversized is set (with Digest/EnvCount/PredecessorOutputCount populated
	// and the verbatim fields cleared) when the full decomposition exceeded
	// maxHashInputBlobBytes. A reader can still report "inputs changed" via the
	// digest but cannot diff field-by-field.
	Oversized *oversizedBlob `json:"oversized,omitempty"`
}

// oversizedBlob is the degraded representation stored when a full HashInputBlob
// would exceed maxHashInputBlobBytes.
type oversizedBlob struct {
	EnvCount               int `json:"envCount"`
	PredecessorCount       int `json:"predecessorCount"`
	PredecessorOutputCount int `json:"predecessorOutputCount"`
}

// secretRefPrefix is the scheme prefix identifying a secret:// reference. It
// must match internal/jobdef/secret's scheme; an env value with this prefix is
// a non-secret pointer (resolved at container-create time, after hashing) and
// is stored verbatim, while every other value is redacted.
const secretRefPrefix = "secret://"

// redactEnv produces the persisted env map: secret:// references verbatim, all
// other values replaced by a digest. The map is built deterministically by the
// caller's JSON encoder (encoding/json sorts map keys), so the result is
// canonical.
func redactEnv(env map[string]string) map[string]envBlobValue {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]envBlobValue, len(env))
	for k, v := range env {
		if strings.HasPrefix(v, secretRefPrefix) {
			out[k] = envBlobValue{Secret: v}
			continue
		}
		sum := sha256.Sum256([]byte(v))
		out[k] = envBlobValue{Redacted: &redactedEnvValue{
			Digest:   "sha256:" + hex.EncodeToString(sum[:]),
			Redacted: true,
		}}
	}
	return out
}

// CanonicalJSON serializes the decomposed HashInput to a canonical,
// secret-redacted JSON blob suitable for persistence and later field-by-field
// diffing (`caesium why`). It is deterministic: encoding/json emits object keys
// in sorted order and every slice is hashed/copied in the same order Compute()
// uses. The returned bytes are bounded by maxHashInputBlobBytes; if the full
// decomposition would exceed that, a compact oversized marker is returned
// instead so dqlite write pressure stays bounded.
//
// Env values are never stored verbatim: secret:// references are kept as-is
// (they are safe pointers) and all other values are reduced to a digest, so a
// credential injected as a literal env value never lands in the blob.
func (h HashInput) CanonicalJSON() ([]byte, error) {
	blob := HashInputBlob{
		BlobVersion:          HashInputBlobVersion,
		Hash:                 h.Compute(),
		JobAlias:             h.JobAlias,
		TaskName:             h.TaskName,
		Image:                h.Image,
		ResolvedImageDigest:  h.ResolvedImageDigest,
		Command:              h.Command,
		Env:                  redactEnv(h.Env),
		WorkDir:              h.WorkDir,
		Mounts:               h.Mounts,
		ResolvedVolumeMounts: h.ResolvedVolumeMounts,
		Kubernetes:           h.Kubernetes,
		PredecessorHashes:    sortedCopy(h.PredecessorHashes),
		PredecessorOutputs:   h.PredecessorOutputs,
		RunParams:            h.RunParams,
		CacheVersion:         h.CacheVersion,
	}

	data, err := json.Marshal(blob)
	if err != nil {
		return nil, fmt.Errorf("cache: marshal hash-input blob: %w", err)
	}
	if len(data) <= maxHashInputBlobBytes {
		return data, nil
	}

	// Degrade gracefully: keep identity + a digest, drop the verbatim fields.
	oversized := HashInputBlob{
		BlobVersion:         HashInputBlobVersion,
		Hash:                blob.Hash,
		JobAlias:            h.JobAlias,
		TaskName:            h.TaskName,
		Image:               h.Image,
		ResolvedImageDigest: h.ResolvedImageDigest,
		CacheVersion:        h.CacheVersion,
		Oversized: &oversizedBlob{
			EnvCount:               len(h.Env),
			PredecessorCount:       len(h.PredecessorHashes),
			PredecessorOutputCount: len(h.PredecessorOutputs),
		},
	}
	data, err = json.Marshal(oversized)
	if err != nil {
		return nil, fmt.Errorf("cache: marshal oversized hash-input blob: %w", err)
	}
	return data, nil
}

// sortedCopy returns a sorted copy of s without mutating the input, so the
// persisted blob lists predecessor hashes in the same deterministic order
// Compute() folds them in.
func sortedCopy(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

// HashInput contains all fields that contribute to a task's identity hash.
type HashInput struct {
	JobAlias string
	TaskName string
	Image    string
	// ResolvedImageDigest is the content digest (sha256:...) the Image tag
	// resolved to when digest pinning is enabled. It is empty when pinning is
	// off, in which case the hash is byte-identical to the pre-pinning era and
	// only the mutable tag contributes. When set, the digest is folded into the
	// key in addition to the tag, so a tag that moves to a new digest yields a
	// different hash — a cache miss, never a stale hit.
	ResolvedImageDigest  string
	Command              []string
	Env                  map[string]string
	WorkDir              string
	Mounts               []container.Mount
	ResolvedVolumeMounts []container.VolumeMount
	Kubernetes           *container.KubernetesSpec
	PredecessorHashes    []string
	PredecessorOutputs   map[string]map[string]string
	RunParams            map[string]string
	CacheVersion         int
}

// Compute returns the SHA-256 hex digest of the canonicalized input.
func (h HashInput) Compute() string {
	digest := sha256.New()
	// Write each field in deterministic order
	w(digest, "job_alias:%s\n", h.JobAlias)
	w(digest, "task_name:%s\n", h.TaskName)
	w(digest, "image:%s\n", h.Image)
	// When digest pinning is on, the resolved content digest is folded in
	// alongside the tag. Empty means pinning off; the line is omitted so the
	// hash stays byte-identical to the literal-tag behavior, preserving
	// existing cache entries. A non-empty digest that changes (a moving tag)
	// changes the key, forcing a cache miss instead of a stale hit.
	if h.ResolvedImageDigest != "" {
		w(digest, "image_digest:%s\n", h.ResolvedImageDigest)
	}
	w(digest, "command:%s\n", strings.Join(h.Command, "\x00"))

	// Sorted env vars
	envKeys := make([]string, 0, len(h.Env))
	for k := range h.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		w(digest, "env:%s=%s\n", k, h.Env[k])
	}

	w(digest, "workdir:%s\n", h.WorkDir)

	// Sorted mounts (serialize each mount deterministically)
	mountStrs := make([]string, 0, len(h.Mounts))
	for _, m := range h.Mounts {
		mountStrs = append(mountStrs, fmt.Sprintf("%s:%s:%s:%t", m.Source, m.Target, m.Type, m.ReadOnly))
	}
	sort.Strings(mountStrs)
	for _, m := range mountStrs {
		w(digest, "mount:%s\n", m)
	}

	volumeMountStrs := make([]string, 0, len(h.ResolvedVolumeMounts))
	for _, m := range h.ResolvedVolumeMounts {
		volumeMountStrs = append(volumeMountStrs, fmt.Sprintf("%s:%s:%s:%s:%t:%s:%s:%s:%s", m.Name, m.Type, m.Source, m.Target, m.ReadOnly, m.SubPath, canonicalJSON(m.Tmpfs), canonicalJSON(m.ClaimTemplate), canonicalJSON(m.VolumeSource)))
	}
	sort.Strings(volumeMountStrs)
	for _, m := range volumeMountStrs {
		w(digest, "volume_mount:%s\n", m)
	}

	if h.Kubernetes != nil {
		w(digest, "kubernetes.service_account:%s\n", h.Kubernetes.ServiceAccountName)
		if h.Kubernetes.AutomountServiceAccountToken != nil {
			w(digest, "kubernetes.automount:%t\n", *h.Kubernetes.AutomountServiceAccountToken)
		}
		keys := make([]string, 0, len(h.Kubernetes.PodAnnotations))
		for k := range h.Kubernetes.PodAnnotations {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w(digest, "kubernetes.annotation:%s=%s\n", k, h.Kubernetes.PodAnnotations[k])
		}
	}

	// Sorted predecessor hashes
	predHashes := make([]string, len(h.PredecessorHashes))
	copy(predHashes, h.PredecessorHashes)
	sort.Strings(predHashes)
	for _, ph := range predHashes {
		w(digest, "pred_hash:%s\n", ph)
	}

	// Sorted predecessor outputs
	predNames := make([]string, 0, len(h.PredecessorOutputs))
	for name := range h.PredecessorOutputs {
		predNames = append(predNames, name)
	}
	sort.Strings(predNames)
	for _, name := range predNames {
		outputs := h.PredecessorOutputs[name]
		outKeys := make([]string, 0, len(outputs))
		for k := range outputs {
			outKeys = append(outKeys, k)
		}
		sort.Strings(outKeys)
		for _, k := range outKeys {
			w(digest, "pred_output:%s:%s=%s\n", name, k, outputs[k])
		}
	}

	// Sorted run params
	paramKeys := make([]string, 0, len(h.RunParams))
	for k := range h.RunParams {
		paramKeys = append(paramKeys, k)
	}
	sort.Strings(paramKeys)
	for _, k := range paramKeys {
		w(digest, "param:%s=%s\n", k, h.RunParams[k])
	}

	w(digest, "cache_version:%d\n", h.CacheVersion)

	return hex.EncodeToString(digest.Sum(nil))
}

// w writes a formatted string to a hash.Hash. hash.Hash.Write never returns
// an error so the error is intentionally discarded.
func w(h hash.Hash, format string, args ...any) {
	_, _ = fmt.Fprintf(h, format, args...)
}

func canonicalJSON(value any) string {
	if value == nil {
		return ""
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(data)
}

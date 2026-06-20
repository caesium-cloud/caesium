package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// whydiff.go implements the read-side, field-by-field diff that powers
// `caesium why` (data-plane-memory A3). It compares two persisted, canonical
// HashInput blobs (see cache.HashInput.CanonicalJSON, written by A2 onto
// TaskRun.HashInputBlob / TaskCache.HashInputBlob) and names the field(s) that
// discriminate them — i.e. the inputs whose change forced a cache miss / re-run,
// or, for a cache hit, the proof that every hashed input was identical.
//
// The diff is honestly scoped to what the blob actually contains: the decomposed
// task-identity hash inputs (image, resolved digest, command, env, workdir,
// mounts, k8s spec, predecessor hashes, predecessor *outputs* — the typed
// data-contract values that flowed between steps — run params, cache version).
// It does NOT claim row/column-level data causality; it attributes which
// declared input or upstream output changed. Trigger-side causation (why the run
// fired at all) is layered on in the why service via the ExecutionEvent store,
// not here.

// blobFieldKind classifies the shape of a diffed field so a renderer can decide
// whether to show a before/after value (scalar / map) or merely "changed"
// (structural).
type blobFieldKind string

const (
	// fieldScalar is a single value (image, command, workdir, cacheVersion).
	fieldScalar blobFieldKind = "scalar"
	// fieldMapEntry is one key within a map-valued field (an env var, a run
	// param, a single predecessor output key). The Field carries the dotted path
	// (e.g. "predecessorOutputs.extract.row_count" or "env.FOO").
	fieldMapEntry blobFieldKind = "map_entry"
	// fieldStructural is an opaque structural field (mounts, volume mounts, the
	// kubernetes spec) compared by canonical-JSON equality. Before/After are
	// omitted because a full structural dump is rarely the useful signal; the
	// renderer reports only that it changed.
	fieldStructural blobFieldKind = "structural"
)

// FieldChange is one discriminating field between two HashInput blobs.
type FieldChange struct {
	// Field is the dotted path of the changed input, e.g. "image",
	// "env.DATABASE_URL", "predecessorOutputs.extract.row_count".
	Field string `json:"field"`
	// Kind classifies how to interpret Before/After.
	Kind blobFieldKind `json:"kind"`
	// Before is the value in the baseline blob ("" / null when the field was
	// absent there — i.e. the input was added). For redacted env values this is
	// the digest, never the plaintext.
	Before string `json:"before,omitempty"`
	// After is the value in the subject blob ("" / null when the field was
	// removed). For redacted env values this is the digest.
	After string `json:"after,omitempty"`
	// Added is true when the field exists only in the subject blob.
	Added bool `json:"added,omitempty"`
	// Removed is true when the field exists only in the baseline blob.
	Removed bool `json:"removed,omitempty"`
	// Redacted is true when the compared value is a redacted env digest, so a
	// renderer can label it "(redacted; digest differs)" rather than printing the
	// digest as if it were the literal value.
	Redacted bool `json:"redacted,omitempty"`
}

// hashInputBlob is a read-side mirror of the persisted blob produced by
// cache.HashInput.CanonicalJSON. It is decoded straight from the JSON (whose
// shape is pinned by HashInputBlobVersion) rather than reusing cache's struct,
// so the diff stays decoupled from cache's unexported env types and from any
// future internal change to the cache package's in-memory representation. The
// json tags MUST match cache.HashInputBlob exactly.
type hashInputBlob struct {
	BlobVersion int    `json:"blobVersion"`
	Hash        string `json:"hash"`

	JobAlias             string                       `json:"jobAlias,omitempty"`
	TaskName             string                       `json:"taskName,omitempty"`
	Image                string                       `json:"image,omitempty"`
	ResolvedImageDigest  string                       `json:"resolvedImageDigest,omitempty"`
	Command              []string                     `json:"command,omitempty"`
	Env                  map[string]envBlobValue      `json:"env,omitempty"`
	WorkDir              string                       `json:"workDir,omitempty"`
	Mounts               json.RawMessage              `json:"mounts,omitempty"`
	ResolvedVolumeMounts json.RawMessage              `json:"resolvedVolumeMounts,omitempty"`
	Kubernetes           json.RawMessage              `json:"kubernetes,omitempty"`
	PredecessorHashes    []string                     `json:"predecessorHashes,omitempty"`
	PredecessorOutputs   map[string]map[string]string `json:"predecessorOutputs,omitempty"`
	RunParams            map[string]string            `json:"runParams,omitempty"`
	CacheVersion         int                          `json:"cacheVersion"`

	Oversized *oversizedBlob `json:"oversized,omitempty"`
}

// envBlobValue mirrors cache's persisted env entry: either a verbatim secret://
// reference or a redacted digest of a literal value.
type envBlobValue struct {
	Secret   string            `json:"secret,omitempty"`
	Redacted *redactedEnvValue `json:"redacted,omitempty"`
}

// display returns a stable, printable representation of an env value and whether
// it is a redacted digest. secret:// references are shown verbatim (they carry
// no credential material); literal values are shown as their sha256: digest.
func (v envBlobValue) display() (value string, redacted bool) {
	if v.Secret != "" {
		return v.Secret, false
	}
	if v.Redacted != nil {
		return v.Redacted.Digest, true
	}
	return "", false
}

type redactedEnvValue struct {
	Digest   string `json:"digest"`
	Redacted bool   `json:"redacted"`
}

// oversizedBlob mirrors cache's degraded marker stored when a full blob would
// exceed the size bound. When either side is oversized the diff cannot report
// field-level changes and says so.
type oversizedBlob struct {
	EnvCount               int `json:"envCount"`
	PredecessorCount       int `json:"predecessorCount"`
	PredecessorOutputCount int `json:"predecessorOutputCount"`
}

// BlobDiff is the structured result of diffing two HashInput blobs.
type BlobDiff struct {
	// HashEqual is true when the two blobs decompose the same identity hash. By
	// construction equal hashes mean every hashed input was identical, so Changes
	// is empty; HashEqual=false with empty Changes can only happen when a blob is
	// oversized/unparseable (see Degraded).
	HashEqual bool `json:"hashEqual"`
	// SubjectHash / BaselineHash are the inline Compute() digests each blob
	// carries, surfaced so a caller can confirm the diff matched the runs it
	// expected.
	SubjectHash  string `json:"subjectHash,omitempty"`
	BaselineHash string `json:"baselineHash,omitempty"`
	// Changes lists every discriminating field, sorted by Field for determinism.
	Changes []FieldChange `json:"changes,omitempty"`
	// Degraded is set with a human-readable reason when a field-level diff was
	// not possible (an oversized blob, a version mismatch, or unparseable JSON).
	// The hash-level verdict (HashEqual) is still valid; only the field detail is
	// unavailable.
	Degraded string `json:"degraded,omitempty"`
}

// DiffHashInputBlobs decodes and diffs two canonical HashInput blobs: subject is
// the run being explained, baseline is what it is compared against (the prior
// run of the same task for a miss, or the cache-origin entry for a hit). Either
// blob may be nil/empty (e.g. caching was disabled, or there is no prior run);
// the result reports that gracefully via Degraded rather than erroring.
func DiffHashInputBlobs(subject, baseline []byte) (*BlobDiff, error) {
	d := &BlobDiff{}

	if len(subject) == 0 || len(baseline) == 0 {
		d.Degraded = "no decomposed input blob available on " + missingSide(subject, baseline) +
			" (caching disabled, the blob predates A2, or there is no comparison run)"
		// Still surface a hash verdict when at least the subject parsed.
		if len(subject) > 0 {
			if sb, err := decodeBlob(subject); err == nil {
				d.SubjectHash = sb.Hash
			}
		}
		if len(baseline) > 0 {
			if bb, err := decodeBlob(baseline); err == nil {
				d.BaselineHash = bb.Hash
			}
		}
		return d, nil
	}

	sb, err := decodeBlob(subject)
	if err != nil {
		return nil, fmt.Errorf("run: decode subject hash-input blob: %w", err)
	}
	bb, err := decodeBlob(baseline)
	if err != nil {
		return nil, fmt.Errorf("run: decode baseline hash-input blob: %w", err)
	}

	d.SubjectHash = sb.Hash
	d.BaselineHash = bb.Hash
	d.HashEqual = sb.Hash != "" && sb.Hash == bb.Hash

	if sb.BlobVersion != bb.BlobVersion {
		d.Degraded = fmt.Sprintf("blob version mismatch (subject v%d, baseline v%d): cannot diff field-by-field; comparing identity hash only", sb.BlobVersion, bb.BlobVersion)
		return d, nil
	}
	if sb.Oversized != nil || bb.Oversized != nil {
		d.Degraded = "one or both blobs were stored oversized (truncated to identity + digest); cannot diff field-by-field"
		return d, nil
	}

	d.Changes = diffBlobs(bb, sb) // (baseline, subject) so Before/After read naturally
	return d, nil
}

func missingSide(subject, baseline []byte) string {
	switch {
	case len(subject) == 0 && len(baseline) == 0:
		return "both runs"
	case len(subject) == 0:
		return "this run"
	default:
		return "the comparison run"
	}
}

func decodeBlob(raw []byte) (*hashInputBlob, error) {
	var b hashInputBlob
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// diffBlobs compares baseline (before) against subject (after) field-by-field,
// returning the changes sorted by field path for deterministic output.
func diffBlobs(before, after *hashInputBlob) []FieldChange {
	var changes []FieldChange

	addScalar := func(field, b, a string) {
		if b != a {
			changes = append(changes, FieldChange{Field: field, Kind: fieldScalar, Before: b, After: a})
		}
	}

	addScalar("image", before.Image, after.Image)
	addScalar("resolvedImageDigest", before.ResolvedImageDigest, after.ResolvedImageDigest)
	addScalar("command", joinCommand(before.Command), joinCommand(after.Command))
	addScalar("workDir", before.WorkDir, after.WorkDir)
	if before.CacheVersion != after.CacheVersion {
		changes = append(changes, FieldChange{
			Field:  "cacheVersion",
			Kind:   fieldScalar,
			Before: fmt.Sprintf("%d", before.CacheVersion),
			After:  fmt.Sprintf("%d", after.CacheVersion),
		})
	}

	changes = append(changes, diffEnv(before.Env, after.Env)...)
	changes = append(changes, diffPredecessorHashes(before.PredecessorHashes, after.PredecessorHashes)...)
	changes = append(changes, diffPredecessorOutputs(before.PredecessorOutputs, after.PredecessorOutputs)...)
	changes = append(changes, diffStringMap("runParams", before.RunParams, after.RunParams)...)

	addStructural := func(field string, b, a json.RawMessage) {
		if !rawJSONEqual(b, a) {
			changes = append(changes, FieldChange{Field: field, Kind: fieldStructural})
		}
	}
	addStructural("mounts", before.Mounts, after.Mounts)
	addStructural("resolvedVolumeMounts", before.ResolvedVolumeMounts, after.ResolvedVolumeMounts)
	addStructural("kubernetes", before.Kubernetes, after.Kubernetes)

	sort.Slice(changes, func(i, j int) bool { return changes[i].Field < changes[j].Field })
	return changes
}

// joinCommand renders a command slice as a single newline-free string for scalar
// comparison; nil and empty both render as "".
func joinCommand(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	b, _ := json.Marshal(cmd)
	return string(b)
}

func diffEnv(before, after map[string]envBlobValue) []FieldChange {
	var changes []FieldChange
	for _, key := range unionKeysEnv(before, after) {
		bv, bok := before[key]
		av, aok := after[key]
		bval, bred := bv.display()
		aval, ared := av.display()
		switch {
		case bok && !aok:
			changes = append(changes, FieldChange{Field: "env." + key, Kind: fieldMapEntry, Before: bval, Removed: true, Redacted: bred})
		case !bok && aok:
			changes = append(changes, FieldChange{Field: "env." + key, Kind: fieldMapEntry, After: aval, Added: true, Redacted: ared})
		case bval != aval:
			changes = append(changes, FieldChange{Field: "env." + key, Kind: fieldMapEntry, Before: bval, After: aval, Redacted: bred || ared})
		}
	}
	return changes
}

func diffStringMap(prefix string, before, after map[string]string) []FieldChange {
	var changes []FieldChange
	for _, key := range unionKeysStr(before, after) {
		bv, bok := before[key]
		av, aok := after[key]
		switch {
		case bok && !aok:
			changes = append(changes, FieldChange{Field: prefix + "." + key, Kind: fieldMapEntry, Before: bv, Removed: true})
		case !bok && aok:
			changes = append(changes, FieldChange{Field: prefix + "." + key, Kind: fieldMapEntry, After: av, Added: true})
		case bv != av:
			changes = append(changes, FieldChange{Field: prefix + "." + key, Kind: fieldMapEntry, Before: bv, After: av})
		}
	}
	return changes
}

// diffPredecessorOutputs is the headline of `caesium why`: it names the upstream
// step + output key whose typed value changed ("predecessorOutputs.extract.row_count
// 1.2M→1.4M"). These are data-contract values, not secrets, so they are shown
// verbatim.
func diffPredecessorOutputs(before, after map[string]map[string]string) []FieldChange {
	var changes []FieldChange
	for _, step := range unionKeysOutputs(before, after) {
		changes = append(changes, diffStringMap("predecessorOutputs."+step, before[step], after[step])...)
	}
	return changes
}

func diffPredecessorHashes(before, after []string) []FieldChange {
	if equalStringSets(before, after) {
		return nil
	}
	// Predecessor hashes are an unordered set in identity terms; report the net
	// add/remove rather than a positional diff.
	var changes []FieldChange
	beforeSet := toSet(before)
	afterSet := toSet(after)
	for _, h := range sortedSet(beforeSet) {
		if _, ok := afterSet[h]; !ok {
			changes = append(changes, FieldChange{Field: "predecessorHashes", Kind: fieldMapEntry, Before: h, Removed: true})
		}
	}
	for _, h := range sortedSet(afterSet) {
		if _, ok := beforeSet[h]; !ok {
			changes = append(changes, FieldChange{Field: "predecessorHashes", Kind: fieldMapEntry, After: h, Added: true})
		}
	}
	return changes
}

func rawJSONEqual(a, b json.RawMessage) bool {
	an := canonicalizeRaw(a)
	bn := canonicalizeRaw(b)
	return bytes.Equal(an, bn)
}

// canonicalizeRaw normalizes a JSON fragment (decode then re-encode) so two
// semantically-equal objects with different key ordering or whitespace compare
// equal. An empty/nil/"null" fragment normalizes to nil.
func canonicalizeRaw(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func unionKeysEnv(a, b map[string]envBlobValue) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	return sortedSet(seen)
}

func unionKeysStr(a, b map[string]string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	return sortedSet(seen)
}

func unionKeysOutputs(a, b map[string]map[string]string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	return sortedSet(seen)
}

func toSet(s []string) map[string]struct{} {
	out := make(map[string]struct{}, len(s))
	for _, v := range s {
		out[v] = struct{}{}
	}
	return out
}

func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := toSet(a)
	for _, v := range b {
		if _, ok := as[v]; !ok {
			return false
		}
	}
	return true
}

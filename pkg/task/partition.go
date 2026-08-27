package task

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// PartitionError is a parse/validation failure of the partition marker
// protocol. It must fail the producing task; the list is never truncated.
type PartitionError struct {
	Msg string
}

func (e *PartitionError) Error() string { return e.Msg }

func partitionErrorf(format string, args ...any) error {
	return &PartitionError{Msg: fmt.Sprintf(format, args...)}
}

func asPartitionError(err error) error {
	if err == nil {
		return nil
	}
	var pe *PartitionError
	if errors.As(err, &pe) {
		return err
	}
	return &PartitionError{Msg: err.Error()}
}

const (
	// partitionsMarker is the stdout line prefix for a JSON array of partition
	// elements (strings or objects). Multiple lines are appended.
	partitionsMarker = "##caesium::partitions "

	// partitionMarker is the stdout line prefix for one string-form partition
	// value. Object form is not accepted on this marker.
	partitionMarker = "##caesium::partition "

	// MaxPartitionListBytes caps the normalized encoding of the full partition
	// list. Independent of MaxOutputBytes — a large work list must not eat the
	// scalar-output / fan-in budget.
	MaxPartitionListBytes = 256 * 1024

	// MaxPartitionObjectBytes caps one normalized partition object.
	MaxPartitionObjectBytes = 2048

	// MaxPartitionAttributes is the maximum number of free-form scalar
	// attributes a structured partition may carry.
	MaxPartitionAttributes = 16

	// MaxPartitionKeyBytes is the maximum UTF-8 size of a partition key.
	MaxPartitionKeyBytes = 256

	// DefaultMaxPartitions is the default count cap when the executor does not
	// pass one (matches CAESIUM_FANOUT_MAX_PARTITIONS's default).
	DefaultMaxPartitions = 1024

	// PartitionEnv is the default injected env var for the partition key.
	PartitionEnv = "CAESIUM_PARTITION"

	// PartitionJSONEnv is the fixed injected env var for the normalized
	// partition object. It is not renameable via fanOut.env.
	PartitionJSONEnv = "CAESIUM_PARTITION_JSON"
)

// reservedPartitionObjectKeys are the structured-partition fields; every other
// object key is a free-form scalar attribute.
var reservedPartitionObjectKeys = map[string]struct{}{
	"key":         {},
	"fingerprint": {},
	"dependsOn":   {},
}

// Partition is the normalized partition element. A string-form emission is
// {Key: <value>} with empty Fingerprint/DependsOn/Attributes.
type Partition struct {
	Key         string            `json:"key"`
	Fingerprint string            `json:"fingerprint,omitempty"`
	DependsOn   []string          `json:"dependsOn,omitempty"`
	Attributes  map[string]string `json:"attributes,omitempty"`
}

// CanonicalObject lifts the partition to the lossless object form used for
// hashing, injection, and persistence. encoding/json sorts map keys, so the
// marshaled form is canonical.
func (p Partition) CanonicalObject() map[string]any {
	obj := make(map[string]any, 3+len(p.Attributes))
	obj["key"] = p.Key
	if p.Fingerprint != "" {
		obj["fingerprint"] = p.Fingerprint
	}
	if len(p.DependsOn) > 0 {
		obj["dependsOn"] = append([]string(nil), p.DependsOn...)
	}
	for k, v := range p.Attributes {
		obj[k] = v
	}
	return obj
}

// CanonicalJSON re-encodes the partition losslessly (sorted keys).
func (p Partition) CanonicalJSON() ([]byte, error) {
	return json.Marshal(p.CanonicalObject())
}

// EqualPayload reports whether two partitions are byte-identical after
// canonical encoding (used to distinguish first-seen dedup from conflict).
func (p Partition) EqualPayload(other Partition) bool {
	a, err := p.CanonicalJSON()
	if err != nil {
		return false
	}
	b, err := other.CanonicalJSON()
	if err != nil {
		return false
	}
	return bytes.Equal(a, b)
}

// EncodePartitions returns the lossless normalized JSON array of partitions.
func EncodePartitions(parts []Partition) ([]byte, error) {
	return encodeNormalizedPartitionList(parts)
}

func encodeNormalizedPartitionList(parts []Partition) ([]byte, error) {
	arr := make([]any, len(parts))
	for i, p := range parts {
		arr[i] = p.CanonicalObject()
	}
	return json.Marshal(arr)
}

type partitionAccumulator struct {
	parts         []Partition
	seen          map[string]Partition
	maxPartitions int
}

func newPartitionAccumulator(maxPartitions int) *partitionAccumulator {
	if maxPartitions <= 0 {
		maxPartitions = DefaultMaxPartitions
	}
	return &partitionAccumulator{
		seen:          make(map[string]Partition),
		maxPartitions: maxPartitions,
	}
}

func (a *partitionAccumulator) add(p Partition) error {
	if existing, ok := a.seen[p.Key]; ok {
		if existing.EqualPayload(p) {
			return nil
		}
		return partitionErrorf("partition key %q emitted with conflicting payloads", p.Key)
	}
	if len(a.parts)+1 > a.maxPartitions {
		return partitionErrorf("partition list exceeds count cap %d (offending key %q)", a.maxPartitions, p.Key)
	}
	encoded, err := p.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("partition %q: canonical encode: %w", p.Key, err)
	}
	if len(encoded) > MaxPartitionObjectBytes {
		return partitionErrorf("partition %q exceeds %d byte object cap (%d bytes)", p.Key, MaxPartitionObjectBytes, len(encoded))
	}
	a.seen[p.Key] = p
	a.parts = append(a.parts, p)
	return nil
}

func (a *partitionAccumulator) finish() ([]Partition, error) {
	if len(a.parts) == 0 {
		return nil, nil
	}
	encoded, err := encodeNormalizedPartitionList(a.parts)
	if err != nil {
		return nil, fmt.Errorf("marshalling partition list: %w", err)
	}
	if len(encoded) > MaxPartitionListBytes {
		return nil, partitionErrorf("partition list exceeds %d byte limit (%d bytes)", MaxPartitionListBytes, len(encoded))
	}
	return a.parts, nil
}

func parsePartitionsArrayLine(payload string, acc *partitionAccumulator) error {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	// Require an actual array token before decoding. JSON `null` unmarshals into
	// a nil []json.RawMessage WITHOUT an error, so `##caesium::partitions null`
	// used to be indistinguishable from "emitted nothing" and was routed through
	// onEmpty (skip, by default) instead of failing the producer. `[]` remains
	// the documented way to declare an empty work list.
	if payload[0] != '[' {
		return partitionErrorf("##caesium::partitions payload must be a JSON array, got %s", truncateForErr(payload))
	}
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return fmt.Errorf("malformed ##caesium::partitions JSON array: %w", err)
	}
	for _, elem := range raw {
		p, err := parsePartitionElement(elem)
		if err != nil {
			return err
		}
		if err := acc.add(p); err != nil {
			return err
		}
	}
	return nil
}

func parsePartitionLine(payload string, acc *partitionAccumulator) error {
	value := strings.TrimSpace(payload)
	if value == "" {
		return nil
	}
	p, err := partitionFromKey(value)
	if err != nil {
		return err
	}
	return acc.add(p)
}

func parsePartitionElement(raw json.RawMessage) (Partition, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return Partition{}, fmt.Errorf("partition element is empty")
	}
	switch trimmed[0] {
	case '"':
		var key string
		if err := json.Unmarshal(trimmed, &key); err != nil {
			return Partition{}, fmt.Errorf("partition string element: %w", err)
		}
		return partitionFromKey(key)
	case '{':
		return parsePartitionObject(trimmed)
	default:
		return Partition{}, fmt.Errorf("partition element is neither a string nor an object")
	}
}

func parsePartitionObject(raw []byte) (Partition, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Partition{}, fmt.Errorf("partition object: %w", err)
	}
	keyRaw, ok := fields["key"]
	if !ok {
		return Partition{}, fmt.Errorf("partition object missing key")
	}
	var key string
	if err := json.Unmarshal(keyRaw, &key); err != nil {
		return Partition{}, fmt.Errorf("partition object key: %w", err)
	}
	p, err := partitionFromKey(key)
	if err != nil {
		return p, err
	}

	if fpRaw, ok := fields["fingerprint"]; ok && !isJSONNull(fpRaw) {
		var fp string
		if err := json.Unmarshal(fpRaw, &fp); err != nil {
			return Partition{}, fmt.Errorf("partition %q fingerprint: %w", p.Key, err)
		}
		if fp != "" && !validSHA256Ref(fp) {
			return Partition{}, fmt.Errorf("partition %q fingerprint is not a valid sha256 digest", p.Key)
		}
		p.Fingerprint = fp
	}

	if depRaw, ok := fields["dependsOn"]; ok && !isJSONNull(depRaw) {
		var deps []string
		if err := json.Unmarshal(depRaw, &deps); err != nil {
			return Partition{}, fmt.Errorf("partition %q dependsOn: %w", p.Key, err)
		}
		// Normalize HERE, on the value that gets persisted. internal/run/fanout.go
		// marshals p.DependsOn verbatim into task_runs.partition_depends_on, so
		// normalizing only inside ValidatePartitionGraph's loop meant " a "
		// resolved as an edge to "a" for indegree purposes and was then stored
		// with the whitespace intact — a stored edge matching no sibling's
		// partition_value. Keys are already trimmed by partitionFromKey; the same
		// rules have to apply to the references to them.
		normalized, err := NormalizeDependsOn(p.Key, deps)
		if err != nil {
			return Partition{}, err
		}
		p.DependsOn = normalized
	}

	attrs := make(map[string]string)
	for k, v := range fields {
		if _, reserved := reservedPartitionObjectKeys[k]; reserved {
			continue
		}
		var decoded any
		if err := json.Unmarshal(v, &decoded); err != nil {
			return Partition{}, fmt.Errorf("partition %q attribute %q: %w", p.Key, k, err)
		}
		s, ok := scalarOutputValue(decoded)
		if !ok {
			return Partition{}, fmt.Errorf("partition %q attribute %q is not a scalar", p.Key, k)
		}
		attrs[k] = s
	}
	if len(attrs) > MaxPartitionAttributes {
		return Partition{}, fmt.Errorf("partition %q has %d attributes (max %d)", p.Key, len(attrs), MaxPartitionAttributes)
	}
	if len(attrs) > 0 {
		p.Attributes = attrs
	}
	return p, nil
}

func partitionFromKey(key string) (Partition, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Partition{}, fmt.Errorf("partition key is empty")
	}
	if !utf8.ValidString(key) {
		return Partition{}, fmt.Errorf("partition key is not valid UTF-8")
	}
	if len(key) > MaxPartitionKeyBytes {
		return Partition{}, fmt.Errorf("partition key %q exceeds %d bytes", truncateForErr(key), MaxPartitionKeyBytes)
	}
	return Partition{Key: key}, nil
}

// NormalizeDependsOn canonicalizes one partition's dependsOn list: each entry is
// trimmed (matching partitionFromKey's treatment of the keys those entries
// reference), an entry that is empty after trimming is rejected as a producer
// bug rather than silently dropped, and duplicates are collapsed so the stored
// edge list and the indegree the scheduler seeds describe the same graph.
//
// It is applied at parse time (so the persisted value is canonical) and again by
// ValidatePartitionGraph (so partitions constructed by any other path get the
// identical treatment). It is idempotent.
func NormalizeDependsOn(key string, deps []string) ([]string, error) {
	if len(deps) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(deps))
	seen := make(map[string]struct{}, len(deps))
	for _, dep := range deps {
		dep = strings.TrimSpace(dep)
		if dep == "" {
			return nil, fmt.Errorf("partition %q dependsOn contains an empty key", key)
		}
		if _, dup := seen[dep]; dup {
			continue
		}
		seen[dep] = struct{}{}
		out = append(out, dep)
	}
	return out, nil
}

// NormalizePartitions returns parts with every dependsOn list canonicalized. It
// is the single entry point callers outside the marker parser (recovery paths,
// tests) use so the graph and the persisted rows never disagree about keys.
func NormalizePartitions(parts []Partition) ([]Partition, error) {
	if len(parts) == 0 {
		return parts, nil
	}
	out := make([]Partition, len(parts))
	copy(out, parts)
	for i := range out {
		normalized, err := NormalizeDependsOn(out[i].Key, out[i].DependsOn)
		if err != nil {
			return nil, err
		}
		out[i].DependsOn = normalized
	}
	return out, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func truncateForErr(s string) string {
	if len(s) <= 64 {
		return s
	}
	return s[:61] + "..."
}

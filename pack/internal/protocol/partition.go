package protocol

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"
)

// Caps mirroring pkg/task's partition limits. The server rejects a marker that
// exceeds any of them by failing the producing task, so the emitter rejects it
// first with a message naming the offending unit.
const (
	MaxPartitionListBytes   = 256 * 1024
	MaxPartitionObjectBytes = 2048
	MaxPartitionAttributes  = 16
	MaxPartitionKeyBytes    = 256
)

// reservedPartitionKeys are the structured fields of a partition object. Every
// other key in the emitted object is a free-form scalar attribute (the design's
// `root` is one of those).
var reservedPartitionKeys = map[string]struct{}{
	"key":         {},
	"fingerprint": {},
	"dependsOn":   {},
}

// Partition is one discovered unit: a stack, a dbt model, a package. The object
// form is what carries the two things a bare string cannot — a per-unit
// fingerprint that enters that instance's cache identity, and the intra-group
// ordering the apply phase needs (spec §5.4).
type Partition struct {
	// Key becomes CAESIUM_PARTITION and identifies the unit.
	Key string
	// Fingerprint is the digest of everything the unit's result depends on.
	// Discover owns it: whatever it says here is the unit's cache-key
	// contribution, so an empty fingerprint is a contract violation, never
	// "unchanged".
	Fingerprint string
	// DependsOn names sibling partition keys that must complete first.
	DependsOn []string
	// Attributes are free-form scalars carried alongside (e.g. root, the stack
	// directory). They must not collide with a reserved field name.
	Attributes map[string]string
}

// object lifts the partition to the wire form. encoding/json sorts map keys, so
// the rendered object is canonical for a given partition.
func (p Partition) object() map[string]any {
	obj := make(map[string]any, 3+len(p.Attributes))
	obj["key"] = p.Key
	if p.Fingerprint != "" {
		obj["fingerprint"] = p.Fingerprint
	}
	if len(p.DependsOn) > 0 {
		deps := append([]string(nil), p.DependsOn...)
		sort.Strings(deps)
		obj["dependsOn"] = deps
	}
	for k, v := range p.Attributes {
		obj[k] = v
	}
	return obj
}

func (p Partition) validate() error {
	if err := validScalar("partition key", p.Key); err != nil {
		return err
	}
	if len(p.Key) > MaxPartitionKeyBytes {
		return fmt.Errorf("partition key %q exceeds %d bytes", p.Key, MaxPartitionKeyBytes)
	}
	if !utf8.ValidString(p.Key) {
		return fmt.Errorf("partition key is not valid UTF-8")
	}
	if p.Fingerprint != "" && !ValidDigest(p.Fingerprint) {
		return fmt.Errorf("partition %q fingerprint %q is not a sha256:<64 hex> digest", p.Key, p.Fingerprint)
	}
	seen := make(map[string]struct{}, len(p.DependsOn))
	for _, dep := range p.DependsOn {
		if err := validScalar(fmt.Sprintf("partition %q dependsOn entry", p.Key), dep); err != nil {
			return err
		}
		if dep == p.Key {
			return fmt.Errorf("partition %q depends on itself", p.Key)
		}
		if _, dup := seen[dep]; dup {
			return fmt.Errorf("partition %q lists %q in dependsOn twice", p.Key, dep)
		}
		seen[dep] = struct{}{}
	}
	if len(p.Attributes) > MaxPartitionAttributes {
		return fmt.Errorf("partition %q has %d attributes (max %d)", p.Key, len(p.Attributes), MaxPartitionAttributes)
	}
	for _, name := range SortedKeys(p.Attributes) {
		if _, reserved := reservedPartitionKeys[name]; reserved {
			return fmt.Errorf("partition %q attribute %q collides with a reserved field", p.Key, name)
		}
		if err := validScalar(fmt.Sprintf("partition %q attribute name", p.Key), name); err != nil {
			return err
		}
		if err := validValue(fmt.Sprintf("partition %q attribute %q", p.Key, name), p.Attributes[name]); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(p.object())
	if err != nil {
		return fmt.Errorf("partition %q: marshal: %w", p.Key, err)
	}
	if len(encoded) > MaxPartitionObjectBytes {
		return fmt.Errorf("partition %q encodes to %d bytes (max %d)", p.Key, len(encoded), MaxPartitionObjectBytes)
	}
	return nil
}

// Partitions stages one ##caesium::partitions marker carrying the whole unit
// set in object form. The list is emitted in key order so two discover runs
// over the same tree produce a byte-identical marker.
//
// Every referenced dependsOn key must be present in the same list: a dangling
// edge would expand into a group whose instance waits on a sibling that never
// materializes.
func (e *Emitter) Partitions(parts []Partition) error {
	if len(parts) == 0 {
		return fmt.Errorf("partitions: no units discovered")
	}
	sorted := append([]Partition(nil), parts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })

	keys := make(map[string]struct{}, len(sorted))
	for _, p := range sorted {
		if err := p.validate(); err != nil {
			return err
		}
		if _, dup := keys[p.Key]; dup {
			return fmt.Errorf("partition key %q emitted twice", p.Key)
		}
		keys[p.Key] = struct{}{}
	}
	for _, p := range sorted {
		for _, dep := range p.DependsOn {
			if _, ok := keys[dep]; !ok {
				return fmt.Errorf("partition %q depends on unknown unit %q", p.Key, dep)
			}
		}
	}
	if err := noPartitionCycle(sorted); err != nil {
		return err
	}

	objects := make([]map[string]any, 0, len(sorted))
	for _, p := range sorted {
		objects = append(objects, p.object())
	}
	payload, err := json.Marshal(objects)
	if err != nil {
		return fmt.Errorf("partitions: marshal: %w", err)
	}
	if len(payload) > MaxPartitionListBytes {
		return fmt.Errorf("partitions: %d bytes exceeds the %d byte limit", len(payload), MaxPartitionListBytes)
	}
	e.stage(PartitionsMarker + " " + string(payload))
	return nil
}

// noPartitionCycle rejects a dependsOn graph with a cycle. Caesium cycle-checks
// at expansion time too, but failing in discover names the units rather than
// failing the whole run with a scheduler error.
func noPartitionCycle(parts []Partition) error {
	deps := make(map[string][]string, len(parts))
	for _, p := range parts {
		deps[p.Key] = p.DependsOn
	}
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(parts))
	var walk func(key string, path []string) error
	walk = func(key string, path []string) error {
		switch state[key] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("partition dependency cycle: %v -> %s", path, key)
		}
		state[key] = visiting
		for _, dep := range deps[key] {
			if err := walk(dep, append(path, key)); err != nil {
				return err
			}
		}
		state[key] = done
		return nil
	}
	for _, p := range parts {
		if err := walk(p.Key, nil); err != nil {
			return err
		}
	}
	return nil
}

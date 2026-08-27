package lint

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// writeEntry is one write mount (readOnly omitted or false) of a named
// volume: the step that owns it and the subPath it writes within the volume
// ("" means the volume root, i.e. the entire volume).
type writeEntry struct {
	step    string
	subPath string
}

// CheckVolumeWriters returns a warning for every named volume (per
// definition) that is mounted read-write — i.e. without readOnly: true — by
// more than one step whose write regions overlap. This is the "two
// read-write mounts on one volume" check from spec §8; it is a lint
// *warning*, not an error (spec §11 Open Question 2): a legitimate
// two-writer case — two steps that each own a disjoint, sibling subPath of a
// shared volume — is real and should not be blocked, only flagged when
// writers genuinely contend for the same region.
//
// Overlap is decided by containment, not exact match: a mount with no
// subPath exposes the ENTIRE volume, so it conflicts with every other write
// mount of that volume regardless of that mount's own subPath; a subPath
// conflicts with any subPath nested under it ("reports" conflicts with
// "reports/2026"). Two subPaths are clear of each other only when neither is
// a path-segment prefix of the other ("a" vs "b").
//
// Two mechanisms write to a named volume and both are covered:
//   - the job-level `volumes:` / step `volumeMounts:` abstraction, keyed on
//     the job-level volume alias (`VolumeMount.Volume`), which carries a
//     `subPath` and so gets the containment treatment above;
//   - the lower-level `mounts: [{type: volume, source: <name>}]` form
//     (`container.Spec.Mounts`, `MountTypeVolume`) for mounting a raw
//     Docker/Podman named volume directly. That form has no subPath, so any
//     two write mounts of the same source name always conflict.
//
// These two mechanisms are checked independently and are not cross-referenced
// against each other: a job-level volume whose resolved per-engine source
// happens to name the same physical Docker/Podman volume as an unrelated raw
// `mounts: type: volume` entry is not detected as a conflict (resolving
// physical identity across the two forms is future work). Nor is
// `Volume.AccessMode: ReadOnlyMany` consulted — a volume-level read-only
// access mode makes step-level `readOnly: true` redundant but its absence is
// still flagged here.
func CheckVolumeWriters(defs []schema.Definition) []string {
	warnings := make([]string, 0)

	for _, def := range defs {
		warnings = append(warnings, checkNamedVolumeWriters(def)...)
		warnings = append(warnings, checkRawMountVolumeWriters(def)...)
	}

	return warnings
}

// checkNamedVolumeWriters covers the job-level volumes:/volumeMounts:
// abstraction, grouping write mounts by volume name and clustering them by
// subPath overlap (containment, not exact match) within each volume.
func checkNamedVolumeWriters(def schema.Definition) []string {
	byVolume := make(map[string][]writeEntry)
	var volumeOrder []string

	for _, step := range def.Steps {
		for _, mount := range step.VolumeMounts {
			if mount.ReadOnly {
				continue
			}
			volumeName := strings.TrimSpace(mount.Volume)
			if volumeName == "" {
				continue
			}
			if _, ok := byVolume[volumeName]; !ok {
				volumeOrder = append(volumeOrder, volumeName)
			}
			byVolume[volumeName] = append(byVolume[volumeName], writeEntry{step: step.Name, subPath: mount.SubPath})
		}
	}

	warnings := make([]string, 0)
	for _, volumeName := range volumeOrder {
		for _, steps := range conflictingStepGroups(byVolume[volumeName]) {
			msg := fmt.Sprintf(
				"volume %q is mounted read-write by multiple steps with overlapping subPaths: %s; "+
					"add readOnly: true, or give each step a non-overlapping subPath, to steps that only need part of the volume",
				volumeName, strings.Join(steps, ", "))
			warnings = append(warnings, withAliasPrefix(def, msg))
		}
	}
	return warnings
}

// checkRawMountVolumeWriters covers the lower-level mounts: [{type: volume,
// source: <name>}] mechanism (container.Spec.Mounts), which bypasses the
// job-level volumes:/volumeMounts: abstraction entirely and has no subPath,
// so any two write mounts of the same source name always conflict.
func checkRawMountVolumeWriters(def schema.Definition) []string {
	bySource := make(map[string][]string)
	var sourceOrder []string

	for _, step := range def.Steps {
		for _, mount := range step.Mounts {
			if mount.Type != container.MountTypeVolume || mount.ReadOnly {
				continue
			}
			source := strings.TrimSpace(mount.Source)
			if source == "" {
				continue
			}
			if _, ok := bySource[source]; !ok {
				sourceOrder = append(sourceOrder, source)
			}
			bySource[source] = appendUniqueStep(bySource[source], step.Name)
		}
	}

	warnings := make([]string, 0)
	for _, source := range sourceOrder {
		steps := bySource[source]
		if len(steps) < 2 {
			continue
		}
		sorted := append([]string(nil), steps...)
		sort.Strings(sorted)
		msg := fmt.Sprintf(
			"volume %q (mounts: type: volume) is mounted read-write by multiple steps: %s; "+
				"add readOnly: true to steps that only read",
			source, strings.Join(sorted, ", "))
		warnings = append(warnings, withAliasPrefix(def, msg))
	}
	return warnings
}

func withAliasPrefix(def schema.Definition, msg string) string {
	if alias := strings.TrimSpace(def.Metadata.Alias); alias != "" {
		return fmt.Sprintf("%s: %s", alias, msg)
	}
	return msg
}

// conflictingStepGroups unions write entries into connected components by
// subPath overlap (containment, not exact match) and returns the sorted step
// names of every component that spans more than one distinct step, in
// first-seen order. Transitivity is intentional and correct here: if a root
// mount (no subPath) is one of the entries, it overlaps every other entry
// for that volume, so all of them land in one component even if their own
// subPaths are otherwise disjoint siblings of each other.
func conflictingStepGroups(entries []writeEntry) [][]string {
	index := make(map[string]int)
	var order []string
	for _, e := range entries {
		if _, ok := index[e.step]; !ok {
			index[e.step] = len(order)
			order = append(order, e.step)
		}
	}

	parent := make([]int, len(order))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[i].step == entries[j].step {
				continue
			}
			if subPathsOverlap(entries[i].subPath, entries[j].subPath) {
				union(index[entries[i].step], index[entries[j].step])
			}
		}
	}

	groups := make(map[int][]string)
	var rootOrder []int
	for _, step := range order {
		r := find(index[step])
		if _, ok := groups[r]; !ok {
			rootOrder = append(rootOrder, r)
		}
		groups[r] = appendUniqueStep(groups[r], step)
	}

	result := make([][]string, 0, len(rootOrder))
	for _, r := range rootOrder {
		steps := groups[r]
		if len(steps) < 2 {
			continue
		}
		sorted := append([]string(nil), steps...)
		sort.Strings(sorted)
		result = append(result, sorted)
	}
	return result
}

// subPathsOverlap reports whether two volumeMount subPaths address
// overlapping regions of the same volume. "" (the volume root) overlaps
// everything; a subPath overlaps any subPath nested under it. Comparison is
// by cleaned path segment, not raw string prefix, so "report" does not
// overlap "reports".
func subPathsOverlap(a, b string) bool {
	as := subPathSegments(a)
	bs := subPathSegments(b)
	return isSegmentPrefix(as, bs) || isSegmentPrefix(bs, as)
}

func subPathSegments(p string) []string {
	trimmed := strings.Trim(strings.TrimSpace(p), "/")
	if trimmed == "" {
		return nil
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || cleaned == "" {
		return nil
	}
	return strings.Split(cleaned, "/")
}

// isSegmentPrefix reports whether prefix's segments are a leading subsequence
// of full's segments. A nil (root) prefix is a prefix of everything,
// including another nil/root, by definition (len 0 <= any length).
func isSegmentPrefix(prefix, full []string) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i, seg := range prefix {
		if full[i] != seg {
			return false
		}
	}
	return true
}

func appendUniqueStep(steps []string, name string) []string {
	for _, s := range steps {
		if s == name {
			return steps
		}
	}
	return append(steps, name)
}

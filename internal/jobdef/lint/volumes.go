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
// volume: the step that owns it and the region it writes within the volume
// ("" means the volume root, i.e. the entire volume).
type writeEntry struct {
	step    string
	subPath string
}

// CheckVolumeWriters returns a warning for every named volume (per
// definition) that is mounted read-write — i.e. without readOnly: true — by
// two or more steps that can run CONCURRENTLY and whose write regions
// overlap. This is the "two read-write mounts on one volume" check from spec
// §8; it is a lint *warning*, not an error (spec §11 Open Question 2).
//
// The hazard is concurrent writers, so two conditions must both hold before
// a pair is flagged:
//
//  1. **No DAG ordering.** If one step is reachable from the other along the
//     definition's resolved edges (`dependsOn`/`next`, or the implicit
//     sequential chain a definition with no explicit edges gets), the two can
//     never run at the same time and their shared volume is a handoff, not a
//     race — the `prepare` → `checkout` and `plan` → `apply` pairs of the
//     infrastructure-deployment pattern are exactly that. Edges come from
//     pkg/jobdef's own DeriveStepSuccessors, so this check cannot disagree
//     with the scheduler about what the DAG is. Steps on parallel branches
//     ARE flagged.
//  2. **Overlapping regions.** Overlap is decided by containment, not exact
//     match: a mount with no subPath exposes the ENTIRE volume, so it
//     overlaps every other write mount of that volume; a subPath overlaps
//     any subPath nested under it ("reports" overlaps "reports/2026"). Two
//     subPaths are clear of each other only when neither is a path-segment
//     prefix of the other ("a" vs "b").
//
// **subPath is engine-dependent.** Kubernetes and Podman apply
// `VolumeMount.SubPath` (internal/atom/kubernetes/engine.go,
// internal/atom/podman/engine.go), so on those engines a subPath really does
// narrow what the container can reach. The **Docker engine does not**: its
// convertMounts builds a mount.Mount with no VolumeOptions.Subpath
// (internal/atom/docker/engine.go), so a docker mount always exposes the
// whole volume and two docker steps declaring different subPaths still
// contend for the same bytes. This check therefore treats every mount on the
// docker engine — and on an unset engine, which pkg/jobdef defaults to
// docker on decode — as a root mount.
//
// Two mechanisms write to a named volume and both are covered:
//   - the job-level `volumes:` / step `volumeMounts:` abstraction, keyed on
//     the job-level volume alias (`VolumeMount.Volume`), which carries a
//     `subPath` and so gets the containment treatment above;
//   - the lower-level `mounts: [{type: volume, source: <name>}]` form
//     (`container.Spec.Mounts`, `MountTypeVolume`) for mounting a raw
//     Docker/Podman named volume directly. That form has no subPath at all,
//     so any two write mounts of the same source name always overlap.
//
// Known gaps, all of them false NEGATIVES (the check never invents a
// conflict it cannot see):
//   - The two mechanisms are not cross-referenced against each other: a
//     job-level volume whose resolved per-engine source happens to name the
//     same physical Docker/Podman volume as an unrelated raw
//     `mounts: type: volume` entry is not detected.
//   - Two job-level `volumes:` entries under different names that resolve to
//     the same physical source are likewise treated as unrelated volumes.
//   - `Volume.AccessMode: ReadOnlyMany` is not consulted: it makes
//     step-level `readOnly: true` redundant, but its absence is still what
//     this check reads.
//   - A step is never checked against ITSELF, so a `fanOut:` step whose N
//     partitions all write one volume is not flagged even though those
//     instances really do run concurrently. Per-unit isolation in the
//     fan-out form has to come from the container (a per-partition path or
//     backend key), not from the mount — see
//     docs/infrastructure-deployment.md's fan-out section.
func CheckVolumeWriters(defs []schema.Definition) []string {
	warnings := make([]string, 0)

	for _, def := range defs {
		ordering := newDAGOrdering(def.Steps)
		warnings = append(warnings, checkNamedVolumeWriters(def, ordering)...)
		warnings = append(warnings, checkRawMountVolumeWriters(def, ordering)...)
	}

	return warnings
}

// checkNamedVolumeWriters covers the job-level volumes:/volumeMounts:
// abstraction, grouping write mounts by volume name and clustering them by
// engine-effective subPath overlap within each volume.
func checkNamedVolumeWriters(def schema.Definition, ordering dagOrdering) []string {
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
			byVolume[volumeName] = append(byVolume[volumeName], writeEntry{
				step:    step.Name,
				subPath: effectiveSubPath(step.Engine, mount.SubPath),
			})
		}
	}

	warnings := make([]string, 0)
	for _, volumeName := range volumeOrder {
		for _, steps := range conflictingStepGroups(byVolume[volumeName], ordering) {
			msg := fmt.Sprintf(
				"volume %q is mounted read-write by steps that are not ordered by the DAG and write overlapping regions: %s; "+
					"add readOnly: true to steps that only read, order the writers with dependsOn, or give each writer a "+
					"non-overlapping subPath (kubernetes/podman only — the docker engine ignores subPath, so every docker "+
					"mount covers the whole volume)",
				volumeName, strings.Join(steps, ", "))
			warnings = append(warnings, withAliasPrefix(def, msg))
		}
	}
	return warnings
}

// checkRawMountVolumeWriters covers the lower-level mounts: [{type: volume,
// source: <name>}] mechanism (container.Spec.Mounts), which bypasses the
// job-level volumes:/volumeMounts: abstraction entirely and has no subPath,
// so every write mount of the same source name covers the whole volume.
func checkRawMountVolumeWriters(def schema.Definition, ordering dagOrdering) []string {
	bySource := make(map[string][]writeEntry)
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
			bySource[source] = append(bySource[source], writeEntry{step: step.Name})
		}
	}

	warnings := make([]string, 0)
	for _, source := range sourceOrder {
		for _, steps := range conflictingStepGroups(bySource[source], ordering) {
			msg := fmt.Sprintf(
				"volume %q (mounts: type: volume) is mounted read-write by steps that are not ordered by the DAG: %s; "+
					"add readOnly: true to steps that only read, or order the writers with dependsOn",
				source, strings.Join(steps, ", "))
			warnings = append(warnings, withAliasPrefix(def, msg))
		}
	}
	return warnings
}

func withAliasPrefix(def schema.Definition, msg string) string {
	if alias := strings.TrimSpace(def.Metadata.Alias); alias != "" {
		return fmt.Sprintf("%s: %s", alias, msg)
	}
	return msg
}

// effectiveSubPath is the region of a volume a mount actually exposes on the
// step's engine. Kubernetes and Podman honour VolumeMount.SubPath; the docker
// engine drops it, so a docker mount always covers the volume root no matter
// what the manifest declares. An empty engine is docker — pkg/jobdef defaults
// it on decode, and a hand-built Definition that leaves it unset gets the
// same, safer reading.
func effectiveSubPath(engine, subPath string) string {
	switch strings.TrimSpace(engine) {
	case schema.EngineKubernetes, schema.EnginePodman:
		return subPath
	default:
		return ""
	}
}

// dagOrdering answers "can these two steps ever run at the same time?" from
// the definition's resolved successor edges.
type dagOrdering struct {
	reachable map[string]map[string]struct{}
}

// newDAGOrdering resolves the step graph once per definition and precomputes
// transitive reachability. Edge resolution is delegated to pkg/jobdef's own
// DeriveStepSuccessors so `next`, `dependsOn` and the implicit sequential
// chain (a definition where no step declares an explicit edge) are all read
// exactly the way the scheduler reads them.
//
// DeriveStepSuccessors validates whole steps because it is also the
// definition's own edge builder, but only the name and the edge fields decide
// adjacency — so an edge-only copy fills the unrelated required fields in.
// That keeps the ordering exemption working on a definition the caller has
// not validated yet (`caesium job lint` runs this check over parse output)
// without re-implementing any edge parsing here.
func newDAGOrdering(steps []schema.Step) dagOrdering {
	edgeOnly := make([]schema.Step, len(steps))
	for i, step := range steps {
		edgeOnly[i] = schema.Step{
			Name:      step.Name,
			Next:      step.Next,
			DependsOn: step.DependsOn,
			Image:     "lint",
			Engine:    schema.EngineDocker,
			Type:      schema.StepTypeTask,
		}
	}

	successors, err := schema.DeriveStepSuccessors(edgeOnly)
	if err != nil {
		// An unresolvable graph (duplicate names, a dangling reference) is an
		// error the definition's own Validate reports. Here it only means no
		// ordering can be proven, so every overlapping writer pair is flagged
		// — the conservative direction.
		return dagOrdering{}
	}

	reachable := make(map[string]map[string]struct{}, len(edgeOnly))
	for _, step := range edgeOnly {
		seen := make(map[string]struct{})
		queue := append([]string(nil), successors[step.Name]...)
		for len(queue) > 0 {
			next := queue[0]
			queue = queue[1:]
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, successors[next]...)
		}
		reachable[step.Name] = seen
	}
	return dagOrdering{reachable: reachable}
}

// ordered reports whether a DAG path runs between a and b in either
// direction, i.e. whether the two steps are guaranteed never to execute
// concurrently.
func (o dagOrdering) ordered(a, b string) bool {
	if a == b {
		return true
	}
	if _, ok := o.reachable[a][b]; ok {
		return true
	}
	_, ok := o.reachable[b][a]
	return ok
}

// conflictingStepGroups unions write entries into connected components by
// "these two can run concurrently AND their regions overlap", and returns the
// sorted step names of every component that spans more than one distinct
// step, in first-seen order. Transitivity is intentional: if an unordered
// root mount (no effective subPath) is one of the entries, it overlaps every
// other entry for that volume, so all of them land in one component even
// when their own subPaths are otherwise disjoint siblings.
func conflictingStepGroups(entries []writeEntry, ordering dagOrdering) [][]string {
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
			if ordering.ordered(entries[i].step, entries[j].step) {
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

// subPathsOverlap reports whether two effective subPaths address overlapping
// regions of the same volume. "" (the volume root) overlaps everything; a
// subPath overlaps any subPath nested under it. Comparison is by cleaned path
// segment, not raw string prefix, so "report" does not overlap "reports".
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

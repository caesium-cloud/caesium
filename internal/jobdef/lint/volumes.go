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
// definition) that can have two or more CONCURRENT writers touching
// overlapping regions. The writers may be distinct DAG steps or partition
// instances of one fanOut step. This is the "two read-write mounts on one
// volume" check from spec §8; it is a lint *warning*, not an error (spec §11
// Open Question 2).
//
// The hazard is concurrent writers, so two conditions must both hold before
// a pair is flagged:
//
//  1. **No DAG ordering.** If one step is reachable from the other along the
//     definition's resolved edges (`dependsOn`/`next`, or the implicit
//     sequential chain a definition with no explicit edges gets), the two
//     cannot run at the same time WITHIN ONE RUN, and their shared volume is a
//     handoff, not a race — the `prepare` → `checkout` and `plan` → `apply`
//     pairs of the infrastructure-deployment pattern are exactly that. Edges
//     come from pkg/jobdef's own DeriveStepSuccessors, so this check cannot
//     disagree with the scheduler about what the DAG is. Steps on parallel
//     branches ARE flagged.
//  2. **Overlapping regions.** Overlap is decided by containment, not exact
//     match: a mount with no subPath exposes the ENTIRE volume, so it
//     overlaps every other write mount of that volume; a subPath overlaps
//     any subPath nested under it ("reports" overlaps "reports/2026"). Two
//     subPaths are clear of each other only when neither is a path-segment
//     prefix of the other ("a" vs "b").
//
// **subPath follows the resolved source.** Kubernetes applies
// `VolumeMount.SubPath`; Docker applies it to both bind and named-volume
// sources since #370 (`Honour VolumeMount.SubPath on the Docker engine`,
// closes #361); and Podman applies it to named volumes. Podman's bind-mount
// conversion does not apply SubPath, so this check conservatively treats a
// Podman bind as exposing the whole source. Per-instance sources (Docker or
// Podman tmpfs, Kubernetes claimTemplate, emptyDir, and generic ephemeral
// volumes) cannot share bytes between steps or fan-out instances and are
// omitted entirely.
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
// **Fan-out self-conflict.** A `fanOut:` step materializes N task instances
// from ONE step definition, and instances may run concurrently up to the
// step's within-run fan-out bound. Every instance shares that step's
// `ResolvedVolumeMounts` configuration verbatim — subPath is fixed at
// step-definition time, and a fanned instance's per-partition customization
// is limited to the injected partition env vars
// (`CAESIUM_PARTITION`/`CAESIUM_PARTITION_JSON`, internal/job/job.go); mounts
// are never touched per-instance. For a potentially shared source there is
// therefore no way today to give two instances disjoint subPaths, so this
// check flags the step against ITSELF regardless of subPath. Sources whose
// runtime lifecycle proves they are private to one container/pod are skipped.
// A shared source is also skipped when the step's own concurrency bound
// (`maxPartitions`, capped by a positive `maxParallel`; see
// fanOutWriterBound) is <= 1, because a second instance from that run can
// never be in flight. See docs/infrastructure-deployment.md's fan-out section.
//
// Known limits:
//   - The two mechanisms are not cross-referenced against each other: a
//     job-level volume whose resolved per-engine source happens to name the
//     same physical Docker/Podman volume as an unrelated raw
//     `mounts: type: volume` entry is not detected.
//   - Two job-level `volumes:` entries under different names that resolve to
//     the same physical source are likewise treated as unrelated volumes.
//   - `Volume.AccessMode: ReadOnlyMany` is not consulted: it makes
//     step-level `readOnly: true` redundant, but its absence is still what
//     this check reads.
//   - An arbitrary Kubernetes `volumeSource` is treated as potentially shared
//     unless it is the single known per-pod kind `emptyDir` or `ephemeral`.
//     That fails closed for storage plugins, but source-enforced read-only
//     kinds can consequently produce a conservative warning when the mount
//     itself omits `readOnly: true`.
//   - **Ordering and fan-out bounds are evaluated within a SINGLE run.** These
//     volumes are persistent (named volumes, pre-provisioned PVCs), and a job with no
//     `metadata.concurrency` block admits unlimited overlapping runs
//     (internal/run/store.go admits everything when MaxRuns <= 0) — so run
//     2's `prepare` can genuinely race run 1's `checkout` on one volume, and
//     the ordering and serialized-fan-out exemptions above say nothing about
//     it. Constrain
//     overlapping runs with `metadata.concurrency`; the reference manifests
//     do, and that block is load-bearing for this check's silence rather
//     than mere hygiene.
//   - **The check runs per DEFINITION.** Two jobs whose `volumes:` entries
//     resolve to the SAME physical docker volume or PVC are never compared,
//     so a cross-job pair of read-write mounts is invisible here no matter
//     how the DAG inside either job is shaped. docs/examples/'s deploy and
//     drift jobs deliberately share stores and mitigate it in the manifests
//     themselves (a `metadata.concurrency` block on each, distinct
//     `ARTIFACT_DIR`s so the two `terraform init -reconfigure` data
//     directories cannot collide, and a provider mirror whose warms are
//     idempotent under an atomic rename).
func CheckVolumeWriters(defs []schema.Definition) []string {
	warnings := make([]string, 0)

	for _, def := range defs {
		ordering := newDAGOrdering(def.Steps)
		warnings = append(warnings, checkNamedVolumeWriters(def, ordering)...)
		warnings = append(warnings, checkRawMountVolumeWriters(def, ordering)...)
		warnings = append(warnings, checkFanOutSelfWriters(def)...)
	}

	return warnings
}

// checkNamedVolumeWriters covers the job-level volumes:/volumeMounts:
// abstraction, grouping shared write mounts by volume name and clustering
// them by their effective subPath overlap. effectiveNamedWriteMount resolves
// source-specific behavior, including per-instance scratch storage and the
// Podman bind-mount case where subPath is not applied.
func checkNamedVolumeWriters(def schema.Definition, ordering dagOrdering) []string {
	byVolume := make(map[string][]writeEntry)
	var volumeOrder []string

	for _, step := range def.Steps {
		resolvedMounts, resolved := resolvedNamedVolumeMounts(def, step)
		for i, mount := range step.VolumeMounts {
			entry, sharedWriter := effectiveNamedWriteMount(step, mount, resolvedMounts, resolved, i)
			if !sharedWriter {
				continue
			}
			volumeName := strings.TrimSpace(mount.Volume)
			if volumeName == "" {
				continue
			}
			if _, ok := byVolume[volumeName]; !ok {
				volumeOrder = append(volumeOrder, volumeName)
			}
			byVolume[volumeName] = append(byVolume[volumeName], entry)
		}
	}

	warnings := make([]string, 0)
	for _, volumeName := range volumeOrder {
		for _, steps := range conflictingStepGroups(byVolume[volumeName], ordering) {
			msg := fmt.Sprintf(
				"volume %q is mounted read-write by steps that are not all pairwise ordered by the DAG and write overlapping regions: %s; "+
					"add readOnly: true to steps that only read, order the writers with dependsOn, or give each writer a "+
					"non-overlapping subPath",
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
				"volume %q (mounts: type: volume) is mounted read-write by steps that are not all pairwise ordered by the DAG: %s; "+
					"add readOnly: true to steps that only read, or order the writers with dependsOn",
				source, strings.Join(steps, ", "))
			warnings = append(warnings, withAliasPrefix(def, msg))
		}
	}
	return warnings
}

// checkFanOutSelfWriters flags a `fanOut:` step's own partition instances as
// concurrent writers of a shared volume WHEN more than one of them can be
// in flight at once. checkNamedVolumeWriters and checkRawMountVolumeWriters
// compare DIFFERENT steps and explicitly skip a step against itself
// (conflictingStepGroups requires >= 2 distinct step names per group), so
// without this pass a fanned step's own partitions — which may run
// concurrently whenever more than one is allowed in flight — are invisible.
//
// A fanOut step whose own concurrency bound (fanOutWriterBound) is <= 1 can
// never have two of its own instances holding the mount at once, so it is
// NOT flagged — the check must not turn "maxParallel: 1" or
// "maxPartitions: 1" into false positives, since neither can ever put a
// second writer in flight.
//
// subPath cannot rescue a shared-source step that IS flagged (see the fan-out
// self-conflict doc above): no subPath value or containment check can clear
// it, because subPath is fixed per step definition regardless of engine.
func checkFanOutSelfWriters(def schema.Definition) []string {
	warnings := make([]string, 0)

	for _, step := range def.Steps {
		if step.FanOut == nil {
			continue
		}
		if n, ok := fanOutWriterBound(step.FanOut); ok && n <= 1 {
			continue
		}
		count := fanOutWriterCountText(step.FanOut)

		for _, volumeName := range writableVolumeNames(def, step) {
			msg := fmt.Sprintf(
				"volume %q is mounted read-write by fanned step %q: fanOut allows %s partition instances of this one step to run concurrently, "+
					"all sharing the identical mount (subPath is fixed per step definition, so it cannot isolate one partition's writes from "+
					"another's); add readOnly: true if the step only reads, set fanOut.maxParallel: 1 to serialize this step's partition "+
					"instances within a run (and constrain metadata.concurrency to one run if the persistent volume must also be exclusive "+
					"across runs), or give each partition instance its own storage location or backend key from inside the container "+
					"(e.g. a sanitized or hash-derived key from the configured fanOut env or CAESIUM_PARTITION_JSON)",
				volumeName, step.Name, count)
			warnings = append(warnings, withAliasPrefix(def, msg))
		}

		for _, source := range writableRawMountSources(step) {
			msg := fmt.Sprintf(
				"volume %q (mounts: type: volume) is mounted read-write by fanned step %q: fanOut allows %s partition instances "+
					"of this one step, all sharing the identical mount (this raw mount form has no subPath at all); add readOnly: true if the "+
					"step only reads, set fanOut.maxParallel: 1 to serialize this step's partition instances within a run (and constrain "+
					"metadata.concurrency to one run if the persistent volume must also be exclusive across runs), or give each "+
					"partition instance its own storage location or backend key from inside the container (e.g. a sanitized or hash-derived "+
					"key from the configured fanOut env or CAESIUM_PARTITION_JSON)",
				source, step.Name, count)
			warnings = append(warnings, withAliasPrefix(def, msg))
		}
	}

	return warnings
}

// fanOutWriterBound returns the maximum number of a fanOut step's own
// partition instances that can be in flight at once within one run, so
// checkFanOutSelfWriters can skip a step whose own fan-out can never exceed a
// single concurrent writer. fanOut.maxPartitions is required and > 0 on a valid definition
// (pkg/jobdef/definition.go's validateFanOut), so the group's instance count
// is always finite — never truly unbounded — and fanOut.maxParallel
// additionally caps concurrency below that when it is set (> 0) and tighter
// (internal/run/fanout.go's claim predicate, internal/worker/claimer.go's
// mirror of it, and internal/job/job.go's local worker-pool cap all enforce
// exactly this bound). The CLI and REST lint surfaces validate definitions
// before calling this check, but the helper stays conservative for direct
// callers: malformed maxPartitions <= 0 has no knowable bound, so ok reports
// false and the caller flags it rather than trusting an invalid value.
func fanOutWriterBound(fo *schema.FanOut) (n int, ok bool) {
	if fo.MaxPartitions <= 0 {
		return 0, false
	}
	n = fo.MaxPartitions
	if fo.MaxParallel > 0 && fo.MaxParallel < n {
		n = fo.MaxParallel
	}
	return n, true
}

// fanOutWriterCountText renders the maximum writer multiplicity a fanOut
// step's own partitions can introduce. It names the tighter configured bound
// as an upper bound because the producer may emit fewer partitions. An
// unknowable bound (maxPartitions <= 0 on an unvalidated definition) is
// rendered conservatively as unbounded, matching fanOutWriterBound's
// fail-flagged behavior for the same input.
func fanOutWriterCountText(fo *schema.FanOut) string {
	n, ok := fanOutWriterBound(fo)
	if !ok {
		return "N>1 (fanOut.maxPartitions is invalid; treated as unbounded)"
	}
	return fmt.Sprintf("N≤%d", n)
}

// writableVolumeNames returns the deduped, first-seen-order set of shared
// job-level volume names a step mounts read-write via volumeMounts:.
func writableVolumeNames(def schema.Definition, step schema.Step) []string {
	seen := make(map[string]struct{})
	var names []string
	resolvedMounts, resolved := resolvedNamedVolumeMounts(def, step)
	for i, mount := range step.VolumeMounts {
		if _, sharedWriter := effectiveNamedWriteMount(step, mount, resolvedMounts, resolved, i); !sharedWriter {
			continue
		}
		name := strings.TrimSpace(mount.Volume)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// resolvedNamedVolumeMounts asks the job-definition resolver for the same
// runtime-neutral sources the engines receive. Lint normally runs after
// definition validation, so resolution succeeds. Direct unit callers may
// supply intentionally incomplete definitions; those fail closed to a shared
// volume root in effectiveNamedWriteMount.
func resolvedNamedVolumeMounts(def schema.Definition, step schema.Step) ([]container.VolumeMount, bool) {
	if step.Engine == "" {
		step.Engine = schema.EngineDocker
	}
	spec, err := def.RuntimeSpecForStep(&step)
	if err != nil || len(spec.ResolvedVolumeMounts) != len(step.VolumeMounts) {
		return nil, false
	}
	return spec.ResolvedVolumeMounts, true
}

// effectiveNamedWriteMount returns the shared write region exposed by one
// named mount. Per-instance scratch sources return false because different
// tasks cannot touch the same bytes. Podman bind mounts return the volume root
// because that engine currently drops SubPath for binds. If source resolution
// is unavailable, treat the mount as the shared volume root; valid CLI and
// REST inputs always resolve before reaching this point.
func effectiveNamedWriteMount(
	step schema.Step,
	mount schema.VolumeMount,
	resolvedMounts []container.VolumeMount,
	resolved bool,
	index int,
) (writeEntry, bool) {
	if mount.ReadOnly {
		return writeEntry{}, false
	}

	effectiveSubPath := ""
	if resolved {
		resolvedMount := resolvedMounts[index]
		if isPerInstanceVolumeMount(resolvedMount) {
			return writeEntry{}, false
		}
		if step.Engine == schema.EnginePodman && resolvedMount.Type == container.VolumeMountTypeBind {
			effectiveSubPath = ""
		} else {
			effectiveSubPath = mount.SubPath
		}
	}

	return writeEntry{step: step.Name, subPath: effectiveSubPath}, true
}

// isPerInstanceVolumeMount identifies sources whose lifecycle is scoped to
// one container or pod. A raw Kubernetes VolumeSource is skipped only for a
// single, unambiguous per-pod source kind; unknown and potentially shared
// sources remain included so lint fails closed.
func isPerInstanceVolumeMount(mount container.VolumeMount) bool {
	switch mount.Type {
	case container.VolumeMountTypeTmpfs, container.VolumeMountTypeClaimTemplate:
		return true
	case container.VolumeMountTypeVolumeSource:
		if len(mount.VolumeSource) != 1 {
			return false
		}
		if _, emptyDir := mount.VolumeSource["emptyDir"]; emptyDir {
			return true
		}
		_, ephemeral := mount.VolumeSource["ephemeral"]
		return ephemeral
	default:
		return false
	}
}

// writableRawMountSources returns the deduped, first-seen-order set of raw
// `mounts: type: volume` source names a step mounts read-write.
func writableRawMountSources(step schema.Step) []string {
	seen := make(map[string]struct{})
	var names []string
	for _, mount := range step.Mounts {
		if mount.Type != container.MountTypeVolume || mount.ReadOnly {
			continue
		}
		source := strings.TrimSpace(mount.Source)
		if source == "" {
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		names = append(names, source)
	}
	return names
}

func withAliasPrefix(def schema.Definition, msg string) string {
	if alias := strings.TrimSpace(def.Metadata.Alias); alias != "" {
		return fmt.Sprintf("%s: %s", alias, msg)
	}
	return msg
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
// That keeps the ordering exemption working for direct callers that supply a
// definition they have not validated, without re-implementing edge parsing
// here. The CLI and REST lint surfaces validate before reaching this helper.
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

package lint

import (
	"testing"

	"github.com/caesium-cloud/caesium/pkg/container"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/suite"
)

type VolumesSuite struct {
	suite.Suite
}

func TestVolumesSuite(t *testing.T) {
	suite.Run(t, new(VolumesSuite))
}

// parallel hangs every step off one `seed` fan-out so the steps are genuine
// siblings on parallel branches.
//
// This is not decoration. A definition in which NO step declares an explicit
// edge is auto-linked into a sequential chain by pkg/jobdef's own
// computeStepAdjacency — which would order every pair and make the
// multi-writer check silent for a reason that has nothing to do with the
// property under test. Every "warns" case below therefore has to be wired
// explicitly parallel.
func parallel(steps ...schema.Step) []schema.Step {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, step.Name)
	}
	return append([]schema.Step{{Name: "seed", Next: names}}, steps...)
}

func k8sMount(volume, path, subPath string) schema.VolumeMount {
	return schema.VolumeMount{Volume: volume, Path: path, SubPath: subPath}
}

func dockerNamedVolume(name string) schema.Volume {
	source := schema.VolumeSource{Volume: "test-" + name}
	return schema.Volume{Name: name, Source: &source}
}

func k8sPVCVolume(name string) schema.Volume {
	source := schema.VolumeSource{PVC: "test-" + name + "-rwx"}
	return schema.Volume{Name: name, Source: &source}
}

func perInstanceVolumeSourceCases() []struct {
	name   string
	engine string
	source schema.VolumeSource
} {
	return []struct {
		name   string
		engine string
		source schema.VolumeSource
	}{
		{name: "docker-tmpfs", engine: schema.EngineDocker, source: schema.VolumeSource{Tmpfs: &schema.TmpfsSource{}}},
		{name: "podman-tmpfs", engine: schema.EnginePodman, source: schema.VolumeSource{Tmpfs: &schema.TmpfsSource{}}},
		{name: "kubernetes-claim-template", engine: schema.EngineKubernetes, source: schema.VolumeSource{ClaimTemplate: &schema.ClaimTemplate{Size: "1Gi"}}},
		{name: "kubernetes-empty-dir", engine: schema.EngineKubernetes, source: schema.VolumeSource{VolumeSource: map[string]any{"emptyDir": map[string]any{}}}},
		{name: "kubernetes-ephemeral", engine: schema.EngineKubernetes, source: schema.VolumeSource{VolumeSource: map[string]any{"ephemeral": map[string]any{"volumeClaimTemplate": map[string]any{}}}}},
	}
}

// ---------------------------------------------------------------------------
// DAG ordering: the hazard is CONCURRENT writers (spec §8)
// ---------------------------------------------------------------------------

func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnParallelWriters() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "two-writers"},
		Steps: parallel(
			schema.Step{Name: "writer-one", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			schema.Step{Name: "writer-two", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "two-writers")
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-one")
	s.Contains(warnings[0], "writer-two")
}

// TestCheckVolumeWritersSilentOnDAGOrderedWriters is the refinement this
// check exists for: two steps that both write the whole volume but are
// separated by a dependsOn edge can never run at the same time, so the volume
// is a handoff (prepare → checkout, plan → apply), not a race.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnDAGOrderedWriters() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "ordered-writers"},
		Steps: []schema.Step{
			{Name: "prepare", VolumeMounts: []schema.VolumeMount{{Volume: "src", Path: "/src"}}, Next: []string{"checkout"}},
			{Name: "checkout", DependsOn: []string{"prepare"}, VolumeMounts: []schema.VolumeMount{{Volume: "src", Path: "/src"}}},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnTransitivelyOrderedWriters proves ordering is
// transitive: plan → apply with an unrelated step in between is still an
// ordering, so the pair is silent.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnTransitivelyOrderedWriters() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "transitively-ordered"},
		Steps: []schema.Step{
			{Name: "plan", VolumeMounts: []schema.VolumeMount{{Volume: "state", Path: "/state"}}, Next: []string{"review"}},
			{Name: "review", DependsOn: []string{"plan"}, Next: []string{"apply"}},
			{Name: "apply", DependsOn: []string{"review"}, VolumeMounts: []schema.VolumeMount{{Volume: "state", Path: "/state"}}},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnImplicitSequentialWriters guards the fact
// that edge resolution is delegated to pkg/jobdef: a definition with no
// explicit edges at all is auto-linked into a sequential chain by the
// scheduler, so its steps are ordered and must not be flagged.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnImplicitSequentialWriters() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "implicit-chain"},
		Steps: []schema.Step{
			{Name: "writer-one", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			{Name: "writer-two", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersWarnsOnOneUnorderedWriterAmongOrderedOnes covers the
// mixed case: prepare → checkout are ordered, but a third step on a parallel
// branch writes the same volume and is ordered against neither.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnOneUnorderedWriterAmongOrderedOnes() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "mixed-ordering"},
		Steps: []schema.Step{
			{Name: "seed", Next: []string{"prepare", "sidecar"}},
			{Name: "prepare", DependsOn: []string{"seed"}, Next: []string{"checkout"},
				VolumeMounts: []schema.VolumeMount{{Volume: "src", Path: "/src"}}},
			{Name: "checkout", DependsOn: []string{"prepare"},
				VolumeMounts: []schema.VolumeMount{{Volume: "src", Path: "/src"}}},
			{Name: "sidecar", DependsOn: []string{"seed"},
				VolumeMounts: []schema.VolumeMount{{Volume: "src", Path: "/src"}}},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "sidecar")
	s.Contains(warnings[0], "prepare")
	s.Contains(warnings[0], "checkout")
}

func (s *VolumesSuite) TestCheckVolumeWritersSilentWithOneWriter() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "one-writer"},
		Steps: parallel(
			schema.Step{Name: "writer", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			schema.Step{Name: "reader", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", ReadOnly: true}}},
			schema.Step{Name: "another-reader", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", ReadOnly: true}}},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// ---------------------------------------------------------------------------
// source-aware subPath containment
// ---------------------------------------------------------------------------

// TestCheckVolumeWritersSilentOnPodmanNamedVolumeDisjointSiblingSubPaths
// covers Podman's source-sensitive behavior: NamedVolume.SubPath is applied,
// so sibling regions of one named volume do not overlap.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnPodmanNamedVolumeDisjointSiblingSubPaths() {
	source := schema.VolumeSource{Volume: "shared-podman"}
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "disjoint-sibling-subpaths"},
		Volumes:  []schema.Volume{{Name: "shared", Source: &source}},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EnginePodman, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EnginePodman, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnDockerDisjointSubPaths proves subPath is
// honoured on docker the same as kubernetes/podman (issue #366 fix round 1,
// following #370 "Honour VolumeMount.SubPath on the Docker engine"): two
// docker steps declaring genuinely disjoint sibling subPaths of one named
// volume do not contend.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnDockerDisjointSubPaths() {
	source := schema.VolumeSource{Volume: "shared-docker"}
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "docker-subpaths"},
		Volumes:  []schema.Volume{{Name: "shared", Source: &source}},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnDockerBindDisjointSubPaths guards the other
// side of the Podman-bind distinction. Docker joins SubPath onto a resolved
// bind source, so sibling host directories are genuinely disjoint.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnDockerBindDisjointSubPaths() {
	source := schema.VolumeSource{Bind: "/host/shared"}
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "docker-bind-subpaths"},
		Volumes:  []schema.Volume{{Name: "shared", Source: &source}},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersWarnsOnPodmanBindDisjointDeclaredSubPaths prevents a
// false negative at the engine boundary. Podman's resolved bind conversion
// ignores VolumeMount.SubPath, so both containers receive the entire host
// source even when the manifest declares sibling paths.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnPodmanBindDisjointDeclaredSubPaths() {
	source := schema.VolumeSource{Bind: "/host/shared"}
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "podman-bind-subpaths"},
		Volumes:  []schema.Volume{{Name: "shared", Source: &source}},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EnginePodman, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EnginePodman, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "writer-a")
	s.Contains(warnings[0], "writer-b")
}

// TestCheckVolumeWritersTreatsUnsetEngineAsDocker mirrors pkg/jobdef's decode
// default: a step with no `engine:` runs on docker, so its subPath is
// honoured the same way an explicit `engine: docker` step's is.
func (s *VolumesSuite) TestCheckVolumeWritersTreatsUnsetEngineAsDocker() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "unset-engine-subpaths"},
		Volumes:  []schema.Volume{dockerNamedVolume("shared")},
		Steps: parallel(
			schema.Step{Name: "writer-a", VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersFallsBackToRootWhenVolumeCannotResolve ensures a
// malformed direct caller cannot manufacture a false negative with declared
// sibling subPaths. CLI and REST definitions validate first; the internal
// helper still fails closed when the named source is absent.
func (s *VolumesSuite) TestCheckVolumeWritersFallsBackToRootWhenVolumeCannotResolve() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "unresolved-subpaths"},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("missing", "/data", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("missing", "/data", "b")}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "writer-a")
	s.Contains(warnings[0], "writer-b")
}

// TestCheckVolumeWritersWarnsOnRootVsSubPath covers containment: a mount
// with no subPath exposes the entire volume, so it conflicts with a parallel
// step's subPath mount even on an engine that honours subPath.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnRootVsSubPath() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "root-vs-subpath"},
		Volumes:  []schema.Volume{k8sPVCVolume("shared")},
		Steps: parallel(
			schema.Step{Name: "writer-root", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			schema.Step{Name: "writer-reports", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "reports")}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-root")
	s.Contains(warnings[0], "writer-reports")
}

// TestCheckVolumeWritersWarnsOnNestedSubPaths covers containment the other
// direction: "reports/2026" is nested under "reports".
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnNestedSubPaths() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "nested-subpaths"},
		Volumes:  []schema.Volume{k8sPVCVolume("shared")},
		Steps: parallel(
			schema.Step{Name: "writer-reports", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "reports")}},
			schema.Step{Name: "writer-2026", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "reports/2026")}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "writer-reports")
	s.Contains(warnings[0], "writer-2026")
}

// TestCheckVolumeWritersSilentOnSegmentPrefixLookalikes guards the segment
// boundary: "reports" is a raw STRING prefix of "reports2" but not a path
// prefix, and the two address unrelated directories.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnSegmentPrefixLookalikes() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "segment-lookalikes"},
		Volumes:  []schema.Volume{k8sPVCVolume("shared")},
		Steps: parallel(
			schema.Step{Name: "writer-reports", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "reports")}},
			schema.Step{Name: "writer-reports2", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "reports2")}},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersNormalizesSubPathSpelling guards the other side of
// the same boundary: "./reports/" and "reports" are the same directory.
func (s *VolumesSuite) TestCheckVolumeWritersNormalizesSubPathSpelling() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "subpath-spelling"},
		Volumes:  []schema.Volume{k8sPVCVolume("shared")},
		Steps: parallel(
			schema.Step{Name: "writer-clean", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "reports")}},
			schema.Step{Name: "writer-dirty", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "./reports/")}},
		),
	}

	s.Require().Len(CheckVolumeWriters([]schema.Definition{def}), 1)
}

func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnSharedSubPath() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "shared-subpath"},
		Volumes:  []schema.Volume{k8sPVCVolume("shared")},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "reports")}},
			schema.Step{Name: "writer-b", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/other", "reports")}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-a")
	s.Contains(warnings[0], "writer-b")
}

// TestCheckVolumeWritersIsolatesUnrelatedSiblingCluster proves the
// containment/clustering logic doesn't over-warn: a root writer bridges two
// otherwise-disjoint sibling subtrees into one conflicting group, but a
// completely unrelated single writer of a *different* volume stays silent.
func (s *VolumesSuite) TestCheckVolumeWritersIsolatesUnrelatedSiblingCluster() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "mixed-clusters"},
		Volumes: []schema.Volume{
			k8sPVCVolume("shared"),
			k8sPVCVolume("other"),
		},
		Steps: parallel(
			schema.Step{Name: "writer-root", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			schema.Step{Name: "writer-a", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/a", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/b", "b")}},
			schema.Step{Name: "writer-other-volume", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{{Volume: "other", Path: "/x"}}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1, "expected exactly one warning for the shared-volume cluster, none for the lone other-volume writer")
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-root")
	s.Contains(warnings[0], "writer-a")
	s.Contains(warnings[0], "writer-b")
	s.NotContains(warnings[0], "writer-other-volume")
}

// TestCheckVolumeWritersSeparatesUnbridgedClustersInOneVolume proves the
// clustering is per-region, not per-volume: two independent conflicting pairs
// under disjoint subtrees of the same volume produce two separate warnings,
// each naming only its own steps.
func (s *VolumesSuite) TestCheckVolumeWritersSeparatesUnbridgedClustersInOneVolume() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "two-clusters"},
		Volumes:  []schema.Volume{k8sPVCVolume("shared")},
		Steps: parallel(
			schema.Step{Name: "alpha-one", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/a", "a")}},
			schema.Step{Name: "alpha-two", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/a", "a")}},
			schema.Step{Name: "xray-one", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/x", "x")}},
			schema.Step{Name: "xray-two", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/x", "x")}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 2)
	s.Contains(warnings[0], "alpha-one")
	s.Contains(warnings[0], "alpha-two")
	s.NotContains(warnings[0], "xray-")
	s.Contains(warnings[1], "xray-one")
	s.Contains(warnings[1], "xray-two")
	s.NotContains(warnings[1], "alpha-")
}

func (s *VolumesSuite) TestCheckVolumeWritersDedupesRepeatedMountsFromSameStep() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "same-step-twice"},
		Steps: []schema.Step{
			{Name: "writer", VolumeMounts: []schema.VolumeMount{
				{Volume: "shared", Path: "/data"},
				{Volume: "shared", Path: "/data-again"},
			}},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

func (s *VolumesSuite) TestCheckVolumeWritersIgnoresUnnamedMounts() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "no-volumes"},
		Steps: parallel(
			schema.Step{Name: "step-one"},
			schema.Step{Name: "step-two"},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// ---------------------------------------------------------------------------
// The raw `mounts: [{type: volume}]` mechanism
// ---------------------------------------------------------------------------

// TestCheckVolumeWritersWarnsOnRawMountTypeVolume covers the low-level
// mounts: [{type: volume, source: <name>}] mechanism (container.Spec.Mounts,
// bypassing the job-level volumes:/volumeMounts: abstraction entirely). It
// has no subPath, so any two parallel write mounts of the same source name
// conflict.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnRawMountTypeVolume() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "raw-mount-writers"},
		Steps: parallel(
			schema.Step{
				Name: "writer-one",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/data"},
				}},
			},
			schema.Step{
				Name: "writer-two",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/other"},
				}},
			},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "raw-mount-writers")
	s.Contains(warnings[0], `"shared-vol"`)
	s.Contains(warnings[0], "writer-one")
	s.Contains(warnings[0], "writer-two")
}

// TestCheckVolumeWritersSilentOnOrderedRawMountWriters applies the same
// concurrency reasoning to the raw mechanism: a dependsOn edge makes the
// shared docker volume a handoff.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnOrderedRawMountWriters() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "raw-mount-ordered"},
		Steps: []schema.Step{
			{
				Name: "writer-one",
				Next: []string{"writer-two"},
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/data"},
				}},
			},
			{
				Name:      "writer-two",
				DependsOn: []string{"writer-one"},
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/other"},
				}},
			},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnRawMountReadOnly proves a readOnly: true raw
// mount doesn't count as a writer, mirroring the named-volume case.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnRawMountReadOnly() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "raw-mount-reader"},
		Steps: parallel(
			schema.Step{
				Name: "writer",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/data"},
				}},
			},
			schema.Step{
				Name: "reader",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/other", ReadOnly: true},
				}},
			},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersIgnoresRawBindAndTmpfsMounts proves only
// MountTypeVolume is scanned — bind and tmpfs mounts are not named volumes.
func (s *VolumesSuite) TestCheckVolumeWritersIgnoresRawBindAndTmpfsMounts() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "raw-non-volume-mounts"},
		Steps: parallel(
			schema.Step{
				Name: "step-one",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeBind, Source: "/host/data", Target: "/data"},
				}},
			},
			schema.Step{
				Name: "step-two",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeTmpfs, Source: "/host/data", Target: "/data"},
				}},
			},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnParallelPerInstanceVolumes proves the named
// volume check distinguishes a logical alias from shared physical storage.
// Each tmpfs, inline claim, or emptyDir belongs to one container/pod, so two
// parallel steps cannot touch the same bytes through it.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnParallelPerInstanceVolumes() {
	for _, tc := range perInstanceVolumeSourceCases() {
		s.Run(tc.name, func() {
			source := tc.source
			def := schema.Definition{
				Metadata: schema.Metadata{Alias: "parallel-private-" + tc.name},
				Volumes:  []schema.Volume{{Name: "scratch", Source: &source}},
				Steps: parallel(
					schema.Step{Name: "writer-a", Engine: tc.engine, VolumeMounts: []schema.VolumeMount{{Volume: "scratch", Path: "/scratch"}}},
					schema.Step{Name: "writer-b", Engine: tc.engine, VolumeMounts: []schema.VolumeMount{{Volume: "scratch", Path: "/scratch"}}},
				),
			}

			s.Empty(CheckVolumeWriters([]schema.Definition{def}))
		})
	}
}

// ---------------------------------------------------------------------------
// Fan-out self-conflict: N partition instances of ONE step
// ---------------------------------------------------------------------------

// TestCheckVolumeWritersWarnsOnFannedWriter is issue #366: a fanOut step's N
// partition instances may run concurrently from one step definition, so a
// shared writable volume is a real hazard even though only one *step* mounts
// it. Before this fix a step was never checked against itself.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnFannedWriter() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-writer"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 4},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "fanned-writer")
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "process")
	s.Contains(warnings[0], "N≤4")
}

// TestCheckVolumeWritersWarnsWithUnsetFanOutMaxParallel covers maxParallel
// left unset: fanOut.maxPartitions is required and > 0 on a valid definition,
// so the group is never actually unbounded — the finding names the bound
// "N≤<maxPartitions>" rather than claiming no cap exists at all.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsWithUnsetFanOutMaxParallel() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-writer-unbounded"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "N≤16")
}

// TestCheckVolumeWritersWarnsOnFannedWriterWithMaxParallelTwo proves a bound
// strictly above 1 is still flagged, and that fanOut.maxParallel — when it is
// the tighter of the two knobs — is the number named in the finding.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnFannedWriterWithMaxParallelTwo() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-writer-max-parallel-two"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 2},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "N≤2")
}

// TestCheckVolumeWritersSilentOnFannedWriterWithMaxParallelOne is the fix
// this review round exists for: fanOut.maxParallel: 1 hard-caps the group at
// one IN-FLIGHT instance on every dispatch lane (internal/run/fanout.go's
// claim predicate, internal/worker/claimer.go's mirror, internal/job/job.go's
// local worker-pool cap), so a second partition instance can never hold the
// mount concurrently with the first within that run — there is no within-run
// hazard to flag. Cross-run exclusion remains the separate responsibility of
// metadata.concurrency.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnFannedWriterWithMaxParallelOne() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-writer-max-parallel-one"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 1},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-raw", Target: "/raw"},
				}},
			},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnFannedWriterWithMaxPartitionsOne covers the
// other knob: fanOut.maxPartitions: 1 means the group can only ever expand to
// a single instance in total, so no second writer can exist regardless of
// maxParallel.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnFannedWriterWithMaxPartitionsOne() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-writer-max-partitions-one"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 1, MaxParallel: 4},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
			},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnFannedPerInstanceVolumes is the fan-out
// counterpart to the pairwise scratch test: instances share a mount
// declaration, but these source kinds allocate different backing storage for
// every container/pod, so there is no multi-writer hazard at any concurrency.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnFannedPerInstanceVolumes() {
	for _, tc := range perInstanceVolumeSourceCases() {
		s.Run(tc.name, func() {
			source := tc.source
			def := schema.Definition{
				Metadata: schema.Metadata{Alias: "fanned-private-" + tc.name},
				Volumes:  []schema.Volume{{Name: "scratch", Source: &source}},
				Steps: []schema.Step{
					{Name: "discover", Next: []string{"process"}},
					{
						Name:         "process",
						Engine:       tc.engine,
						DependsOn:    []string{"discover"},
						FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 4},
						VolumeMounts: []schema.VolumeMount{{Volume: "scratch", Path: "/scratch"}},
					},
				},
			}

			s.Empty(CheckVolumeWriters([]schema.Definition{def}))
		})
	}
}

// TestCheckVolumeWritersWarnsOnFannedPotentiallySharedVolumeSources keeps the
// source classification fail-closed. hostPath is plainly shared; inline CSI
// is also intentionally not exempt because its driver contract may reuse the
// same backing volume for identical sources across pods.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnFannedPotentiallySharedVolumeSources() {
	for _, tc := range []struct {
		name   string
		source map[string]any
	}{
		{name: "host-path", source: map[string]any{"hostPath": map[string]any{"path": "/host/shared"}}},
		{name: "inline-csi", source: map[string]any{"csi": map[string]any{"driver": "storage.example.test"}}},
	} {
		s.Run(tc.name, func() {
			source := schema.VolumeSource{VolumeSource: tc.source}
			def := schema.Definition{
				Metadata: schema.Metadata{Alias: "fanned-" + tc.name},
				Volumes:  []schema.Volume{{Name: "shared", Source: &source}},
				Steps: []schema.Step{
					{Name: "discover", Next: []string{"process"}},
					{
						Name:         "process",
						Engine:       schema.EngineKubernetes,
						DependsOn:    []string{"discover"},
						FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 4},
						VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
					},
				},
			}

			warnings := CheckVolumeWriters([]schema.Definition{def})
			s.Require().Len(warnings, 1)
			s.Contains(warnings[0], "fanned-"+tc.name)
		})
	}
}

// TestCheckVolumeWritersFannedMixedPrivateAndSharedMounts emits only the
// genuinely shared alias when one step mounts both per-pod scratch space and
// a persistent PVC.
func (s *VolumesSuite) TestCheckVolumeWritersFannedMixedPrivateAndSharedMounts() {
	privateSource := schema.VolumeSource{ClaimTemplate: &schema.ClaimTemplate{Size: "1Gi"}}
	sharedSource := schema.VolumeSource{PVC: "shared-rwx"}
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-mixed-storage"},
		Volumes: []schema.Volume{
			{Name: "scratch", Source: &privateSource},
			{Name: "shared", Source: &sharedSource},
		},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:      "process",
				Engine:    schema.EngineKubernetes,
				DependsOn: []string{"discover"},
				FanOut:    &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 4},
				VolumeMounts: []schema.VolumeMount{
					{Volume: "scratch", Path: "/scratch"},
					{Volume: "shared", Path: "/data"},
				},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], `"shared"`)
	s.NotContains(warnings[0], `"scratch"`)
}

// TestCheckVolumeWritersWarnsOnFannedWriterWithInvalidMaxPartitions proves
// the helper fails CLOSED for a malformed definition supplied by a direct
// caller. The CLI and REST lint surfaces validate first, but this internal
// API does not require its caller to do so; maxPartitions <= 0 has no safe
// concurrency bound, so it remains flagged rather than being trusted.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnFannedWriterWithInvalidMaxPartitions() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-writer-invalid-max-partitions"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 0, MaxParallel: 1},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "N>1")
}

// TestCheckVolumeWritersWarnsOnFannedWriterDespiteSubPath proves subPath
// cannot be used to opt a fanOut step's writers out of this finding: even a
// subPath value that looks partition-scoped is a fixed string on the step
// definition, identical for every one of the fanned step's N partition
// instances (internal/job/job.go's per-instance customization is limited to
// the injected partition env vars, never mounts) — regardless of engine, and
// regardless of whether that engine honours subPath at all. There is no
// syntax that makes subPath vary per fanned instance today, so whenever the
// within-run fan-out bound exceeds one, the check flags it independently of
// subPath.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnFannedWriterDespiteSubPath() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-writer-subpath"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				Engine:       schema.EngineKubernetes,
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 2},
				VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "partitions/$CAESIUM_PARTITION")},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "process")
	s.Contains(warnings[0], "subPath is fixed per step definition")
}

// TestCheckVolumeWritersSilentOnFannedReadOnlyMount proves a readOnly: true
// mount on a fanOut step is not a writer, mirroring the non-fanned case.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnFannedReadOnlyMount() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-reader"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:         "process",
				DependsOn:    []string{"discover"},
				FanOut:       &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 4},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", ReadOnly: true}},
			},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersWarnsOnFannedRawMountWriter covers the raw `mounts:
// [{type: volume}]` mechanism, which has no subPath at all, so a fanned
// step's own partitions writing it are flagged the same way.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnFannedRawMountWriter() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "fanned-raw-mount-writer"},
		Steps: []schema.Step{
			{Name: "discover", Next: []string{"process"}},
			{
				Name:      "process",
				DependsOn: []string{"discover"},
				FanOut:    &schema.FanOut{From: "discover", MaxPartitions: 16, MaxParallel: 4},
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/data"},
				}},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], `"shared-vol"`)
	s.Contains(warnings[0], "process")
}

// TestCheckVolumeWritersFallsBackToWarningOnAnUnresolvableGraph proves the
// ordering exemption fails CLOSED: when the step graph cannot be resolved at
// all (here a dangling dependsOn, which the definition's own Validate
// reports as an error), no ordering can be proven and the overlapping
// writers are still flagged.
func (s *VolumesSuite) TestCheckVolumeWritersFallsBackToWarningOnAnUnresolvableGraph() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "dangling-edge"},
		Steps: []schema.Step{
			{Name: "writer-one", DependsOn: []string{"does-not-exist"},
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			{Name: "writer-two",
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
		},
	}

	s.Require().Len(CheckVolumeWriters([]schema.Definition{def}), 1)
}

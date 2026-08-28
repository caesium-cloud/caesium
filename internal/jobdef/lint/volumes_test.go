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
// subPath containment — kubernetes/podman only
// ---------------------------------------------------------------------------

// TestCheckVolumeWritersSilentOnDisjointSiblingSubPaths covers the
// genuinely-disjoint two-writer case Open Question 2 anticipates: on an
// engine that HONOURS subPath, two sibling subPaths that share no
// ancestor/descendant relationship never overlap on disk.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnDisjointSiblingSubPaths() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "disjoint-sibling-subpaths"},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EngineKubernetes, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EnginePodman, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersWarnsOnDockerSubPathWriters is the docker caveat: the
// docker engine's convertMounts never sets VolumeOptions.Subpath, so two
// docker steps declaring different subPaths of one named volume both see the
// whole volume and genuinely contend.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnDockerSubPathWriters() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "docker-subpaths"},
		Steps: parallel(
			schema.Step{Name: "writer-a", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", Engine: schema.EngineDocker, VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-a")
	s.Contains(warnings[0], "writer-b")
	s.Contains(warnings[0], "docker engine ignores subPath")
}

// TestCheckVolumeWritersTreatsUnsetEngineAsDocker mirrors pkg/jobdef's decode
// default: a step with no `engine:` runs on docker, so its subPath is just as
// unenforced.
func (s *VolumesSuite) TestCheckVolumeWritersTreatsUnsetEngineAsDocker() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "unset-engine-subpaths"},
		Steps: parallel(
			schema.Step{Name: "writer-a", VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "a")}},
			schema.Step{Name: "writer-b", VolumeMounts: []schema.VolumeMount{k8sMount("shared", "/data", "b")}},
		),
	}

	s.Require().Len(CheckVolumeWriters([]schema.Definition{def}), 1)
}

// TestCheckVolumeWritersWarnsOnRootVsSubPath covers containment: a mount
// with no subPath exposes the entire volume, so it conflicts with a parallel
// step's subPath mount even on an engine that honours subPath.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnRootVsSubPath() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "root-vs-subpath"},
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

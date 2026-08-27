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

func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnTwoWriters() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "two-writers"},
		Steps: []schema.Step{
			{
				Name:         "writer-one",
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
			},
			{
				Name:         "writer-two",
				VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "two-writers")
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-one")
	s.Contains(warnings[0], "writer-two")
}

func (s *VolumesSuite) TestCheckVolumeWritersSilentWithOneWriter() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "one-writer"},
		Steps: []schema.Step{
			{Name: "writer", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			{Name: "reader", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", ReadOnly: true}}},
			{Name: "another-reader", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", ReadOnly: true}}},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersSilentOnDisjointSiblingSubPaths covers the
// genuinely-disjoint two-writer case Open Question 2 anticipates: two
// sibling subPaths ("a" and "b") that share no ancestor/descendant
// relationship never overlap on disk, so no warning fires.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnDisjointSiblingSubPaths() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "disjoint-sibling-subpaths"},
		Steps: []schema.Step{
			{Name: "writer-a", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", SubPath: "a"}}},
			{Name: "writer-b", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", SubPath: "b"}}},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersWarnsOnRootVsSubPath covers containment: a mount
// with no subPath exposes the entire volume, so it conflicts with a sibling
// step's subPath mount even though the subPaths themselves are not equal.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnRootVsSubPath() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "root-vs-subpath"},
		Steps: []schema.Step{
			{Name: "writer-root", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			{Name: "writer-reports", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", SubPath: "reports"}}},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-root")
	s.Contains(warnings[0], "writer-reports")
}

// TestCheckVolumeWritersWarnsOnNestedSubPaths covers containment the other
// direction: "reports/2026" is nested under "reports", so the two mounts
// overlap even though neither subPath equals the other.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnNestedSubPaths() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "nested-subpaths"},
		Steps: []schema.Step{
			{Name: "writer-reports", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", SubPath: "reports"}}},
			{Name: "writer-2026", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", SubPath: "reports/2026"}}},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "writer-reports")
	s.Contains(warnings[0], "writer-2026")
}

func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnSharedSubPath() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "shared-subpath"},
		Steps: []schema.Step{
			{Name: "writer-a", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", SubPath: "reports"}}},
			{Name: "writer-b", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/other", SubPath: "reports"}}},
		},
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
		Steps: []schema.Step{
			{Name: "writer-root", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			{Name: "writer-a", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/a", SubPath: "a"}}},
			{Name: "writer-b", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/b", SubPath: "b"}}},
			{Name: "writer-other-volume", VolumeMounts: []schema.VolumeMount{{Volume: "other", Path: "/x"}}},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1, "expected exactly one warning for the shared-volume cluster, none for the lone other-volume writer")
	s.Contains(warnings[0], `"shared"`)
	s.Contains(warnings[0], "writer-root")
	s.Contains(warnings[0], "writer-a")
	s.Contains(warnings[0], "writer-b")
	s.NotContains(warnings[0], "writer-other-volume")
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
		Steps: []schema.Step{
			{Name: "step-one"},
			{Name: "step-two"},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersWarnsOnRawMountTypeVolume covers the low-level
// mounts: [{type: volume, source: <name>}] mechanism (container.Spec.Mounts,
// bypassing the job-level volumes:/volumeMounts: abstraction entirely). It
// has no subPath, so any two write mounts of the same source name conflict.
func (s *VolumesSuite) TestCheckVolumeWritersWarnsOnRawMountTypeVolume() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "raw-mount-writers"},
		Steps: []schema.Step{
			{
				Name: "writer-one",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/data"},
				}},
			},
			{
				Name: "writer-two",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/other"},
				}},
			},
		},
	}

	warnings := CheckVolumeWriters([]schema.Definition{def})
	s.Require().Len(warnings, 1)
	s.Contains(warnings[0], "raw-mount-writers")
	s.Contains(warnings[0], `"shared-vol"`)
	s.Contains(warnings[0], "writer-one")
	s.Contains(warnings[0], "writer-two")
}

// TestCheckVolumeWritersSilentOnRawMountReadOnly proves a readOnly: true raw
// mount doesn't count as a writer, mirroring the named-volume case.
func (s *VolumesSuite) TestCheckVolumeWritersSilentOnRawMountReadOnly() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "raw-mount-reader"},
		Steps: []schema.Step{
			{
				Name: "writer",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/data"},
				}},
			},
			{
				Name: "reader",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeVolume, Source: "shared-vol", Target: "/other", ReadOnly: true},
				}},
			},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

// TestCheckVolumeWritersIgnoresRawBindAndTmpfsMounts proves only
// MountTypeVolume is scanned — bind and tmpfs mounts are not named volumes.
func (s *VolumesSuite) TestCheckVolumeWritersIgnoresRawBindAndTmpfsMounts() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "raw-non-volume-mounts"},
		Steps: []schema.Step{
			{
				Name: "step-one",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeBind, Source: "/host/data", Target: "/data"},
				}},
			},
			{
				Name: "step-two",
				Spec: container.Spec{Mounts: []container.Mount{
					{Type: container.MountTypeTmpfs, Source: "/host/data", Target: "/data"},
				}},
			},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
}

package lint

import (
	"testing"

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

func (s *VolumesSuite) TestCheckVolumeWritersSilentOnDisjointSubPaths() {
	def := schema.Definition{
		Metadata: schema.Metadata{Alias: "disjoint-subpaths"},
		Steps: []schema.Step{
			{Name: "writer-root", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data"}}},
			{Name: "writer-reports", VolumeMounts: []schema.VolumeMount{{Volume: "shared", Path: "/data", SubPath: "reports"}}},
		},
	}

	s.Empty(CheckVolumeWriters([]schema.Definition{def}))
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
	s.Contains(warnings[0], `subPath "reports"`)
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

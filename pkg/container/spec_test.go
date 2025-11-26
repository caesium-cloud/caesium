package container

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type SpecSuite struct {
	suite.Suite
}

func (s *SpecSuite) TestHasEnv() {
	spec := Spec{}
	s.False(spec.HasEnv())
	spec.Env = map[string]string{"FOO": "bar"}
	s.True(spec.HasEnv())
}

func (s *SpecSuite) TestHasMounts() {
	spec := Spec{}
	s.False(spec.HasMounts())
	spec.Mounts = []Mount{{Type: MountTypeBind, Source: "/host", Target: "/data"}}
	s.True(spec.HasMounts())
}

func TestSpecSuite(t *testing.T) {
	suite.Run(t, new(SpecSuite))
}

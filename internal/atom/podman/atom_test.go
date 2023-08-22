package podman

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/stretchr/testify/assert"
)

func (s *PodmanTestSuite) TestAtom() {
	// valid states
	for podmanState, atomState := range stateMap {
		c := &Atom{
			metadata: newContainer(
				testAtomID,
				&define.InspectContainerState{
					Status:     podmanState,
					StartedAt:  time.Now(),
					FinishedAt: time.Now(),
				},
			),
		}

		assert.Equal(s.T(), testAtomID, c.ID())
		assert.Equal(s.T(), atomState, c.State())
		assert.NotZero(s.T(), c.CreatedAt())
		assert.NotZero(s.T(), c.StartedAt())
		assert.NotZero(s.T(), c.StoppedAt())
	}

	// invalid state
	c := &Atom{
		metadata: newContainer(
			testAtomID,
			&define.InspectContainerState{
				Status: "invalid",
			},
		),
	}

	assert.Equal(s.T(), atom.Invalid, c.State())

	// valid results
	for podmanResult, atomResult := range resultMap {
		c := &Atom{
			metadata: newContainer(
				testAtomID,
				&define.InspectContainerState{
					ExitCode: int32(podmanResult),
				},
			),
		}

		assert.Equal(s.T(), atomResult, c.Result())
		assert.NotZero(s.T(), c.CreatedAt())
		assert.Zero(s.T(), c.StartedAt())
		assert.Zero(s.T(), c.StoppedAt())
	}

	// unknown result
	c = &Atom{
		metadata: newContainer(
			testAtomID,
			&define.InspectContainerState{
				ExitCode: -1,
			},
		),
	}

	assert.Equal(s.T(), atom.Unknown, c.Result())
}

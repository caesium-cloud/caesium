package docker

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
)

func (s *DockerTestSuite) TestAtom() {
	// valid states
	for dockerState, atomState := range stateMap {
		c := &Atom{
			metadata: newContainer(
				testAtomID,
				&types.ContainerState{
					Status:     dockerState,
					StartedAt:  time.Now().Format(time.RFC3339Nano),
					FinishedAt: time.Now().Format(time.RFC3339Nano),
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
			&types.ContainerState{
				Status: "invalid",
			},
		),
	}

	assert.Equal(s.T(), atom.Invalid, c.State())

	// valid results
	for dockerResult, atomResult := range resultMap {
		c := &Atom{
			metadata: newContainer(
				testAtomID,
				&types.ContainerState{
					ExitCode: dockerResult,
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
			&types.ContainerState{
				ExitCode: -1,
			},
		),
	}

	assert.Equal(s.T(), atom.Unknown, c.Result())
}

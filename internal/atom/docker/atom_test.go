package docker

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
)

func (s *DockerTestSuite) TestAtom() {
	// valid states
	for _, dockerState := range []string{"created", "running", "paused", "restarting", "removing", "exited", "dead"} {
		atomState := atom.ContainerState(dockerState)
		c := &Atom{
			metadata: newContainer(
				testAtomID,
				&container.State{
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
			&container.State{
				Status: "invalid",
			},
		),
	}

	assert.Equal(s.T(), atom.Invalid, c.State())

	// valid results
	for _, dockerResult := range []int{0, 1, 125, 126, 127, 137, 143} {
		atomResult := atom.ResultForExitCode(dockerResult)
		c := &Atom{
			metadata: newContainer(
				testAtomID,
				&container.State{
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
			&container.State{
				ExitCode: -1,
			},
		),
	}

	assert.Equal(s.T(), atom.Unknown, c.Result())
}

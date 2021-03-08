package docker

import (
	"time"

	"github.com/caesium-dev/caesium/internal/capsule"
	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
)

func (s *DockerTestSuite) TestCapsule() {
	// valid states
	for dockerState, capsuleState := range stateMap {
		c := &Capsule{
			metadata: newContainer(
				testCapsuleID,
				&types.ContainerState{
					Status:     dockerState,
					StartedAt:  time.Now().Format(time.RFC3339Nano),
					FinishedAt: time.Now().Format(time.RFC3339Nano),
				},
			),
		}

		assert.Equal(s.T(), testCapsuleID, c.ID())
		assert.Equal(s.T(), capsuleState, c.State())
		assert.NotZero(s.T(), c.CreatedAt())
		assert.NotZero(s.T(), c.StartedAt())
		assert.NotZero(s.T(), c.StoppedAt())
	}

	// invalid state
	c := &Capsule{
		metadata: newContainer(
			testCapsuleID,
			&types.ContainerState{
				Status: "invalid",
			},
		),
	}

	assert.Equal(s.T(), capsule.Invalid, c.State())

	// valid results
	for dockerResult, capsuleResult := range resultMap {
		c := &Capsule{
			metadata: newContainer(
				testCapsuleID,
				&types.ContainerState{
					ExitCode: dockerResult,
				},
			),
		}

		assert.Equal(s.T(), capsuleResult, c.Result())
		assert.NotZero(s.T(), c.CreatedAt())
		assert.Zero(s.T(), c.StartedAt())
		assert.Zero(s.T(), c.StoppedAt())
	}

	// unknown result
	c = &Capsule{
		metadata: newContainer(
			testCapsuleID,
			&types.ContainerState{
				ExitCode: -1,
			},
		),
	}

	assert.Equal(s.T(), capsule.Unknown, c.Result())
}

package kubernetes

import (
	"time"

	"github.com/caesium-dev/caesium/internal/atom"
	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (s *KubernetesTestSuite) TestAtom() {
	// valid states
	for podPhase, atomState := range stateMap {
		c := &Atom{
			metadata: newPod(
				testAtomID,
				v1.PodStatus{
					Phase:     podPhase,
					StartTime: &metav1.Time{Time: time.Now()},
				},
				time.Now(),
				time.Now(),
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
		metadata: newPod(
			testAtomID,
			v1.PodStatus{
				Phase:     "invalid",
				StartTime: &metav1.Time{Time: time.Now()},
			},
			time.Now(),
			time.Now(),
		),
	}

	assert.Equal(s.T(), atom.Invalid, c.State())

	// valid results
	for podResult, atomResult := range resultMap {
		c := &Atom{
			metadata: newPod(
				testAtomID,
				v1.PodStatus{
					Phase: v1.PodFailed,
					ContainerStatuses: []v1.ContainerStatus{
						{
							State: v1.ContainerState{
								Terminated: &v1.ContainerStateTerminated{
									ExitCode: podResult,
								},
							},
						},
					},
				},
				time.Now(),
				time.Time{},
			),
		}

		assert.Equal(s.T(), atomResult, c.Result())
		assert.NotZero(s.T(), c.CreatedAt())
		assert.Zero(s.T(), c.StartedAt())
		assert.Zero(s.T(), c.StoppedAt())
	}

	// unknown result
	c = &Atom{
		metadata: newPod(
			testAtomID,
			v1.PodStatus{
				Phase: v1.PodFailed,
				ContainerStatuses: []v1.ContainerStatus{
					{
						State: v1.ContainerState{
							Terminated: &v1.ContainerStateTerminated{
								ExitCode: int32(-1),
							},
						},
					},
				},
			},
			time.Now(),
			time.Now(),
		),
	}

	assert.Equal(s.T(), atom.Unknown, c.Result())
}

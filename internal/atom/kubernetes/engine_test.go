package kubernetes

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func (s *KubernetesTestSuite) TestNewEngine() {
	engine := NewEngine(
		context.Background(),
		fake.NewClientset().CoreV1(),
	)
	assert.NotNil(s.T(), engine)
}

func (s *KubernetesTestSuite) TestGet() {
	req := &atom.EngineGetRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Get", testAtomID).
		Return()

	c, err := s.engine.Get(req)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), req.ID, c.ID())
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestGetError() {
	req := &atom.EngineGetRequest{}

	s.engine.backend.(*mockKubernetesBackend).
		On("Get", "").
		Return(fmt.Errorf("invalid pod name"))

	c, err := s.engine.Get(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestList() {
	req := &atom.EngineListRequest{}

	s.engine.backend.(*mockKubernetesBackend).On("List").Return()

	c, err := s.engine.List(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.Len(s.T(), c, 1)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestCreate() {
	req := &atom.EngineCreateRequest{
		Name:    testAtomID,
		Image:   testImage,
		Command: []string{"test", "cmd"},
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Create", mock.AnythingOfType("*v1.Pod")).
		Return()

	c, err := s.engine.Create(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), c)
	assert.True(s.T(), strings.HasPrefix(c.ID(), testAtomID))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestCreateAppliesSpec() {
	req := &atom.EngineCreateRequest{
		Name:    testAtomID,
		Image:   testImage,
		Command: []string{"test"},
		Spec: container.Spec{
			Env:     map[string]string{"FOO": "bar"},
			WorkDir: "/app",
			Mounts: []container.Mount{{
				Type:   container.MountTypeBind,
				Source: "/host",
				Target: "/data",
			}},
		},
	}

	podMatcher := mock.MatchedBy(func(pod *v1.Pod) bool {
		c := pod.Spec.Containers[0]
		if c.WorkingDir != "/app" || len(c.Env) != 1 || c.Env[0].Name != "FOO" || c.Env[0].Value != "bar" {
			return false
		}
		if len(c.VolumeMounts) != 1 || c.VolumeMounts[0].MountPath != "/data" {
			return false
		}
		if len(pod.Spec.Volumes) != 1 || pod.Spec.Volumes[0].HostPath == nil || pod.Spec.Volumes[0].HostPath.Path != "/host" {
			return false
		}
		return true
	})

	s.engine.backend.(*mockKubernetesBackend).
		On("Create", podMatcher).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestCreateAppliesResolvedVolumesAndIdentity() {
	automount := false
	req := &atom.EngineCreateRequest{
		Name:    testAtomID,
		Image:   testImage,
		Command: []string{"test"},
		Spec: container.Spec{
			Kubernetes: &container.KubernetesSpec{
				ServiceAccountName:           "caesium-deployer",
				PodAnnotations:               map[string]string{"iam": "enabled"},
				AutomountServiceAccountToken: &automount,
			},
			ResolvedVolumeMounts: []container.VolumeMount{
				{
					Name:     "work",
					Type:     container.VolumeMountTypePVC,
					Source:   "ci-shared-rwx",
					Target:   "/work",
					ReadOnly: true,
					SubPath:  "plans",
				},
				{
					Name:   "scratch",
					Type:   container.VolumeMountTypeClaimTemplate,
					Target: "/scratch",
					ClaimTemplate: &container.KubernetesClaimTemplate{
						StorageClass: "nfs-csi",
						Size:         "5Gi",
						AccessMode:   "ReadWriteOnce",
					},
				},
				{
					Name:   "nfs",
					Type:   container.VolumeMountTypeVolumeSource,
					Target: "/nfs",
					VolumeSource: map[string]any{
						"nfs": map[string]any{"server": "10.0.0.5", "path": "/export/caesium"},
					},
				},
			},
		},
	}

	podMatcher := mock.MatchedBy(func(pod *v1.Pod) bool {
		if pod.Spec.ServiceAccountName != "caesium-deployer" {
			return false
		}
		if pod.Annotations["iam"] != "enabled" {
			return false
		}
		if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
			return false
		}
		c := pod.Spec.Containers[0]
		if len(c.VolumeMounts) != 3 || c.VolumeMounts[0].MountPath != "/work" || !c.VolumeMounts[0].ReadOnly || c.VolumeMounts[0].SubPath != "plans" {
			return false
		}
		if len(pod.Spec.Volumes) != 3 {
			return false
		}
		if pod.Spec.Volumes[0].PersistentVolumeClaim == nil || pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "ci-shared-rwx" {
			return false
		}
		if pod.Spec.Volumes[1].Ephemeral == nil || pod.Spec.Volumes[1].Ephemeral.VolumeClaimTemplate == nil {
			return false
		}
		claim := pod.Spec.Volumes[1].Ephemeral.VolumeClaimTemplate.Spec
		if claim.StorageClassName == nil || *claim.StorageClassName != "nfs-csi" {
			return false
		}
		if len(claim.AccessModes) != 1 || claim.AccessModes[0] != v1.ReadWriteOnce {
			return false
		}
		if _, ok := claim.Resources.Requests[v1.ResourceStorage]; !ok {
			return false
		}
		return pod.Spec.Volumes[2].NFS != nil &&
			pod.Spec.Volumes[2].NFS.Server == "10.0.0.5" &&
			pod.Spec.Volumes[2].NFS.Path == "/export/caesium"
	})

	s.engine.backend.(*mockKubernetesBackend).
		On("Create", podMatcher).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

// TestCreateDelegatesToKueue asserts that a step declaring a Kueue queue stamps
// the kueue.x-k8s.io/queue-name label on the created pod (and preserves the
// Caesium management label). The label is all Caesium sets — Kueue's webhook
// gates the pod for admission, which is how scheduling is delegated rather than
// performed by Caesium.
func (s *KubernetesTestSuite) TestCreateDelegatesToKueue() {
	req := &atom.EngineCreateRequest{
		Name:    testAtomID,
		Image:   testImage,
		Command: []string{"test"},
		Spec: container.Spec{
			Kubernetes: &container.KubernetesSpec{
				QueueName: "data-eng",
			},
		},
	}

	podMatcher := mock.MatchedBy(func(pod *v1.Pod) bool {
		if pod.Labels[kueueQueueLabel] != "data-eng" {
			return false
		}
		// The Caesium management label must not be clobbered by the queue label.
		if _, ok := pod.Labels[atom.Label]; !ok {
			return false
		}
		return true
	})

	s.engine.backend.(*mockKubernetesBackend).
		On("Create", podMatcher).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

// TestCreateNoQueueOmitsKueueLabel asserts the queue label is absent when no
// queue is declared, so non-Kueue clusters are unaffected.
func (s *KubernetesTestSuite) TestCreateNoQueueOmitsKueueLabel() {
	req := &atom.EngineCreateRequest{
		Name:    testAtomID,
		Image:   testImage,
		Command: []string{"test"},
	}

	podMatcher := mock.MatchedBy(func(pod *v1.Pod) bool {
		_, hasQueue := pod.Labels[kueueQueueLabel]
		return !hasQueue
	})

	s.engine.backend.(*mockKubernetesBackend).
		On("Create", podMatcher).
		Return()

	_, err := s.engine.Create(req)
	s.Require().NoError(err)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestCreateError() {
	req := &atom.EngineCreateRequest{
		Name:    "",
		Image:   testImage,
		Command: []string{"test", "cmd"},
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Create", mock.AnythingOfType("*v1.Pod")).
		Return(fmt.Errorf("invalid pod name"))

	c, err := s.engine.Create(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), c)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestStop() {
	req := &atom.EngineStopRequest{
		ID: testAtomID,
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Delete", testAtomID).
		Return()

	assert.Nil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestStopError() {
	req := &atom.EngineStopRequest{
		ID: "",
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Delete", "").
		Return(fmt.Errorf("invalid pod name"))

	assert.NotNil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestStopTimeout() {
	req := &atom.EngineStopRequest{
		ID:      testAtomID,
		Timeout: time.Nanosecond,
		Force:   true,
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("Delete", testAtomID).
		Return()

	assert.NotNil(s.T(), s.engine.Stop(req))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestLogs() {
	req := &atom.EngineLogsRequest{
		ID:    testAtomID,
		Since: time.Now(),
	}

	s.engine.backend.(*mockKubernetesBackend).
		On("GetLogs", testAtomID).
		Return()

	logs, err := s.engine.Logs(req)
	assert.Nil(s.T(), err)
	assert.NotNil(s.T(), logs)

	buf, err := io.ReadAll(logs)
	assert.Nil(s.T(), err)
	assert.Equal(s.T(), "logs", string(buf))
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

func (s *KubernetesTestSuite) TestLogsError() {
	req := &atom.EngineLogsRequest{ID: ""}

	s.engine.backend.(*mockKubernetesBackend).
		On("GetLogs", "").
		Return()

	logs, err := s.engine.Logs(req)
	assert.NotNil(s.T(), err)
	assert.Nil(s.T(), logs)
	s.engine.backend.(*mockKubernetesBackend).AssertExpectations(s.T())
}

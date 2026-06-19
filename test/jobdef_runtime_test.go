//go:build integration

package test

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/require"
)

func (s *IntegrationTestSuite) TestJobApplyPersistsVolumeAndWorkloadIdentityRuntimeSpecs() {
	suffix := time.Now().UnixNano()
	volumeAlias := fmt.Sprintf("integration-volume-runtime-%d", suffix)
	identityAlias := fmt.Sprintf("integration-identity-runtime-%d", suffix)
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %[1]s
trigger:
  type: http
  configuration:
    path: "/hooks/integration/%[1]s"
volumes:
  - name: workspace
    sources:
      docker:
        volume: caesium-integration-workspace
      podman:
        volume: caesium-integration-workspace
      kubernetes:
        pvc: caesium-integration-workspace-rwx
steps:
  - name: prepare-artifact
    engine: docker
    image: alpine:3.23
    command: ["sh", "-c", "echo prepared > /workspace/result.txt"]
    volumeMounts:
      - {volume: workspace, path: /workspace}
    next: verify-artifact
  - name: verify-artifact
    engine: docker
    image: alpine:3.23
    dependsOn: [prepare-artifact]
    command: ["sh", "-c", "test -s /workspace/result.txt"]
    volumeMounts:
      - {volume: workspace, path: /workspace, readOnly: true}
---
apiVersion: v1
kind: Job
metadata:
  alias: %[2]s
  serviceAccountName: caesium-report-reader
  podAnnotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/caesium-report-reader
  automountServiceAccountToken: false
trigger:
  type: http
  configuration:
    path: "/hooks/integration/%[2]s"
volumes:
  - name: cloud-workspace
    sources:
      kubernetes:
        pvc: caesium-cloud-workspace-rwx
steps:
  - name: plan-access
    engine: kubernetes
    image: alpine:3.23
    command: ["sh", "-c", "echo plan"]
    volumeMounts:
      - {volume: cloud-workspace, path: /workspace}
    next: write-cloud-report
  - name: write-cloud-report
    engine: kubernetes
    image: alpine:3.23
    dependsOn: [plan-access]
    serviceAccountName: caesium-cloud-writer
    podAnnotations:
      eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/caesium-cloud-writer
    automountServiceAccountToken: true
    command: ["sh", "-c", "echo write"]
    volumeMounts:
      - {volume: cloud-workspace, path: /workspace, readOnly: true, subPath: reports}
`, volumeAlias, identityAlias)

	dir := s.writeRawJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	volumeJob := s.requireJobByAlias(volumeAlias)
	volumeSpecs := s.fetchAtomSpecsByTaskName(volumeJob.ID)
	require.Len(s.T(), volumeSpecs, 2)

	prepareMount := requireSingleResolvedMount(s.T(), volumeSpecs["prepare-artifact"])
	require.Equal(s.T(), container.VolumeMountTypeVolume, prepareMount.Type)
	require.Equal(s.T(), "workspace", prepareMount.Name)
	require.Equal(s.T(), "caesium-integration-workspace", prepareMount.Source)
	require.Equal(s.T(), "/workspace", prepareMount.Target)
	require.False(s.T(), prepareMount.ReadOnly)

	verifyMount := requireSingleResolvedMount(s.T(), volumeSpecs["verify-artifact"])
	require.Equal(s.T(), container.VolumeMountTypeVolume, verifyMount.Type)
	require.Equal(s.T(), "caesium-integration-workspace", verifyMount.Source)
	require.Equal(s.T(), "/workspace", verifyMount.Target)
	require.True(s.T(), verifyMount.ReadOnly)

	identityJob := s.requireJobByAlias(identityAlias)
	identitySpecs := s.fetchAtomSpecsByTaskName(identityJob.ID)
	require.Len(s.T(), identitySpecs, 2)

	planSpec := identitySpecs["plan-access"]
	planMount := requireSingleResolvedMount(s.T(), planSpec)
	require.Equal(s.T(), container.VolumeMountTypePVC, planMount.Type)
	require.Equal(s.T(), "caesium-cloud-workspace-rwx", planMount.Source)
	require.Equal(s.T(), "/workspace", planMount.Target)
	require.NotNil(s.T(), planSpec.Kubernetes)
	require.Equal(s.T(), "caesium-report-reader", planSpec.Kubernetes.ServiceAccountName)
	require.Equal(
		s.T(),
		map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/caesium-report-reader"},
		planSpec.Kubernetes.PodAnnotations,
	)
	require.NotNil(s.T(), planSpec.Kubernetes.AutomountServiceAccountToken)
	require.False(s.T(), *planSpec.Kubernetes.AutomountServiceAccountToken)

	writeSpec := identitySpecs["write-cloud-report"]
	writeMount := requireSingleResolvedMount(s.T(), writeSpec)
	require.Equal(s.T(), container.VolumeMountTypePVC, writeMount.Type)
	require.Equal(s.T(), "caesium-cloud-workspace-rwx", writeMount.Source)
	require.Equal(s.T(), "/workspace", writeMount.Target)
	require.True(s.T(), writeMount.ReadOnly)
	require.Equal(s.T(), "reports", writeMount.SubPath)
	require.NotNil(s.T(), writeSpec.Kubernetes)
	require.Equal(s.T(), "caesium-cloud-writer", writeSpec.Kubernetes.ServiceAccountName)
	require.Equal(
		s.T(),
		map[string]string{"eks.amazonaws.com/role-arn": "arn:aws:iam::123456789012:role/caesium-cloud-writer"},
		writeSpec.Kubernetes.PodAnnotations,
	)
	require.NotNil(s.T(), writeSpec.Kubernetes.AutomountServiceAccountToken)
	require.True(s.T(), *writeSpec.Kubernetes.AutomountServiceAccountToken)
}

func (s *IntegrationTestSuite) writeRawJobManifest(contents string) string {
	dir, err := os.MkdirTemp("", "caesium-runtime-job-*")
	require.NoError(s.T(), err)

	path := filepath.Join(dir, "runtime.job.yaml")
	require.NoError(s.T(), os.WriteFile(path, []byte(contents), 0o644))
	return dir
}

func (s *IntegrationTestSuite) fetchAtomSpecsByTaskName(jobID string) map[string]container.Spec {
	tasks := s.jobTasks(jobID)
	specs := make(map[string]container.Spec, len(tasks))
	for _, task := range tasks {
		name := taskString(task, "name")
		atomID := taskString(task, "AtomID", "atom_id")
		require.NotEmptyf(s.T(), name, "task response missing name: %#v", task)
		require.NotEmptyf(s.T(), atomID, "task response missing atom id: %#v", task)
		specs[name] = s.fetchAtomSpec(atomID)
	}
	return specs
}

func taskString(task map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := valueFromMap(task, key); value != nil {
			if str, ok := value.(string); ok {
				return str
			}
		}
	}
	return ""
}

func requireSingleResolvedMount(t require.TestingT, spec container.Spec) container.VolumeMount {
	require.Len(t, spec.ResolvedVolumeMounts, 1)
	return spec.ResolvedVolumeMounts[0]
}

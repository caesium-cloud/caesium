//go:build integration

package test

import (
	"fmt"
	"time"
)

// TestBlameCLIAttributesVolumeSourceEdit pins that a job-level volume source
// edit (the PVC claim behind a mounted volume) writes a snapshot and is
// attributed to the editing commit, though the step itself is unchanged.
func (s *IntegrationTestSuite) TestBlameCLIAttributesVolumeSourceEdit() {
	alias := fmt.Sprintf("e2e-blame-volume-%d", time.Now().UnixNano())
	jobID := s.applyBlameManifestWithCommit(alias, blameVolumeManifest(alias, "claim-a", "pw-one-literal"), "blame-volume-1")
	s.applyBlameManifestWithCommit(alias, blameVolumeManifest(alias, "claim-a", "pw-one-literal"), "blame-volume-2")
	s.Require().Len(s.blameTopologyHistory(jobID).Snapshots, 1, "an identical re-apply must dedup")

	s.applyBlameManifestWithCommit(alias, blameVolumeManifest(alias, "claim-b", "pw-one-literal"), "blame-volume-3")
	s.Require().Len(s.blameTopologyHistory(jobID).Snapshots, 2, "a PVC claim edit must write a snapshot")
	res := s.runBlameJSON(alias)
	work := requireBlameTask(s, res.Tasks, "work")
	s.Equal("blame-volume-3", work.IntroducingCommit)
	s.Contains(fmt.Sprint(work.Element.Behavior["volumeSources"]), "claim-b")

	// A credential inside a raw Kubernetes volumeSource is digested, never
	// stored: absent from blame --json stdout and the topology history, while
	// editing it is still detected.
	s.applyBlameManifestWithCommit(alias, blameVolumeManifest(alias, "claim-b", "pw-two-literal"), "blame-volume-4")
	s.Require().Len(s.blameTopologyHistory(jobID).Snapshots, 3, "a volume credential edit must write a snapshot")
	raw, err := s.runCLIStdout("blame", alias, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	for _, literal := range []string{"pw-one-literal", "pw-two-literal"} {
		s.NotContains(raw, literal, "a volume source credential must not appear in blame output")
		var hist map[string]any
		s.getJSON(fmt.Sprintf("/v1/jobs/%s/topology/history", jobID), &hist)
		s.NotContains(fmt.Sprint(hist), literal, "...nor in the topology history")
	}
}

func blameVolumeManifest(alias, claim, password string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
volumes:
  - name: data
    sources:
      docker:
        volume: blame-data
      podman:
        volume: blame-data
      kubernetes:
        pvc: %s
  - name: creds
    sources:
      docker:
        volume: blame-creds
      podman:
        volume: blame-creds
      kubernetes:
        volumeSource:
          flexVolume:
            driver: example/cifs
            options:
              password: %s
steps:
  - name: work
    image: alpine:3.23
    command: ["sh","-c","echo work"]
    volumeMounts:
      - volume: data
        path: /data
      - volume: creds
        path: /creds
`, alias, claim, password)
}

//go:build integration

package cluster

import (
	"fmt"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

const (
	BlockStep     = "block"
	SuccessorStep = "successor"
	ProbeStep     = "probe"
)

func RecorderURL() string {
	return fmt.Sprintf("http://%s:%d", "robustness-recorder", 8090)
}

func ProbeDefinition(alias, probeID, taskImage string) jobdef.Definition {
	script := fmt.Sprintf(`set -eu
RECORDER=%q
PROBE_ID=%q
wget -qO- --header='Content-Type: application/json' --post-data="{\"id\":\"${PROBE_ID}\"}" "${RECORDER}/probe"
wget -qO- "${RECORDER}/probe/${PROBE_ID}"
`, RecorderURL(), probeID)
	return jobdef.Definition{
		APIVersion: jobdef.APIVersionV1,
		Kind:       jobdef.KindJob,
		Metadata: jobdef.Metadata{
			Alias: alias,
			Labels: map[string]string{
				"caesium-robustness": "probe",
			},
		},
		Trigger: jobdef.Trigger{
			Type: jobdef.TriggerHTTP,
			Configuration: map[string]any{
				"path": "robustness-probe-" + alias,
			},
		},
		Steps: []jobdef.Step{
			{
				Name:    ProbeStep,
				Engine:  jobdef.EngineKubernetes,
				Image:   taskImage,
				Command: []string{"sh", "-c", script},
			},
		},
	}
}

func FixtureDefinition(alias, taskImage string) jobdef.Definition {
	block := fmt.Sprintf(`set -eu
RECORDER=%q
STEP=%q
NONCE="$(cat /proc/sys/kernel/random/uuid)"
wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"start\"}" "${RECORDER}/start"
i=0
while [ "$i" -lt 180 ]; do
  if wget -qO- "${RECORDER}/wait?run_id=${CAESIUM_RUN_ID}" | grep -q released; then
    wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"complete\"}" "${RECORDER}/effect" >/dev/null || true
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done
exit 1
`, RecorderURL(), BlockStep)

	successor := fmt.Sprintf(`set -eu
RECORDER=%q
STEP=%q
NONCE="$(cat /proc/sys/kernel/random/uuid)"
wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"start\"}" "${RECORDER}/start"
wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"complete\"}" "${RECORDER}/effect"
`, RecorderURL(), SuccessorStep)

	return jobdef.Definition{
		APIVersion: jobdef.APIVersionV1,
		Kind:       jobdef.KindJob,
		Metadata: jobdef.Metadata{
			Alias: alias,
			Labels: map[string]string{
				"caesium-robustness": "owner-crash",
			},
		},
		Trigger: jobdef.Trigger{
			Type: jobdef.TriggerHTTP,
			Configuration: map[string]any{
				"path": "robustness-owner-crash-" + alias,
			},
		},
		Steps: []jobdef.Step{
			{
				Name:    BlockStep,
				Engine:  jobdef.EngineKubernetes,
				Image:   taskImage,
				Command: []string{"sh", "-c", block},
				Next:    []string{SuccessorStep},
			},
			{
				Name:      SuccessorStep,
				Engine:    jobdef.EngineKubernetes,
				Image:     taskImage,
				Command:   []string{"sh", "-c", successor},
				DependsOn: []string{BlockStep},
			},
		},
	}
}

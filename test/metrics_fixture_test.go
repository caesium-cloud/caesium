//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metricsFixtureImage is the canonical pinned image every data-assertions
// metrics-emitting fixture step must use — exactly this tag, never an
// untagged reference. The repo's image-pin guardrail
// (internal/guardrails/guardrails_test.go,
// TestPinnedContainerImageVersionsAreConsistent) scans this file's source
// text for non-canonical base-image refs, so this constant is the ONLY place
// a metrics fixture may spell the image.
const metricsFixtureImage = "alpine:3.23"

// metricsMarkerLine renders one ##caesium::metrics marker line exactly as it
// would appear on a step's real stdout: the marker stem followed by a flat
// JSON object, per pkg/task/output.go's documented payload shape. dataset,
// when non-empty, is written into the payload's "dataset" key, selecting
// which declared dataset the metrics attach to; pass "" to omit it and rely
// on the step's sole declared dataset (the marker errors at parse time if
// that is ambiguous). metrics supplies the remaining flat key/value pairs —
// use a Go float64/int for a numeric metric value and a string for an
// RFC3339 watermark value.
func metricsMarkerLine(dataset string, metrics map[string]any) (string, error) {
	payload := make(map[string]any, len(metrics)+1)
	for k, v := range metrics {
		payload[k] = v
	}
	if dataset != "" {
		payload["dataset"] = dataset
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal metrics payload: %w", err)
	}
	return "##caesium::metrics " + string(body), nil
}

// metricsProducerStep returns a `steps:` list entry (2-space indented,
// matching the inline job manifests built elsewhere in this package via
// fmt.Sprintf) named stepName that runs on the canonical metricsFixtureImage
// and echoes one ##caesium::metrics line per element of lines, in order — so
// Stream A/B/C scenarios can drive the live metrics-persistence and
// assertion-evaluator surface with a real container run instead of an
// internal function call. Passing more than one element exercises the
// marker's documented last-write-wins-per-(dataset, metric) merge across
// lines.
//
// This helper deliberately knows nothing about the jobdef
// datasets.produces/assertions shape (that schema is this plan's Stream A) —
// callers embed the returned step under their own job manifest's `steps:`
// list, alongside a `datasets:` block declaring the dataset the metrics
// belong to.
func metricsProducerStep(stepName string, lines ...map[string]any) (string, error) {
	return metricsProducerStepForDataset(stepName, "", lines...)
}

// metricsProducerStepForDataset is metricsProducerStep with an explicit
// "dataset" field set on every emitted marker line. Use it when a scenario's
// step needs to name the dataset explicitly (e.g. the step declares more
// than one produced dataset, so the marker's implicit "sole declared
// dataset" default does not apply).
func metricsProducerStepForDataset(stepName, dataset string, lines ...map[string]any) (string, error) {
	if len(lines) == 0 {
		return "", fmt.Errorf("metricsProducerStepForDataset(%q): at least one metrics line is required", stepName)
	}

	echoes := make([]string, 0, len(lines))
	for _, m := range lines {
		line, err := metricsMarkerLine(dataset, m)
		if err != nil {
			return "", err
		}
		// Escape embedded double quotes for the surrounding YAML
		// double-quoted command element, mirroring the existing
		// ##caesium::output fixtures elsewhere in this package (e.g.
		// `echo '##caesium::output {\"rows\": \"100\"}'`).
		echoes = append(echoes, fmt.Sprintf("echo '%s'", strings.ReplaceAll(line, `"`, `\"`)))
	}

	return fmt.Sprintf(`  - name: %s
    image: %s
    command: ["sh","-c","%s"]
`, stepName, metricsFixtureImage, strings.Join(echoes, "; ")), nil
}

// TestMetricsProducerStepEmitsCanonicalMarkerYAML is a pure logic self-test
// for this fixture (no live server needed): it pins the exact marker/YAML
// shape Stream A/B/C scenarios rely on — the canonical alpine:3.23 image, the
// "##caesium::metrics" stem, the dataset field, and the last-write-wins
// multi-line join — so a change to the escaping or formatting here fails
// loudly at the fixture instead of silently corrupting every scenario that
// embeds it.
func TestMetricsProducerStepEmitsCanonicalMarkerYAML(t *testing.T) {
	step, err := metricsProducerStepForDataset("emit", "orders",
		map[string]any{"rowCount": 100},
		map[string]any{"rowCount": 101},
	)
	require.NoError(t, err)
	assert.Contains(t, step, "image: alpine:3.23")
	assert.Contains(t, step, "name: emit")
	assert.Contains(t, step, `echo '##caesium::metrics {\"dataset\":\"orders\",\"rowCount\":100}'`)
	assert.Contains(t, step, `echo '##caesium::metrics {\"dataset\":\"orders\",\"rowCount\":101}'`)

	_, err = metricsProducerStep("emit")
	assert.Error(t, err, "at least one metrics line is required")
}

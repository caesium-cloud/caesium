package jobdef

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertionsJob is the design's worked example, expressed in the SHIPPED YAML
// nesting (steps[].datasets.produces, keyed on `name`).
const assertionsJob = `
apiVersion: v1
kind: Job
metadata:
  alias: transactions-daily
  onUpstreamHold: skip
trigger:
  type: cron
  configuration:
    expression: "0 2 * * *"
steps:
  - name: load-transactions
    image: etl/load:1.4
    datasets:
      produces:
        - name: warehouse/transactions_daily
          assertions:
            rowCount: {min: 1000, deltaFromBaseline: 50%}
            nullRate: {metric: null_rate_customer_id, max: 0.02}
            freshness: {watermark: max_event_time, maxLag: 26h}
            custom:
              - {metric: dedup_ratio, max: 0.05}
          onViolation: hold
          release: manual
  - name: publish-report
    image: etl/report:2.0
    datasets:
      consumes: [warehouse/transactions_daily]
`

func parseAndValidate(t *testing.T, y string) (*Definition, error) {
	t.Helper()
	// Parse already runs Validate, so an invalid manifest fails there; the
	// explicit Validate call keeps this helper honest for definitions that
	// arrive some other way (the server re-validates decoded JSON).
	def, err := Parse([]byte(y))
	if err != nil {
		return nil, err
	}
	return def, def.Validate()
}

func TestAssertions_ParseAndValidate(t *testing.T) {
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")

	def, err := parseAndValidate(t, assertionsJob)
	require.NoError(t, err)

	produced := def.Steps[0].Datasets.Produces[0]
	require.NotNil(t, produced.Assertions)
	assert.Equal(t, DatasetOnViolationHold, produced.OnViolation)
	assert.Equal(t, DatasetReleaseManual, produced.EffectiveRelease())
	assert.Equal(t, OnUpstreamHoldSkip, def.Metadata.EffectiveOnUpstreamHold())

	require.NotNil(t, produced.Assertions.RowCount)
	require.NotNil(t, produced.Assertions.RowCount.Min)
	assert.InDelta(t, 1000, *produced.Assertions.RowCount.Min, 0.001)
	assert.Equal(t, "50%", produced.Assertions.RowCount.DeltaFromBaseline)

	require.NotNil(t, produced.Assertions.NullRate)
	assert.Equal(t, "null_rate_customer_id", produced.Assertions.NullRate.Metric)
	require.NotNil(t, produced.Assertions.NullRate.Max)
	assert.InDelta(t, 0.02, *produced.Assertions.NullRate.Max, 0.0001)

	require.NotNil(t, produced.Assertions.Freshness)
	assert.Equal(t, "max_event_time", produced.Assertions.Freshness.Watermark)
	assert.Equal(t, "26h", produced.Assertions.Freshness.MaxLag)

	require.Len(t, produced.Assertions.Custom, 1)
	assert.Equal(t, "dedup_ratio", produced.Assertions.Custom[0].Metric)

	// The block must survive the CLI→server JSON round trip unchanged: the
	// server re-validates the definition it receives as JSON, not YAML.
	encoded, err := json.Marshal(def)
	require.NoError(t, err)
	var roundTripped Definition
	require.NoError(t, json.Unmarshal(encoded, &roundTripped))
	require.NoError(t, roundTripped.Validate())
	assert.Equal(t, "50%", roundTripped.Steps[0].Datasets.Produces[0].Assertions.RowCount.DeltaFromBaseline)
	assert.Equal(t, "skip", roundTripped.Metadata.OnUpstreamHold)
}

// TestAssertions_InertWhenFlagOff pins arc convention 1: a manifest declaring
// the new surface is REFUSED with a message naming the gate, rather than
// applying as a silent no-op.
func TestAssertions_InertWhenFlagOff(t *testing.T) {
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")

	_, err := parseAndValidate(t, assertionsJob)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CAESIUM_DATA_ASSERTIONS_ENABLED=true")
}

func TestAssertions_OnUpstreamHoldGatedAndValidated(t *testing.T) {
	const y = `
apiVersion: v1
kind: Job
metadata:
  alias: consumer
  onUpstreamHold: %s
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: read
    image: etl:1
    datasets:
      consumes: [warehouse/orders]
`
	t.Run("gated off", func(t *testing.T) {
		t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "false")
		_, err := parseAndValidate(t, fmt.Sprintf(y, "run"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "CAESIUM_DATA_ASSERTIONS_ENABLED=true")
	})

	t.Run("bad value", func(t *testing.T) {
		t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
		_, err := parseAndValidate(t, fmt.Sprintf(y, "maybe"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "metadata.onUpstreamHold")
	})

	t.Run("accepted", func(t *testing.T) {
		t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
		def, err := parseAndValidate(t, fmt.Sprintf(y, "run"))
		require.NoError(t, err)
		assert.Equal(t, OnUpstreamHoldRun, def.Metadata.EffectiveOnUpstreamHold())
	})

	t.Run("unset defaults to skip", func(t *testing.T) {
		t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")
		var m Metadata
		assert.Equal(t, OnUpstreamHoldSkip, m.EffectiveOnUpstreamHold())
	})
}

func TestAssertions_RejectsBadDeclarations(t *testing.T) {
	t.Setenv("CAESIUM_DATA_ASSERTIONS_ENABLED", "true")

	cases := []struct {
		name     string
		produces string
		contains string
	}{
		{
			name:     "unknown onViolation",
			produces: "assertions: {rowCount: {min: 1}}\n          onViolation: explode",
			contains: "onViolation",
		},
		{
			name:     "unknown release",
			produces: "assertions: {rowCount: {min: 1}}\n          release: eventually",
			contains: "release",
		},
		{
			name:     "onViolation without assertions",
			produces: "onViolation: hold",
			contains: "no assertions are declared",
		},
		{
			name:     "assertion with no bound",
			produces: "assertions: {rowCount: {}}",
			contains: "at least one of min, max or deltaFromBaseline",
		},
		{
			name:     "min above max",
			produces: "assertions: {rowCount: {min: 10, max: 1}}",
			contains: "must not exceed max",
		},
		{
			name:     "deltaFromBaseline without a percentage",
			produces: "assertions: {rowCount: {deltaFromBaseline: \"50\"}}",
			contains: "percentage of the baseline median",
		},
		{
			name:     "negative deltaFromBaseline",
			produces: "assertions: {rowCount: {deltaFromBaseline: \"-5%\"}}",
			contains: "positive percentage",
		},
		{
			name:     "custom entry without a metric",
			produces: "assertions: {custom: [{max: 1}]}",
			contains: "custom[0].metric is required",
		},
		{
			name:     "freshness without a watermark",
			produces: "assertions: {freshness: {maxLag: 26h}}",
			contains: "freshness.watermark is required",
		},
		{
			name:     "freshness without maxLag",
			produces: "assertions: {freshness: {watermark: max_event_time}}",
			contains: "freshness.maxLag is required",
		},
		{
			name:     "freshness with an unparseable maxLag",
			produces: "assertions: {freshness: {watermark: max_event_time, maxLag: soon}}",
			contains: "must be a valid duration",
		},
		{
			name:     "two assertions on one metric",
			produces: "assertions: {rowCount: {min: 1}, custom: [{metric: rowCount, max: 5}]}",
			contains: "both assert on metric",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := `
apiVersion: v1
kind: Job
metadata:
  alias: bad-assertions
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: load
    image: etl:1
    datasets:
      produces:
        - name: warehouse/orders
          ` + tc.produces + "\n"
			_, err := parseAndValidate(t, y)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.contains)
		})
	}
}

func TestParseDeltaFromBaseline(t *testing.T) {
	frac, err := ParseDeltaFromBaseline("50%")
	require.NoError(t, err)
	assert.InDelta(t, 0.5, frac, 0.0001)

	frac, err = ParseDeltaFromBaseline(" 12.5% ")
	require.NoError(t, err)
	assert.InDelta(t, 0.125, frac, 0.0001)

	_, err = ParseDeltaFromBaseline("50")
	require.Error(t, err)

	_, err = ParseDeltaFromBaseline("0%")
	require.Error(t, err)
}

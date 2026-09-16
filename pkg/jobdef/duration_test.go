package jobdef

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseJSONDuration(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		field   string
		want    time.Duration
		wantErr string
	}{
		{name: "string seconds", raw: `"1s"`, field: "retryDelay", want: time.Second},
		{name: "string thirty seconds", raw: `"30s"`, field: "retryDelay", want: 30 * time.Second},
		{name: "string minute", raw: `"1m"`, field: "runTimeout", want: time.Minute},
		{name: "integer nanoseconds", raw: `1000000000`, field: "retryDelay", want: time.Second},
		{name: "zero integer", raw: `0`, field: "taskTimeout", want: 0},
		{name: "null", raw: `null`, field: "retryDelay", want: 0},
		{name: "empty", raw: ``, field: "retryDelay", want: 0},
		{name: "scientific integer", raw: `1e9`, field: "retryDelay", want: time.Second},
		{name: "integral decimal", raw: `1.0`, field: "retryDelay", want: time.Nanosecond},
		{
			name:  "max int64",
			raw:   strconv.FormatInt(math.MaxInt64, 10),
			field: "retryDelay",
			want:  time.Duration(math.MaxInt64),
		},
		{
			name:  "min int64",
			raw:   strconv.FormatInt(math.MinInt64, 10),
			field: "retryDelay",
			want:  time.Duration(math.MinInt64),
		},
		{
			name:  "mantissa beyond float64 exact integer",
			raw:   `9007199254740993e0`,
			field: "retryDelay",
			want:  time.Duration(9007199254740993),
		},
		{
			name:    "overflow max int64 plus one",
			raw:     `9223372036854775808`,
			field:   "retryDelay",
			wantErr: "retryDelay must be a duration string (e.g. 1s, 30s) or integer nanoseconds",
		},
		{
			name:    "overflow min int64 minus one",
			raw:     `-9223372036854775809`,
			field:   "retryDelay",
			wantErr: "retryDelay must be a duration string (e.g. 1s, 30s) or integer nanoseconds",
		},
		{
			name:    "scientific overflow",
			raw:     `1e19`,
			field:   "retryDelay",
			wantErr: "retryDelay must be a duration string (e.g. 1s, 30s) or integer nanoseconds",
		},
		{
			name:    "invalid string",
			raw:     `"not-a-duration"`,
			field:   "retryDelay",
			wantErr: "retryDelay must be a duration string (e.g. 1s, 30s) or integer nanoseconds",
		},
		{
			name:    "bool",
			raw:     `true`,
			field:   "taskTimeout",
			wantErr: "taskTimeout must be a duration string (e.g. 1s, 30s) or integer nanoseconds",
		},
		{
			name:    "fractional number",
			raw:     `1.5`,
			field:   "runTimeout",
			wantErr: "runTimeout must be a duration string (e.g. 1s, 30s) or integer nanoseconds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseJSONDuration(json.RawMessage(tt.raw), tt.field)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				require.NotContains(t, err.Error(), "cannot unmarshal")
				require.NotContains(t, err.Error(), "rawStep")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestStepUnmarshalJSONDurationString(t *testing.T) {
	t.Parallel()

	var step Step
	err := json.Unmarshal([]byte(`{"name":"extract","image":"alpine:3.23","retryDelay":"1s"}`), &step)
	require.NoError(t, err)
	require.Equal(t, time.Second, step.RetryDelay)
}

func TestStepUnmarshalJSONDurationNanoseconds(t *testing.T) {
	t.Parallel()

	var step Step
	err := json.Unmarshal([]byte(`{"name":"extract","image":"alpine:3.23","retryDelay":1000000000}`), &step)
	require.NoError(t, err)
	require.Equal(t, time.Second, step.RetryDelay)
}

func TestStepUnmarshalJSONInvalidDurationNamesField(t *testing.T) {
	t.Parallel()

	var step Step
	err := json.Unmarshal([]byte(`{"name":"extract","image":"alpine:3.23","retryDelay":"bogus"}`), &step)
	require.Error(t, err)
	require.EqualError(t, err, "retryDelay must be a duration string (e.g. 1s, 30s) or integer nanoseconds")
	require.NotContains(t, err.Error(), "cannot unmarshal")
	require.NotContains(t, err.Error(), "rawStep")
}

func TestMetadataUnmarshalJSONDurationStrings(t *testing.T) {
	t.Parallel()

	var md Metadata
	err := json.Unmarshal([]byte(`{"alias":"qa","taskTimeout":"30s","runTimeout":"1m"}`), &md)
	require.NoError(t, err)
	require.Equal(t, "qa", md.Alias)
	require.Equal(t, 30*time.Second, md.TaskTimeout)
	require.Equal(t, time.Minute, md.RunTimeout)
}

func TestMetadataUnmarshalJSONDurationNanoseconds(t *testing.T) {
	t.Parallel()

	var md Metadata
	err := json.Unmarshal([]byte(`{"alias":"qa","taskTimeout":30000000000,"runTimeout":60000000000}`), &md)
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, md.TaskTimeout)
	require.Equal(t, time.Minute, md.RunTimeout)
}

func TestMetadataUnmarshalJSONInvalidTimeoutNamesField(t *testing.T) {
	t.Parallel()

	var md Metadata
	err := json.Unmarshal([]byte(`{"alias":"qa","taskTimeout":"nope"}`), &md)
	require.EqualError(t, err, "taskTimeout must be a duration string (e.g. 1s, 30s) or integer nanoseconds")
	require.NotContains(t, err.Error(), "cannot unmarshal")

	err = json.Unmarshal([]byte(`{"alias":"qa","runTimeout":"nope"}`), &md)
	require.EqualError(t, err, "runTimeout must be a duration string (e.g. 1s, 30s) or integer nanoseconds")
}

func TestSLAConfigUnmarshalJSONDurationString(t *testing.T) {
	t.Parallel()

	var sla SLAConfig
	err := json.Unmarshal([]byte(`{"duration":"5m","completedBy":"06:00"}`), &sla)
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, sla.Duration)
	require.Equal(t, "06:00", sla.CompletedBy)
}

func TestSLAConfigUnmarshalJSONDurationNanoseconds(t *testing.T) {
	t.Parallel()

	var sla SLAConfig
	err := json.Unmarshal([]byte(`{"duration":300000000000}`), &sla)
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, sla.Duration)
}

func TestSLAConfigUnmarshalJSONInvalidDurationNamesField(t *testing.T) {
	t.Parallel()

	var sla SLAConfig
	err := json.Unmarshal([]byte(`{"duration":"tomorrow"}`), &sla)
	require.EqualError(t, err, "duration must be a duration string (e.g. 1s, 30s) or integer nanoseconds")
	require.NotContains(t, err.Error(), "cannot unmarshal")
}

func TestDefinitionJSONDurationRoundTrip(t *testing.T) {
	t.Parallel()

	orig := Definition{
		APIVersion: APIVersionV1,
		Kind:       KindJob,
		Metadata: Metadata{
			Alias:       "duration-round-trip",
			TaskTimeout: 30 * time.Second,
			RunTimeout:  time.Minute,
			SLA:         &SLAConfig{Duration: 5 * time.Minute, CompletedBy: "06:00"},
		},
		Trigger: Trigger{
			Type:          TriggerCron,
			Configuration: map[string]any{"cron": "0 0 1 1 *"},
		},
		Steps: []Step{{
			Name:       "extract",
			Image:      "alpine:3.23",
			RetryDelay: time.Second,
		}},
	}

	raw, err := json.Marshal(orig)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"1s"`)

	var decoded Definition
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Equal(t, orig.Metadata.TaskTimeout, decoded.Metadata.TaskTimeout)
	require.Equal(t, orig.Metadata.RunTimeout, decoded.Metadata.RunTimeout)
	require.NotNil(t, decoded.Metadata.SLA)
	require.Equal(t, orig.Metadata.SLA.Duration, decoded.Metadata.SLA.Duration)
	require.Equal(t, orig.Metadata.SLA.CompletedBy, decoded.Metadata.SLA.CompletedBy)
	require.Equal(t, orig.Steps[0].RetryDelay, decoded.Steps[0].RetryDelay)
}

func TestDefinitionJSONDurationStrings(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {
			"alias": "qa-duration-json",
			"taskTimeout": "30s",
			"runTimeout": "1m",
			"sla": {"duration": "5m"}
		},
		"trigger": {"type": "cron", "configuration": {"cron": "0 0 1 1 *"}},
		"steps": [{"name": "extract", "image": "alpine:3.23", "retries": 1, "retryDelay": "1s"}]
	}`)

	var def Definition
	require.NoError(t, json.Unmarshal(raw, &def))
	require.Equal(t, 30*time.Second, def.Metadata.TaskTimeout)
	require.Equal(t, time.Minute, def.Metadata.RunTimeout)
	require.NotNil(t, def.Metadata.SLA)
	require.Equal(t, 5*time.Minute, def.Metadata.SLA.Duration)
	require.Equal(t, time.Second, def.Steps[0].RetryDelay)
}

func TestParseYAMLDurationStringsStillWork(t *testing.T) {
	t.Parallel()

	src := `
apiVersion: v1
kind: Job
metadata:
  alias: qa-duration-yaml
  taskTimeout: 30s
  runTimeout: 1m
  sla:
    duration: 5m
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: extract
    image: alpine:3.23
    retries: 1
    retryDelay: 1s
`
	def, err := Parse([]byte(src))
	require.NoError(t, err)
	require.Equal(t, 30*time.Second, def.Metadata.TaskTimeout)
	require.Equal(t, time.Minute, def.Metadata.RunTimeout)
	require.NotNil(t, def.Metadata.SLA)
	require.Equal(t, 5*time.Minute, def.Metadata.SLA.Duration)
	require.Equal(t, time.Second, def.Steps[0].RetryDelay)
}

func TestDefinitionJSONInvalidDurationDoesNotLeakDecoderInternals(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"apiVersion": "v1",
		"kind": "Job",
		"metadata": {"alias": "qa-duration-bad"},
		"trigger": {"type": "cron", "configuration": {"cron": "0 0 1 1 *"}},
		"steps": [{"name": "extract", "image": "alpine:3.23", "retryDelay": "nope"}]
	}`)
	var def Definition
	err := json.Unmarshal(raw, &def)
	require.Error(t, err)
	require.Contains(t, err.Error(), "retryDelay")
	require.Contains(t, err.Error(), "duration string")
	require.NotContains(t, err.Error(), "cannot unmarshal")
	require.NotContains(t, err.Error(), "rawStep")
	require.False(t, strings.Contains(err.Error(), "Go struct field"))
}

package metrics

import (
	"testing"

	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDataAssertionsTotalRecordsEveryDisposition pins the bounded `result`
// label set the data circuit breaker emits. The values are the APPLIED
// disposition, not the declared one:
//
//	pass        — the dataset's declared contract held on this run
//	seeding     — a cold-start deltaFromBaseline verdict: recorded, never
//	              enforced
//	warn        — a violation recorded without failing the task
//	              (onViolation: warn)
//	hold        — a violation that opened or appended to a DatasetHold
//	              (onViolation: hold); the task still succeeds
//	fail        — a violation escalated into a red run (onViolation: fail)
//	unavailable — the metric could not be observed at all, because the marker
//	              stream was lost (unreadable log, or a truncated
//	              ##caesium::metrics scan). Warn-only by construction
//	              (issue #437)
//
// A dashboard groups by this label, so adding a value is an API change and has
// to fail here first.
func TestDataAssertionsTotalRecordsEveryDisposition(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(DataAssertionsTotal)

	for _, result := range []string{"pass", "seeding", "warn", "hold", "fail", "unavailable"} {
		before := metrictestutil.CounterValue(t, DataAssertionsTotal, result)
		DataAssertionsTotal.WithLabelValues(result).Inc()
		after := metrictestutil.CounterValue(t, DataAssertionsTotal, result)
		assert.InDelta(t, before+1, after, 0.0001, "result=%s must increment", result)
	}

	families, err := registry.Gather()
	require.NoError(t, err)

	var found *string
	labels := map[string]bool{}
	for _, fam := range families {
		if fam.GetName() != "caesium_data_assertions_total" {
			continue
		}
		name := fam.GetName()
		found = &name
		for _, metric := range fam.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == "result" {
					labels[pair.GetValue()] = true
				}
			}
		}
	}
	require.NotNil(t, found, "caesium_data_assertions_total must be gatherable")
	for _, result := range []string{"pass", "seeding", "warn", "hold", "fail", "unavailable"} {
		assert.True(t, labels[result], "result=%s must be a gatherable label value", result)
	}
}

// TestDataAssertionsTotalIsRegisteredWithTheDefaultRegistry proves the counter
// is actually wired into Register() — a metric defined in the var block but
// never registered is invisible on /metrics, which is how a "shipped" metric
// silently never appears.
func TestDataAssertionsTotalIsRegisteredWithTheDefaultRegistry(t *testing.T) {
	Register()
	DataAssertionsTotal.WithLabelValues("pass").Inc()

	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)

	for _, fam := range families {
		if fam.GetName() == "caesium_data_assertions_total" {
			return
		}
	}
	t.Fatal("caesium_data_assertions_total must be registered by metrics.Register()")
}

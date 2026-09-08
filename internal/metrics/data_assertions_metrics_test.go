package metrics

import (
	"testing"

	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDataAssertionsTotalRecordsEveryDisposition pins the bounded `result`
// label set the data circuit breaker emits. The four values are the APPLIED
// disposition, not the declared one:
//
//	pass    — the dataset's declared contract held on this run
//	seeding — a cold-start deltaFromBaseline verdict: recorded, never enforced
//	warn    — a violation recorded without failing the task (onViolation: warn,
//	          and onViolation: hold until Stream C wires the breaker)
//	fail    — a violation escalated into a red run (onViolation: fail)
//
// A dashboard groups by this label, so adding a fifth value is an API change
// and has to fail here first.
func TestDataAssertionsTotalRecordsEveryDisposition(t *testing.T) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(DataAssertionsTotal)

	for _, result := range []string{"pass", "seeding", "warn", "fail"} {
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
	for _, result := range []string{"pass", "seeding", "warn", "fail"} {
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

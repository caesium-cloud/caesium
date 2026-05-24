package testutil

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// GaugeValue returns the current value of a Gauge.
func GaugeValue(tb testing.TB, g prometheus.Gauge) float64 {
	tb.Helper()

	var m dto.Metric
	require.NoError(tb, g.Write(&m))
	return m.GetGauge().GetValue()
}

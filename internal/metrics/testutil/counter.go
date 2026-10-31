package testutil

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// CounterValue returns the current value for a CounterVec label set.
func CounterValue(tb testing.TB, vec *prometheus.CounterVec, labels ...string) float64 {
	tb.Helper()

	var m dto.Metric
	counter, err := vec.GetMetricWithLabelValues(labels...)
	require.NoError(tb, err)
	require.NoError(tb, counter.(prometheus.Metric).Write(&m))
	return m.GetCounter().GetValue()
}

// CounterTotal returns the current value of a Counter with no labels.
func CounterTotal(tb testing.TB, counter prometheus.Counter) float64 {
	tb.Helper()

	var m dto.Metric
	require.NoError(tb, counter.Write(&m))
	return m.GetCounter().GetValue()
}

package testutil

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

// HistogramSampleCount returns the number of observations recorded on a
// HistogramVec for the given label set.  Asserting the count (rather than a
// specific duration) is what makes a wall-clock metric testable without a
// stubbed clock.
func HistogramSampleCount(tb testing.TB, vec *prometheus.HistogramVec, labels ...string) uint64 {
	tb.Helper()

	observer, err := vec.GetMetricWithLabelValues(labels...)
	require.NoError(tb, err)
	metric, ok := observer.(prometheus.Metric)
	require.True(tb, ok, "histogram observer must implement prometheus.Metric")

	var m dto.Metric
	require.NoError(tb, metric.Write(&m))
	return m.GetHistogram().GetSampleCount()
}

package lineage

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	registerMetricsOnce sync.Once

	LineageEventsEmitted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_lineage_events_emitted_total",
			Help: "Total number of OpenLineage events emitted by type and status.",
		},
		[]string{"event_type", "status"},
	)

	LineageEmitDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caesium_lineage_emit_duration_seconds",
			Help:    "Duration of OpenLineage event emission in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
		},
		[]string{"transport"},
	)
)

func RegisterMetrics() {
	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(
			LineageEventsEmitted,
			LineageEmitDuration,
		)
	})
}

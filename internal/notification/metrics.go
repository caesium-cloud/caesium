package notification

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	registerMetricsOnce sync.Once

	// NotificationSendsTotal counts notification dispatch attempts by channel type and outcome.
	NotificationSendsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_notification_sends_total",
			Help: "Total notification dispatch attempts by channel type and outcome.",
		},
		[]string{"channel_type", "status"},
	)

	// NotificationSendDuration tracks how long each send takes.
	NotificationSendDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "caesium_notification_send_duration_seconds",
			Help:    "Duration of notification send operations in seconds.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
		},
		[]string{"channel_type"},
	)

	// TaskFailuresTotal counts task failure events observed by the notification subscriber.
	TaskFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_task_failures_total",
			Help: "Total task failure events observed.",
		},
		[]string{"job_alias"},
	)

	// RunFailuresTotal counts run failure events observed by the notification subscriber.
	RunFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_run_failures_total",
			Help: "Total run failure events observed.",
		},
		[]string{"job_alias"},
	)

	// RunTimeoutsTotal counts run timeout events.
	RunTimeoutsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_run_timeouts_total",
			Help: "Total run timeout events observed.",
		},
		[]string{"job_alias"},
	)

	// SLAMissesTotal counts SLA miss events.
	SLAMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "caesium_sla_misses_total",
			Help: "Total SLA miss events observed.",
		},
		[]string{"job_alias"},
	)
)

// RegisterMetrics registers all notification metrics with the default Prometheus registry.
func RegisterMetrics() {
	registerMetricsOnce.Do(func() {
		prometheus.MustRegister(
			NotificationSendsTotal,
			NotificationSendDuration,
			TaskFailuresTotal,
			RunFailuresTotal,
			RunTimeoutsTotal,
			SLAMissesTotal,
		)
	})
}

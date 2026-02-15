package metrics

import (
	"testing"

	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/suite"
)

type MetricsSuite struct {
	suite.Suite
	registry *prometheus.Registry
}

func TestMetricsSuite(t *testing.T) {
	suite.Run(t, new(MetricsSuite))
}

func (s *MetricsSuite) SetupTest() {
	s.registry = prometheus.NewRegistry()
	s.registry.MustRegister(
		JobRunsTotal,
		JobRunDurationSeconds,
		TaskRunsTotal,
		TaskRunDurationSeconds,
		JobsActive,
		CallbackRunsTotal,
		TriggerFiresTotal,
		WorkerClaimsTotal,
		WorkerClaimContentionTotal,
		WorkerLeaseExpirationsTotal,
	)
}

func (s *MetricsSuite) TestJobRunsTotalIncrements() {
	JobRunsTotal.WithLabelValues("job-1", "succeeded").Inc()
	JobRunsTotal.WithLabelValues("job-1", "failed").Inc()
	JobRunsTotal.WithLabelValues("job-1", "failed").Inc()

	val := metrictestutil.CounterValue(s.T(), JobRunsTotal, "job-1", "succeeded")
	s.GreaterOrEqual(val, float64(1))

	val = metrictestutil.CounterValue(s.T(), JobRunsTotal, "job-1", "failed")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestJobRunDurationObserves() {
	JobRunDurationSeconds.WithLabelValues("job-1", "succeeded").Observe(42.5)

	families, err := s.registry.Gather()
	s.Require().NoError(err)

	found := false
	for _, fam := range families {
		if fam.GetName() == "caesium_job_run_duration_seconds" {
			for _, m := range fam.GetMetric() {
				h := m.GetHistogram()
				if h != nil && h.GetSampleCount() > 0 {
					found = true
					s.Equal(uint64(1), h.GetSampleCount())
					s.Equal(42.5, h.GetSampleSum())
				}
			}
		}
	}
	s.True(found, "expected histogram sample")
}

func (s *MetricsSuite) TestTaskRunsTotalIncrements() {
	TaskRunsTotal.WithLabelValues("job-1", "task-1", "docker", "succeeded").Inc()

	val := metrictestutil.CounterValue(s.T(), TaskRunsTotal, "job-1", "task-1", "docker", "succeeded")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) TestTaskRunDurationObserves() {
	TaskRunDurationSeconds.WithLabelValues("job-1", "docker", "failed").Observe(10.0)

	families, err := s.registry.Gather()
	s.Require().NoError(err)

	found := false
	for _, fam := range families {
		if fam.GetName() == "caesium_task_run_duration_seconds" {
			for _, m := range fam.GetMetric() {
				h := m.GetHistogram()
				if h != nil && h.GetSampleCount() > 0 {
					found = true
				}
			}
		}
	}
	s.True(found, "expected task histogram sample")
}

func (s *MetricsSuite) TestJobsActiveGauge() {
	JobsActive.WithLabelValues("job-1").Inc()
	JobsActive.WithLabelValues("job-1").Inc()
	JobsActive.WithLabelValues("job-1").Dec()

	val := s.gaugeValue(JobsActive, "job-1")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) TestCallbackRunsTotalIncrements() {
	CallbackRunsTotal.WithLabelValues("job-1", "succeeded").Inc()

	val := metrictestutil.CounterValue(s.T(), CallbackRunsTotal, "job-1", "succeeded")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) TestTriggerFiresTotalIncrements() {
	TriggerFiresTotal.WithLabelValues("job-1", "cron").Inc()

	val := metrictestutil.CounterValue(s.T(), TriggerFiresTotal, "job-1", "cron")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) TestWorkerClaimsTotalIncrements() {
	WorkerClaimsTotal.WithLabelValues("node-a").Inc()

	val := metrictestutil.CounterValue(s.T(), WorkerClaimsTotal, "node-a")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) TestWorkerClaimContentionTotalIncrements() {
	WorkerClaimContentionTotal.WithLabelValues("node-a").Inc()
	WorkerClaimContentionTotal.WithLabelValues("node-a").Inc()

	val := metrictestutil.CounterValue(s.T(), WorkerClaimContentionTotal, "node-a")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestWorkerLeaseExpirationsTotalAdds() {
	WorkerLeaseExpirationsTotal.WithLabelValues("node-a").Add(3)

	val := metrictestutil.CounterValue(s.T(), WorkerLeaseExpirationsTotal, "node-a")
	s.GreaterOrEqual(val, float64(3))
}

func (s *MetricsSuite) gaugeValue(vec *prometheus.GaugeVec, labels ...string) float64 {
	var m dto.Metric
	gauge, err := vec.GetMetricWithLabelValues(labels...)
	s.Require().NoError(err)
	s.Require().NoError(gauge.(prometheus.Metric).Write(&m))
	return m.GetGauge().GetValue()
}

package metrics

import (
	"testing"

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
	)
}

func (s *MetricsSuite) TestJobRunsTotalIncrements() {
	JobRunsTotal.WithLabelValues("job-1", "succeeded").Inc()
	JobRunsTotal.WithLabelValues("job-1", "failed").Inc()
	JobRunsTotal.WithLabelValues("job-1", "failed").Inc()

	val := s.counterValue(JobRunsTotal, "job-1", "succeeded")
	s.GreaterOrEqual(val, float64(1))

	val = s.counterValue(JobRunsTotal, "job-1", "failed")
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

	val := s.counterValue(TaskRunsTotal, "job-1", "task-1", "docker", "succeeded")
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

	val := s.counterValue(CallbackRunsTotal, "job-1", "succeeded")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) TestTriggerFiresTotalIncrements() {
	TriggerFiresTotal.WithLabelValues("job-1", "cron").Inc()

	val := s.counterValue(TriggerFiresTotal, "job-1", "cron")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) counterValue(vec *prometheus.CounterVec, labels ...string) float64 {
	var m dto.Metric
	counter, err := vec.GetMetricWithLabelValues(labels...)
	s.Require().NoError(err)
	s.Require().NoError(counter.(prometheus.Metric).Write(&m))
	return m.GetCounter().GetValue()
}

func (s *MetricsSuite) gaugeValue(vec *prometheus.GaugeVec, labels ...string) float64 {
	var m dto.Metric
	gauge, err := vec.GetMetricWithLabelValues(labels...)
	s.Require().NoError(err)
	s.Require().NoError(gauge.(prometheus.Metric).Write(&m))
	return m.GetGauge().GetValue()
}

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
		TaskRegisterBatchSize,
		JobsActive,
		CallbackRunsTotal,
		TriggerFiresTotal,
		WorkerClaimsTotal,
		WorkerClaimContentionTotal,
		DBBusyRetriesTotal,
		ReclaimDurationSeconds,
		WorkerLeaseExpirationsTotal,
		TaskRetriesTotal,
		WebhookAuthFailuresTotal,
		EventBusDroppedTotal,
		TriggerChainDepth,
		TriggerChainRejectedTotal,
		EventTriggerMatchesTotal,
		EventsIngestedTotal,
		EventBridgeFailuresTotal,
		ContractFindingsTotal,
		SSOLoginsTotal,
		SSOLoginDurationSeconds,
		SSOLogoutsTotal,
		ContractBreaksBlockedTotal,
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

func (s *MetricsSuite) TestTaskRegisterBatchSizeObserves() {
	var before dto.Metric
	s.Require().NoError(TaskRegisterBatchSize.Write(&before))

	TaskRegisterBatchSize.Observe(12)

	var after dto.Metric
	s.Require().NoError(TaskRegisterBatchSize.Write(&after))
	s.Equal(before.GetHistogram().GetSampleCount()+1, after.GetHistogram().GetSampleCount())
	s.InDelta(before.GetHistogram().GetSampleSum()+12, after.GetHistogram().GetSampleSum(), 0.000001)
}

func (s *MetricsSuite) TestTriggerChainMetricsObserveAndIncrement() {
	beforeDrop := metrictestutil.CounterValue(s.T(), EventBusDroppedTotal, "run_completed")
	EventBusDroppedTotal.WithLabelValues("run_completed").Inc()
	s.GreaterOrEqual(metrictestutil.CounterValue(s.T(), EventBusDroppedTotal, "run_completed"), beforeDrop+1)

	var beforeDepth dto.Metric
	s.Require().NoError(TriggerChainDepth.Write(&beforeDepth))
	TriggerChainDepth.Observe(3)
	var afterDepth dto.Metric
	s.Require().NoError(TriggerChainDepth.Write(&afterDepth))
	s.Equal(beforeDepth.GetHistogram().GetSampleCount()+1, afterDepth.GetHistogram().GetSampleCount())

	var beforeRejected dto.Metric
	s.Require().NoError(TriggerChainRejectedTotal.Write(&beforeRejected))
	TriggerChainRejectedTotal.Inc()
	var afterRejected dto.Metric
	s.Require().NoError(TriggerChainRejectedTotal.Write(&afterRejected))
	s.GreaterOrEqual(afterRejected.GetCounter().GetValue(), beforeRejected.GetCounter().GetValue()+1)
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

func (s *MetricsSuite) TestDBBusyRetriesTotalIncrements() {
	DBBusyRetriesTotal.Inc()
	DBBusyRetriesTotal.Inc()

	var m dto.Metric
	s.Require().NoError(DBBusyRetriesTotal.Write(&m))
	s.GreaterOrEqual(m.GetCounter().GetValue(), float64(2))
}

func (s *MetricsSuite) TestReclaimDurationSecondsObserves() {
	ReclaimDurationSeconds.Observe(0.125)

	families, err := s.registry.Gather()
	s.Require().NoError(err)

	found := false
	for _, fam := range families {
		if fam.GetName() == "caesium_reclaim_duration_seconds" {
			for _, m := range fam.GetMetric() {
				h := m.GetHistogram()
				if h != nil && h.GetSampleCount() > 0 {
					found = true
				}
			}
		}
	}
	s.True(found, "expected reclaim duration histogram sample")
}

func (s *MetricsSuite) TestWorkerLeaseExpirationsTotalAdds() {
	WorkerLeaseExpirationsTotal.WithLabelValues("node-a").Add(3)

	val := metrictestutil.CounterValue(s.T(), WorkerLeaseExpirationsTotal, "node-a")
	s.GreaterOrEqual(val, float64(3))
}

func (s *MetricsSuite) TestWebhookAuthFailuresTotalIncrements() {
	WebhookAuthFailuresTotal.WithLabelValues("github/push", "invalid_signature").Inc()
	WebhookAuthFailuresTotal.WithLabelValues("github/push", "replayed_request").Inc()
	WebhookAuthFailuresTotal.WithLabelValues("github/push", "replayed_request").Inc()

	val := metrictestutil.CounterValue(s.T(), WebhookAuthFailuresTotal, "github/push", "invalid_signature")
	s.GreaterOrEqual(val, float64(1))

	val = metrictestutil.CounterValue(s.T(), WebhookAuthFailuresTotal, "github/push", "replayed_request")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestEventsIngestedTotalIncrements() {
	EventsIngestedTotal.WithLabelValues("ingest").Inc()
	EventsIngestedTotal.WithLabelValues("ingest").Inc()

	val := metrictestutil.CounterValue(s.T(), EventsIngestedTotal, "ingest")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestEventTriggerMatchesTotalIncrements() {
	EventTriggerMatchesTotal.WithLabelValues("trigger-1").Inc()
	EventTriggerMatchesTotal.WithLabelValues("trigger-1").Inc()

	val := metrictestutil.CounterValue(s.T(), EventTriggerMatchesTotal, "trigger-1")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestEventBridgeFailuresTotalIncrements() {
	EventBridgeFailuresTotal.WithLabelValues("webhook").Inc()

	val := metrictestutil.CounterValue(s.T(), EventBridgeFailuresTotal, "webhook")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) TestContractFindingsTotalIncrements() {
	ContractFindingsTotal.WithLabelValues("breaking").Inc()
	ContractFindingsTotal.WithLabelValues("unknown").Inc()
	ContractFindingsTotal.WithLabelValues("unknown").Inc()

	val := metrictestutil.CounterValue(s.T(), ContractFindingsTotal, "breaking")
	s.GreaterOrEqual(val, float64(1))

	val = metrictestutil.CounterValue(s.T(), ContractFindingsTotal, "unknown")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestSSOLoginsTotalIncrements() {
	SSOLoginsTotal.WithLabelValues("oidc", "success").Inc()
	SSOLoginsTotal.WithLabelValues("oidc", "denied").Inc()
	SSOLoginsTotal.WithLabelValues("oidc", "denied").Inc()

	val := metrictestutil.CounterValue(s.T(), SSOLoginsTotal, "oidc", "success")
	s.GreaterOrEqual(val, float64(1))

	val = metrictestutil.CounterValue(s.T(), SSOLoginsTotal, "oidc", "denied")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestSSOLoginDurationObserves() {
	SSOLoginDurationSeconds.WithLabelValues("saml").Observe(0.25)

	families, err := s.registry.Gather()
	s.Require().NoError(err)

	found := false
	for _, fam := range families {
		if fam.GetName() == "caesium_sso_login_duration_seconds" {
			for _, m := range fam.GetMetric() {
				h := m.GetHistogram()
				if h != nil && h.GetSampleCount() > 0 {
					found = true
				}
			}
		}
	}
	s.True(found, "expected SSO login duration histogram sample")
}

func (s *MetricsSuite) TestSSOLogoutsTotalIncrements() {
	SSOLogoutsTotal.WithLabelValues("success").Inc()
	SSOLogoutsTotal.WithLabelValues("error").Inc()
	SSOLogoutsTotal.WithLabelValues("error").Inc()

	val := metrictestutil.CounterValue(s.T(), SSOLogoutsTotal, "success")
	s.GreaterOrEqual(val, float64(1))

	val = metrictestutil.CounterValue(s.T(), SSOLogoutsTotal, "error")
	s.GreaterOrEqual(val, float64(2))
}

func (s *MetricsSuite) TestContractBreaksBlockedTotalIncrements() {
	ContractBreaksBlockedTotal.WithLabelValues("producer.output.customer_id").Inc()

	val := metrictestutil.CounterValue(s.T(), ContractBreaksBlockedTotal, "producer.output.customer_id")
	s.GreaterOrEqual(val, float64(1))
}

func (s *MetricsSuite) gaugeValue(vec *prometheus.GaugeVec, labels ...string) float64 {
	var m dto.Metric
	gauge, err := vec.GetMetricWithLabelValues(labels...)
	s.Require().NoError(err)
	s.Require().NoError(gauge.(prometheus.Metric).Write(&m))
	return m.GetGauge().GetValue()
}

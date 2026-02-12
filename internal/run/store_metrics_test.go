package run

import (
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type StoreMetricsSuite struct {
	suite.Suite
	db    *gorm.DB
	store *Store
}

func TestStoreMetricsSuite(t *testing.T) {
	suite.Run(t, new(StoreMetricsSuite))
}

func (s *StoreMetricsSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
	s.store = NewStore(db)

	metrics.JobRunsTotal.Reset()
	metrics.JobRunDurationSeconds.Reset()
	metrics.TaskRunsTotal.Reset()
	metrics.TaskRunDurationSeconds.Reset()
	metrics.JobsActive.Reset()
	metrics.CallbackRunsTotal.Reset()
	metrics.TriggerFiresTotal.Reset()
}

func (s *StoreMetricsSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *StoreMetricsSuite) TestStartIncrementsActiveGauge() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID)
	s.Require().NoError(err)
	s.Require().NotNil(run)

	val := s.gaugeValue(metrics.JobsActive, jobID.String())
	s.Equal(float64(1), val)
}

func (s *StoreMetricsSuite) TestCompleteRecordsCounterAndDuration() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID)
	s.Require().NoError(err)

	// Simulate some passage of time by updating started_at in the past.
	past := time.Now().UTC().Add(-10 * time.Second)
	s.db.Model(&models.JobRun{}).Where("id = ?", run.ID).Update("started_at", past)

	err = s.store.Complete(run.ID, nil)
	s.Require().NoError(err)

	// Active gauge should be back to zero.
	val := s.gaugeValue(metrics.JobsActive, jobID.String())
	s.Equal(float64(0), val)

	// Counter should have one succeeded.
	ctr := s.counterValue(metrics.JobRunsTotal, jobID.String(), "succeeded")
	s.Equal(float64(1), ctr)

	// Duration histogram should have one observation.
	count, sum := s.histogramValues(metrics.JobRunDurationSeconds, jobID.String(), "succeeded")
	s.Equal(uint64(1), count)
	s.GreaterOrEqual(sum, float64(9)) // at least ~10s minus small timing variance
}

func (s *StoreMetricsSuite) TestCompleteWithErrorRecordsFailure() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID)
	s.Require().NoError(err)

	err = s.store.Complete(run.ID, fmt.Errorf("boom"))
	s.Require().NoError(err)

	ctr := s.counterValue(metrics.JobRunsTotal, jobID.String(), "failed")
	s.Equal(float64(1), ctr)
}

func (s *StoreMetricsSuite) TestCompleteTaskRecordsMetrics() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID)
	s.Require().NoError(err)

	taskID := uuid.New()
	atomID := uuid.New()
	task := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID}
	atom := &models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox"}
	s.Require().NoError(s.db.Create(task).Error)
	s.Require().NoError(s.db.Create(atom).Error)
	s.Require().NoError(s.store.RegisterTask(run.ID, task, atom, 0))
	s.Require().NoError(s.store.StartTask(run.ID, taskID, "container-1"))

	// Simulate started_at in the past.
	s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", run.ID, taskID).
		Update("started_at", time.Now().UTC().Add(-5*time.Second))

	err = s.store.CompleteTask(run.ID, taskID, "ok")
	s.Require().NoError(err)

	ctr := s.counterValue(metrics.TaskRunsTotal, jobID.String(), taskID.String(), "docker", "succeeded")
	s.Equal(float64(1), ctr)

	count, sum := s.histogramValues(metrics.TaskRunDurationSeconds, jobID.String(), "docker", "succeeded")
	s.Equal(uint64(1), count)
	s.GreaterOrEqual(sum, float64(4))
}

func (s *StoreMetricsSuite) TestFailTaskRecordsMetrics() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID)
	s.Require().NoError(err)

	taskID := uuid.New()
	atomID := uuid.New()
	task := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID}
	atom := &models.Atom{ID: atomID, Engine: models.AtomEnginePodman, Image: "alpine"}
	s.Require().NoError(s.db.Create(task).Error)
	s.Require().NoError(s.db.Create(atom).Error)
	s.Require().NoError(s.store.RegisterTask(run.ID, task, atom, 0))
	s.Require().NoError(s.store.StartTask(run.ID, taskID, "pod-1"))

	s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", run.ID, taskID).
		Update("started_at", time.Now().UTC().Add(-3*time.Second))

	err = s.store.FailTask(run.ID, taskID, fmt.Errorf("crash"))
	s.Require().NoError(err)

	ctr := s.counterValue(metrics.TaskRunsTotal, jobID.String(), taskID.String(), "podman", "failed")
	s.Equal(float64(1), ctr)

	count, _ := s.histogramValues(metrics.TaskRunDurationSeconds, jobID.String(), "podman", "failed")
	s.Equal(uint64(1), count)
}

func (s *StoreMetricsSuite) TestCompleteResumedRunDoesNotDecrementGauge() {
	jobID := uuid.New()

	// Simulate a run that was started by a previous process: insert directly
	// into the DB without going through Start().
	runID := uuid.New()
	s.Require().NoError(s.db.Create(&models.JobRun{
		ID:        runID,
		JobID:     jobID,
		Status:    string(StatusRunning),
		StartedAt: time.Now().UTC().Add(-30 * time.Second),
	}).Error)

	// Gauge should be at zero since Start() was never called.
	val := s.gaugeValue(metrics.JobsActive, jobID.String())
	s.Equal(float64(0), val)

	// Complete the resumed run.
	err := s.store.Complete(runID, nil)
	s.Require().NoError(err)

	// Gauge must remain at zero, not go negative.
	val = s.gaugeValue(metrics.JobsActive, jobID.String())
	s.Equal(float64(0), val)

	// Counter should still record the completion.
	ctr := s.counterValue(metrics.JobRunsTotal, jobID.String(), "succeeded")
	s.Equal(float64(1), ctr)
}

func (s *StoreMetricsSuite) counterValue(vec *prometheus.CounterVec, labels ...string) float64 {
	var m dto.Metric
	counter, err := vec.GetMetricWithLabelValues(labels...)
	s.Require().NoError(err)
	s.Require().NoError(counter.(prometheus.Metric).Write(&m))
	return m.GetCounter().GetValue()
}

func (s *StoreMetricsSuite) gaugeValue(vec *prometheus.GaugeVec, labels ...string) float64 {
	var m dto.Metric
	gauge, err := vec.GetMetricWithLabelValues(labels...)
	s.Require().NoError(err)
	s.Require().NoError(gauge.(prometheus.Metric).Write(&m))
	return m.GetGauge().GetValue()
}

func (s *StoreMetricsSuite) histogramValues(vec *prometheus.HistogramVec, labels ...string) (uint64, float64) {
	var m dto.Metric
	observer, err := vec.GetMetricWithLabelValues(labels...)
	s.Require().NoError(err)
	s.Require().NoError(observer.(prometheus.Metric).Write(&m))
	h := m.GetHistogram()
	return h.GetSampleCount(), h.GetSampleSum()
}

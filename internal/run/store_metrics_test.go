package run

import (
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
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
	metrics.DBWritesTotal.Reset()
	metrics.DBStatementsTotal.Reset()
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
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)
	s.Require().NotNil(run)

	val := s.gaugeValue(metrics.JobsActive, jobID.String())
	s.Equal(float64(1), val)
}

func (s *StoreMetricsSuite) TestCompleteRecordsCounterAndDuration() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
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
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	err = s.store.Complete(run.ID, fmt.Errorf("boom"))
	s.Require().NoError(err)

	ctr := s.counterValue(metrics.JobRunsTotal, jobID.String(), "failed")
	s.Equal(float64(1), ctr)
}

func (s *StoreMetricsSuite) TestRegisterTasksObservesBatchSize() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	atomID := uuid.New()
	taskA := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID}
	taskB := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID}
	atom := &models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	s.Require().NoError(s.db.Create([]*models.Task{taskA, taskB}).Error)
	s.Require().NoError(s.db.Create(atom).Error)

	beforeCount, beforeSum := s.histogramValue(metrics.TaskRegisterBatchSize)
	s.Require().NoError(s.store.RegisterTasks(run.ID, []RegisterTaskInput{
		{Task: taskA, Atom: atom, OutstandingPredecessors: 0},
		{Task: taskB, Atom: atom, OutstandingPredecessors: 1},
	}))

	afterCount, afterSum := s.histogramValue(metrics.TaskRegisterBatchSize)
	s.Equal(beforeCount+1, afterCount)
	s.InDelta(beforeSum+2, afterSum, 0.000001)
}

func (s *StoreMetricsSuite) TestRegisterTasksEmptyInputObservesZeroBatchSize() {
	beforeCount, beforeSum := s.histogramValue(metrics.TaskRegisterBatchSize)
	s.Require().NoError(s.store.RegisterTasks(uuid.New(), nil))

	afterCount, afterSum := s.histogramValue(metrics.TaskRegisterBatchSize)
	s.Equal(beforeCount+1, afterCount)
	s.Equal(beforeSum, afterSum)
}

func (s *StoreMetricsSuite) TestCompleteTaskRecordsMetrics() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	taskID := uuid.New()
	atomID := uuid.New()
	task := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID}
	atom := &models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	s.Require().NoError(s.db.Create(task).Error)
	s.Require().NoError(s.db.Create(atom).Error)
	s.Require().NoError(s.store.RegisterTask(run.ID, task, atom, 0))
	s.Require().NoError(s.store.StartTask(run.ID, taskID, "container-1"))

	// Simulate started_at in the past.
	s.db.Model(&models.TaskRun{}).
		Where("job_run_id = ? AND task_id = ?", run.ID, taskID).
		Update("started_at", time.Now().UTC().Add(-5*time.Second))

	err = s.store.CompleteTask(run.ID, taskID, "ok", nil, nil)
	s.Require().NoError(err)

	ctr := s.counterValue(metrics.TaskRunsTotal, jobID.String(), taskID.String(), "docker", "succeeded")
	s.Equal(float64(1), ctr)

	count, sum := s.histogramValues(metrics.TaskRunDurationSeconds, jobID.String(), "docker", "succeeded")
	s.Equal(uint64(1), count)
	s.GreaterOrEqual(sum, float64(4))
}

func (s *StoreMetricsSuite) TestFailTaskRecordsMetrics() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	taskID := uuid.New()
	atomID := uuid.New()
	task := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID}
	atom := &models.Atom{ID: atomID, Engine: models.AtomEnginePodman, Image: "alpine:3.23"}
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

func (s *StoreMetricsSuite) TestDBWritesTotalIncrements() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	atomID := uuid.New()
	taskID := uuid.New()
	task := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID}
	atom := &models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	s.Require().NoError(s.db.Create(task).Error)
	s.Require().NoError(s.db.Create(atom).Error)

	// RegisterTasks: should produce task_run_insert = 1.
	s.Require().NoError(s.store.RegisterTask(run.ID, task, atom, 0))
	s.Equal(float64(1), s.dbWriteValue(metrics.DBWriteCategoryTaskRunInsert))

	// StartTask: should produce task_run_status = 1.
	s.Require().NoError(s.store.StartTask(run.ID, taskID, "container-1"))
	s.Equal(float64(1), s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus))

	// CompleteTask: should produce task_run_status += 1.
	s.Require().NoError(s.store.CompleteTask(run.ID, taskID, "ok", nil, nil))
	s.GreaterOrEqual(s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus), float64(2))

	// event_insert should be non-zero from task_ready + task_started + task_succeeded events.
	s.Greater(s.dbWriteValue(metrics.DBWriteCategoryEventInsert), float64(0))
}

func (s *StoreMetricsSuite) TestDBWritesTotalFailTask() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	atomID := uuid.New()
	taskID := uuid.New()
	task := &models.Task{ID: taskID, JobID: jobID, AtomID: atomID}
	atom := &models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	s.Require().NoError(s.db.Create(task).Error)
	s.Require().NoError(s.db.Create(atom).Error)
	s.Require().NoError(s.store.RegisterTask(run.ID, task, atom, 0))
	s.Require().NoError(s.store.StartTask(run.ID, taskID, "container-1"))

	beforeStatus := s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus)
	s.Require().NoError(s.store.FailTask(run.ID, taskID, fmt.Errorf("crash")))
	afterStatus := s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus)
	s.Equal(float64(1), afterStatus-beforeStatus, "FailTask should produce exactly one task_run_status write")
}

// TestFanOutCompletionWriteCounts asserts that completing a root task with
// fan-out=4 successors produces the correct row counts in the DB write metrics:
// exactly 4 task_run_status writes (1 batched UPDATE touching 4 rows, counted
// as 4) and at least 1 event_insert write (1 batched INSERT for
// task_succeeded + 4×task_ready = 5 rows, counted as 5).
func (s *StoreMetricsSuite) TestFanOutCompletionWriteCounts() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	atomID := uuid.New()
	atom := &models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	s.Require().NoError(s.db.Create(atom).Error)

	root := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "root"}
	lane1 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "lane1"}
	lane2 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "lane2"}
	lane3 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "lane3"}
	lane4 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "lane4"}
	s.Require().NoError(s.db.Create([]*models.Task{root, lane1, lane2, lane3, lane4}).Error)

	now := time.Now().UTC()
	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane1.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane2.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane3.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane4.ID, CreatedAt: now, UpdatedAt: now},
	}
	s.Require().NoError(s.db.Create(&edges).Error)

	s.Require().NoError(s.store.RegisterTask(run.ID, root, atom, 0))
	s.Require().NoError(s.store.RegisterTask(run.ID, lane1, atom, 1))
	s.Require().NoError(s.store.RegisterTask(run.ID, lane2, atom, 1))
	s.Require().NoError(s.store.RegisterTask(run.ID, lane3, atom, 1))
	s.Require().NoError(s.store.RegisterTask(run.ID, lane4, atom, 1))
	s.Require().NoError(s.store.StartTask(run.ID, root.ID, "container-root"))

	beforeStatusRows := s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus)
	beforeEventRows := s.dbWriteValue(metrics.DBWriteCategoryEventInsert)
	beforeStatusStmts := s.dbStatementValue(metrics.DBWriteCategoryTaskRunStatus)
	beforeEventStmts := s.dbStatementValue(metrics.DBWriteCategoryEventInsert)

	s.Require().NoError(s.store.CompleteTask(run.ID, root.ID, "ok", nil, nil))

	afterStatusRows := s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus)
	afterEventRows := s.dbWriteValue(metrics.DBWriteCategoryEventInsert)
	afterStatusStmts := s.dbStatementValue(metrics.DBWriteCategoryTaskRunStatus)
	afterEventStmts := s.dbStatementValue(metrics.DBWriteCategoryEventInsert)

	// 1 status row for the task itself + 4 for the predecessor decrements = 5 rows total,
	// produced by 2 statements (1 for the completed task UPDATE + 1 batched UPDATE
	// for the 4 predecessors). Batching factor: 5/2 = 2.5.
	s.Equal(float64(5), afterStatusRows-beforeStatusRows,
		"fan-out=4 completion: expected 5 task_run_status rows (1 complete + 4 predecessors)")
	s.Equal(float64(2), afterStatusStmts-beforeStatusStmts,
		"fan-out=4 completion: expected 2 task_run_status statements (1 complete UPDATE + 1 batched predecessor UPDATE)")

	// task_succeeded (1) + task_ready×4 = 5 event rows in one batched INSERT.
	// Batching factor: 5/1 = 5.0 — the headline win from Phase 1.1.
	s.Equal(float64(5), afterEventRows-beforeEventRows,
		"fan-out=4 completion: expected 5 event rows (1 task_succeeded + 4 task_ready)")
	s.Equal(float64(1), afterEventStmts-beforeEventStmts,
		"fan-out=4 completion: expected 1 event_insert statement (5 events in one batched INSERT)")
}

// TestCacheHitCompletionWriteCounts verifies that CacheHitTask with fan-out=2
// also produces batched write counts.
func (s *StoreMetricsSuite) TestCacheHitCompletionWriteCounts() {
	jobID := uuid.New()
	run, err := s.store.Start(jobID, nil)
	s.Require().NoError(err)

	atomID := uuid.New()
	atom := &models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	s.Require().NoError(s.db.Create(atom).Error)

	root := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "root"}
	lane1 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "lane1"}
	lane2 := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: "lane2"}
	s.Require().NoError(s.db.Create([]*models.Task{root, lane1, lane2}).Error)

	now := time.Now().UTC()
	edges := []models.TaskEdge{
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane1.ID, CreatedAt: now, UpdatedAt: now},
		{ID: uuid.New(), JobID: jobID, FromTaskID: root.ID, ToTaskID: lane2.ID, CreatedAt: now, UpdatedAt: now},
	}
	s.Require().NoError(s.db.Create(&edges).Error)

	s.Require().NoError(s.store.RegisterTask(run.ID, root, atom, 0))
	s.Require().NoError(s.store.RegisterTask(run.ID, lane1, atom, 1))
	s.Require().NoError(s.store.RegisterTask(run.ID, lane2, atom, 1))

	beforeStatusRows := s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus)
	beforeEventRows := s.dbWriteValue(metrics.DBWriteCategoryEventInsert)
	beforeStatusStmts := s.dbStatementValue(metrics.DBWriteCategoryTaskRunStatus)
	beforeEventStmts := s.dbStatementValue(metrics.DBWriteCategoryEventInsert)

	_, err = s.store.CacheHitTask(run.ID, root.ID, CacheHitSource{
		RunID:     uuid.New(),
		CreatedAt: time.Now().UTC(),
	}, "ok", nil, nil)
	s.Require().NoError(err)

	afterStatusRows := s.dbWriteValue(metrics.DBWriteCategoryTaskRunStatus)
	afterEventRows := s.dbWriteValue(metrics.DBWriteCategoryEventInsert)
	afterStatusStmts := s.dbStatementValue(metrics.DBWriteCategoryTaskRunStatus)
	afterEventStmts := s.dbStatementValue(metrics.DBWriteCategoryEventInsert)

	// 1 cache-hit status + 2 predecessor decrements = 3 rows in 2 statements.
	s.Equal(float64(3), afterStatusRows-beforeStatusRows,
		"fan-out=2 cache hit: expected 3 task_run_status rows (1 cached + 2 predecessors)")
	s.Equal(float64(2), afterStatusStmts-beforeStatusStmts,
		"fan-out=2 cache hit: expected 2 task_run_status statements")

	// task_cached (1) + task_ready×2 = 3 event rows in 1 batched INSERT.
	s.Equal(float64(3), afterEventRows-beforeEventRows,
		"fan-out=2 cache hit: expected 3 event rows (1 task_cached + 2 task_ready)")
	s.Equal(float64(1), afterEventStmts-beforeEventStmts,
		"fan-out=2 cache hit: expected 1 event_insert statement (3 events in one batched INSERT)")
}

func (s *StoreMetricsSuite) dbWriteValue(category string) float64 {
	var m dto.Metric
	counter, err := metrics.DBWritesTotal.GetMetricWithLabelValues(category)
	s.Require().NoError(err)
	s.Require().NoError(counter.(prometheus.Metric).Write(&m))
	return m.GetCounter().GetValue()
}

func (s *StoreMetricsSuite) dbStatementValue(category string) float64 {
	var m dto.Metric
	counter, err := metrics.DBStatementsTotal.GetMetricWithLabelValues(category)
	s.Require().NoError(err)
	s.Require().NoError(counter.(prometheus.Metric).Write(&m))
	return m.GetCounter().GetValue()
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

func (s *StoreMetricsSuite) histogramValue(histogram prometheus.Histogram) (uint64, float64) {
	var m dto.Metric
	s.Require().NoError(histogram.Write(&m))
	h := m.GetHistogram()
	s.Require().NotNil(h)
	return h.GetSampleCount(), h.GetSampleSum()
}

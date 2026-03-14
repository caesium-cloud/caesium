package task

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type TaskSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestTaskSuite(t *testing.T) {
	suite.Run(t, new(TaskSuite))
}

func (s *TaskSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *TaskSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *TaskSuite) svc() *taskService {
	return &taskService{ctx: context.Background(), db: s.db}
}

func (s *TaskSuite) createTrigger() uuid.UUID {
	id := uuid.New()
	s.Require().NoError(s.db.Create(&models.Trigger{
		ID:   id,
		Type: models.TriggerTypeCron,
	}).Error)
	return id
}

func (s *TaskSuite) createJob(triggerID uuid.UUID) uuid.UUID {
	id := uuid.New()
	s.Require().NoError(s.db.Create(&models.Job{
		ID:        id,
		Alias:     "test-job",
		TriggerID: triggerID,
	}).Error)
	return id
}

func (s *TaskSuite) createAtom() uuid.UUID {
	id := uuid.New()
	s.Require().NoError(s.db.Create(&models.Atom{
		ID:      id,
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:latest",
		Command: `["echo","hello"]`,
	}).Error)
	return id
}

func (s *TaskSuite) createTask(jobID, atomID uuid.UUID) *models.Task {
	svc := s.svc()
	task, err := svc.Create(&CreateRequest{
		JobID:  jobID.String(),
		AtomID: atomID.String(),
	})
	s.Require().NoError(err)
	return task
}

// --- List ---

func (s *TaskSuite) TestListEmpty() {
	tasks, err := s.svc().List(&ListRequest{})
	s.Require().NoError(err)
	s.Empty(tasks)
}

func (s *TaskSuite) TestListByJobID() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID := s.createAtom()
	s.createTask(jobID, atomID)

	// Create another job with a task to ensure filtering works
	jobID2 := s.createJob(triggerID)
	s.createTask(jobID2, atomID)

	tasks, err := s.svc().List(&ListRequest{JobID: jobID.String()})
	s.Require().NoError(err)
	s.Len(tasks, 1)
	s.Equal(jobID, tasks[0].JobID)
}

func (s *TaskSuite) TestListByAtomID() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID1 := s.createAtom()
	atomID2 := s.createAtom()
	s.createTask(jobID, atomID1)
	s.createTask(jobID, atomID2)

	tasks, err := s.svc().List(&ListRequest{AtomID: atomID1.String()})
	s.Require().NoError(err)
	s.Len(tasks, 1)
	s.Equal(atomID1, tasks[0].AtomID)
}

// --- Get ---

func (s *TaskSuite) TestGetFound() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID := s.createAtom()
	created := s.createTask(jobID, atomID)

	task, err := s.svc().Get(created.ID)
	s.Require().NoError(err)
	s.Equal(created.ID, task.ID)
	s.Equal(jobID, task.JobID)
	s.Equal(atomID, task.AtomID)
}

func (s *TaskSuite) TestGetNotFound() {
	_, err := s.svc().Get(uuid.New())
	s.Error(err)
}

// --- Create ---

func (s *TaskSuite) TestCreateBasic() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID := s.createAtom()

	task, err := s.svc().Create(&CreateRequest{
		JobID:  jobID.String(),
		AtomID: atomID.String(),
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, task.ID)
	s.Equal(jobID, task.JobID)
	s.Equal(atomID, task.AtomID)
	s.Nil(task.NextID)
}

func (s *TaskSuite) TestCreateWithNextID() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID := s.createAtom()

	// Create first task
	first := s.createTask(jobID, atomID)

	// Create second task pointing to first
	nextID := first.ID.String()
	task, err := s.svc().Create(&CreateRequest{
		JobID:  jobID.String(),
		AtomID: atomID.String(),
		NextID: &nextID,
	})
	s.Require().NoError(err)
	s.Require().NotNil(task.NextID)
	s.Equal(first.ID, *task.NextID)
}

func (s *TaskSuite) TestCreateWithNodeSelector() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID := s.createAtom()

	task, err := s.svc().Create(&CreateRequest{
		JobID:        jobID.String(),
		AtomID:       atomID.String(),
		NodeSelector: map[string]string{"region": "us-east-1"},
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, task.ID)
}

// --- Delete ---

func (s *TaskSuite) TestDelete() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID := s.createAtom()
	task := s.createTask(jobID, atomID)

	err := s.svc().Delete(task.ID)
	s.Require().NoError(err)

	_, err = s.svc().Get(task.ID)
	s.Error(err)
}

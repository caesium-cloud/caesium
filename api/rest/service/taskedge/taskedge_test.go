package taskedge

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type TaskEdgeSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestTaskEdgeSuite(t *testing.T) {
	suite.Run(t, new(TaskEdgeSuite))
}

func (s *TaskEdgeSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *TaskEdgeSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *TaskEdgeSuite) svc() *taskEdgeService {
	return &taskEdgeService{ctx: context.Background(), db: s.db}
}

func (s *TaskEdgeSuite) createTrigger() uuid.UUID {
	id := uuid.New()
	s.Require().NoError(s.db.Create(&models.Trigger{
		ID:   id,
		Type: models.TriggerTypeCron,
	}).Error)
	return id
}

func (s *TaskEdgeSuite) createJob(triggerID uuid.UUID) uuid.UUID {
	id := uuid.New()
	s.Require().NoError(s.db.Create(&models.Job{
		ID:        id,
		Alias:     "test-job",
		TriggerID: triggerID,
	}).Error)
	return id
}

func (s *TaskEdgeSuite) createAtom() uuid.UUID {
	id := uuid.New()
	s.Require().NoError(s.db.Create(&models.Atom{
		ID:      id,
		Engine:  models.AtomEngineDocker,
		Image:   "alpine:latest",
		Command: `["echo","hello"]`,
	}).Error)
	return id
}

func (s *TaskEdgeSuite) createTask(jobID, atomID uuid.UUID) uuid.UUID {
	id := uuid.New()
	s.Require().NoError(s.db.Create(&models.Task{
		ID:     id,
		JobID:  jobID,
		AtomID: atomID,
	}).Error)
	return id
}

// --- List ---

func (s *TaskEdgeSuite) TestListEmpty() {
	edges, err := s.svc().List(&ListRequest{})
	s.Require().NoError(err)
	s.Empty(edges)
}

func (s *TaskEdgeSuite) TestListByJobID() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID1 := s.createAtom()
	atomID2 := s.createAtom()
	taskID1 := s.createTask(jobID, atomID1)
	taskID2 := s.createTask(jobID, atomID2)

	// Create an edge
	_, err := s.svc().Create(&CreateRequest{
		JobID:      jobID.String(),
		FromTaskID: taskID1.String(),
		ToTaskID:   taskID2.String(),
	})
	s.Require().NoError(err)

	// Create another job with an edge to verify filtering
	jobID2 := s.createJob(triggerID)
	taskID3 := s.createTask(jobID2, atomID1)
	taskID4 := s.createTask(jobID2, atomID2)
	_, err = s.svc().Create(&CreateRequest{
		JobID:      jobID2.String(),
		FromTaskID: taskID3.String(),
		ToTaskID:   taskID4.String(),
	})
	s.Require().NoError(err)

	edges, err := s.svc().List(&ListRequest{JobID: jobID.String()})
	s.Require().NoError(err)
	s.Len(edges, 1)
	s.Equal(jobID, edges[0].JobID)
}

// --- Create ---

func (s *TaskEdgeSuite) TestCreateValidEdge() {
	triggerID := s.createTrigger()
	jobID := s.createJob(triggerID)
	atomID1 := s.createAtom()
	atomID2 := s.createAtom()
	taskID1 := s.createTask(jobID, atomID1)
	taskID2 := s.createTask(jobID, atomID2)

	edge, err := s.svc().Create(&CreateRequest{
		JobID:      jobID.String(),
		FromTaskID: taskID1.String(),
		ToTaskID:   taskID2.String(),
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, edge.ID)
	s.Equal(jobID, edge.JobID)
	s.Equal(taskID1, edge.FromTaskID)
	s.Equal(taskID2, edge.ToTaskID)
}

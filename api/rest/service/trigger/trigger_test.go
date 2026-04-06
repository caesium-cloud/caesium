package trigger

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type TriggerSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestTriggerSuite(t *testing.T) {
	suite.Run(t, new(TriggerSuite))
}

func (s *TriggerSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *TriggerSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *TriggerSuite) svc() *triggerService {
	return &triggerService{ctx: context.Background(), db: s.db}
}

func (s *TriggerSuite) createTrigger(triggerType, alias string) *models.Trigger {
	svc := s.svc()
	config := map[string]interface{}{
		"schedule": "* * * * *",
	}
	if triggerType == string(models.TriggerTypeHTTP) {
		config = map[string]interface{}{
			"path": "/hooks/" + alias,
		}
	}
	trigger, err := svc.Create(&CreateRequest{
		Alias:         alias,
		Type:          triggerType,
		Configuration: config,
	})
	s.Require().NoError(err)
	return trigger
}

func (s *TriggerSuite) createHTTPTrigger(alias, path string) *models.Trigger {
	svc := s.svc()
	trigger, err := svc.Create(&CreateRequest{
		Alias: alias,
		Type:  string(models.TriggerTypeHTTP),
		Configuration: map[string]interface{}{
			"path": path,
		},
	})
	s.Require().NoError(err)
	return trigger
}

// --- List ---

func (s *TriggerSuite) TestListEmpty() {
	triggers, err := s.svc().List(&ListRequest{})
	s.Require().NoError(err)
	s.Empty(triggers)
}

func (s *TriggerSuite) TestListByType() {
	s.createTrigger(string(models.TriggerTypeCron), "cron-trigger")
	s.createTrigger(string(models.TriggerTypeHTTP), "http-trigger")

	triggers, err := s.svc().List(&ListRequest{Type: string(models.TriggerTypeCron)})
	s.Require().NoError(err)
	s.Len(triggers, 1)
	s.Equal(models.TriggerTypeCron, triggers[0].Type)
}

func (s *TriggerSuite) TestListWithPagination() {
	for i := 0; i < 5; i++ {
		s.createTrigger(string(models.TriggerTypeCron), "trigger")
	}

	triggers, err := s.svc().List(&ListRequest{Limit: 2, Offset: 1})
	s.Require().NoError(err)
	s.Len(triggers, 2)
}

func (s *TriggerSuite) TestListByPath() {
	matched := s.createHTTPTrigger("webhook-matched", "/hooks/run")
	s.createHTTPTrigger("webhook-other", "deploy/other")
	s.createTrigger(string(models.TriggerTypeCron), "cron-trigger")

	triggers, err := s.svc().ListByPath("run")
	s.Require().NoError(err)
	s.Len(triggers, 1)
	s.Equal(matched.ID, triggers[0].ID)

	triggers, err = s.svc().ListByPath("/hooks/run")
	s.Require().NoError(err)
	s.Len(triggers, 1)
	s.Equal(matched.ID, triggers[0].ID)

	triggers, err = s.svc().ListByPath("/v1/hooks/run")
	s.Require().NoError(err)
	s.Len(triggers, 1)
	s.Equal(matched.ID, triggers[0].ID)
}

// --- Get ---

func (s *TriggerSuite) TestGetFound() {
	created := s.createTrigger(string(models.TriggerTypeCron), "my-trigger")

	trigger, err := s.svc().Get(created.ID)
	s.Require().NoError(err)
	s.Equal(created.ID, trigger.ID)
	s.Equal("my-trigger", trigger.Alias)
	s.Equal(models.TriggerTypeCron, trigger.Type)
}

func (s *TriggerSuite) TestGetNotFound() {
	_, err := s.svc().Get(uuid.New())
	s.Error(err)
}

// --- Create ---

func (s *TriggerSuite) TestCreateCron() {
	trigger, err := s.svc().Create(&CreateRequest{
		Alias: "daily-cron",
		Type:  string(models.TriggerTypeCron),
		Configuration: map[string]interface{}{
			"schedule": "0 0 * * *",
		},
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, trigger.ID)
	s.Equal("daily-cron", trigger.Alias)
	s.Equal(models.TriggerTypeCron, trigger.Type)
	s.Contains(trigger.Configuration, "schedule")
}

func (s *TriggerSuite) TestCreateHTTP() {
	trigger, err := s.svc().Create(&CreateRequest{
		Alias: "webhook",
		Type:  string(models.TriggerTypeHTTP),
		Configuration: map[string]interface{}{
			"path": "/hooks/webhook",
		},
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, trigger.ID)
	s.Equal(models.TriggerTypeHTTP, trigger.Type)
	s.Equal("webhook", trigger.NormalizedPath)
}

func (s *TriggerSuite) TestUpdateHTTPConfigurationRefreshesNormalizedPath() {
	created := s.createHTTPTrigger("webhook", "/hooks/original")

	updated, err := s.svc().Update(created.ID, &UpdateRequest{
		Configuration: map[string]interface{}{
			"path": "/v1/hooks/updated/path",
		},
	})
	s.Require().NoError(err)
	s.Equal("updated/path", updated.NormalizedPath)

	byPath, err := s.svc().ListByPath("/hooks/updated/path")
	s.Require().NoError(err)
	s.Len(byPath, 1)
	s.Equal(created.ID, byPath[0].ID)
}

// --- Update (Delete used as proxy) ---

func (s *TriggerSuite) TestDelete() {
	trigger := s.createTrigger(string(models.TriggerTypeCron), "to-delete")

	err := s.svc().Delete(trigger.ID)
	s.Require().NoError(err)

	_, err = s.svc().Get(trigger.ID)
	s.Error(err)
}

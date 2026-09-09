package notification

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// NotificationServiceSuite covers the channel/policy CRUD the notification
// controller drives — there was no unit coverage of this service at all
// before issue #413 wired audit logging around it, so a mutation regression
// here previously would have shipped visible only through the integration
// lane.
type NotificationServiceSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestNotificationServiceSuite(t *testing.T) {
	suite.Run(t, new(NotificationServiceSuite))
}

func (s *NotificationServiceSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *NotificationServiceSuite) TearDownTest() {
	if s.db != nil {
		if sqlDB, err := s.db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *NotificationServiceSuite) svc() *service {
	return &service{ctx: context.Background(), db: s.db}
}

func (s *NotificationServiceSuite) TestChannelCreateGetUpdateDelete() {
	svc := s.svc()

	created, err := svc.CreateChannel(&CreateChannelRequest{
		Name:   "pager-primary",
		Type:   "webhook",
		Config: map[string]any{"url": "https://example.invalid/hook"},
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, created.ID)
	s.True(created.Enabled, "channels default to enabled")

	fetched, err := svc.GetChannel(created.ID)
	s.Require().NoError(err)
	s.Equal("pager-primary", fetched.Name)

	newName := "pager-secondary"
	updated, err := svc.UpdateChannel(created.ID, &UpdateChannelRequest{Name: &newName})
	s.Require().NoError(err)
	s.Equal(newName, updated.Name)

	s.Require().NoError(svc.DeleteChannel(created.ID))
	_, err = svc.GetChannel(created.ID)
	s.Error(err, "a deleted channel must not be readable")
}

func (s *NotificationServiceSuite) TestChannelNameConflict() {
	svc := s.svc()

	_, err := svc.CreateChannel(&CreateChannelRequest{Name: "dup", Type: "webhook"})
	s.Require().NoError(err)

	_, err = svc.CreateChannel(&CreateChannelRequest{Name: "dup", Type: "webhook"})
	s.ErrorIs(err, ErrChannelNameConflict)
}

func (s *NotificationServiceSuite) TestPolicyCreateGetUpdateDelete() {
	svc := s.svc()

	channel, err := svc.CreateChannel(&CreateChannelRequest{Name: "chan", Type: "webhook"})
	s.Require().NoError(err)

	created, err := svc.CreatePolicy(&CreatePolicyRequest{
		Name:       "page-oncall",
		ChannelID:  channel.ID,
		EventTypes: []string{"run_failed"},
	})
	s.Require().NoError(err)
	s.True(created.Enabled)

	fetched, err := svc.GetPolicy(created.ID)
	s.Require().NoError(err)
	s.Equal("page-oncall", fetched.Name)

	disabled := false
	updated, err := svc.UpdatePolicy(created.ID, &UpdatePolicyRequest{Enabled: &disabled})
	s.Require().NoError(err)
	s.False(updated.Enabled)

	s.Require().NoError(svc.DeletePolicy(created.ID))
	_, err = svc.GetPolicy(created.ID)
	s.Error(err, "a deleted policy must not be readable")
}

func (s *NotificationServiceSuite) TestCreatePolicyRequiresExistingChannel() {
	svc := s.svc()

	_, err := svc.CreatePolicy(&CreatePolicyRequest{
		Name:       "orphan",
		ChannelID:  uuid.New(),
		EventTypes: []string{"run_failed"},
	})
	s.ErrorIs(err, ErrInvalidPolicy)
}

package notification

import (
	"context"
	"testing"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
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
	s.db = jobdeftestutil.OpenTestDB(s.T())
}

func (s *NotificationServiceSuite) TearDownTest() {
	jobdeftestutil.CloseDB(s.db)
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

func (s *NotificationServiceSuite) channelIn(name, namespace string) *models.NotificationChannel {
	ch, err := s.svc().CreateChannel(&CreateChannelRequest{Name: name, Type: "webhook", Namespace: namespace})
	s.Require().NoError(err)
	return ch
}

func (s *NotificationServiceSuite) TestChannelNamespaceCreateOnly() {
	svc := s.svc()

	s.Equal(models.DefaultNamespace, s.channelIn("ns-omitted", "").Namespace, "omitted namespace means default, never shared")
	s.Equal(models.ClusterNamespace, s.channelIn("ns-shared", " * ").Namespace)
	marketing := s.channelIn("ns-marketing", "marketing")
	s.Equal("marketing", marketing.Namespace)

	fetched, err := svc.GetChannel(marketing.ID)
	s.Require().NoError(err)
	s.Equal("marketing", fetched.Namespace)

	_, err = svc.CreateChannel(&CreateChannelRequest{Name: "ns-bad", Type: "webhook", Namespace: "Bad_NS"})
	s.Require().ErrorIs(err, ErrInvalidChannel)
	s.NotErrorIs(err, models.ErrNamespaceImmutable)

	finance, renamed := "finance", "ns-renamed"
	_, err = svc.UpdateChannel(marketing.ID, &UpdateChannelRequest{Namespace: &finance, Name: &renamed})
	s.Require().ErrorIs(err, models.ErrNamespaceImmutable)
	s.ErrorIs(err, ErrInvalidChannel)
	fetched, err = svc.GetChannel(marketing.ID)
	s.Require().NoError(err)
	s.Equal("ns-marketing", fetched.Name, "a rejected update must write nothing")
	s.Equal("marketing", fetched.Namespace)

	echo, blank := "marketing", ""
	updated, err := svc.UpdateChannel(marketing.ID, &UpdateChannelRequest{Namespace: &echo, Name: &renamed})
	s.Require().NoError(err)
	s.Equal("ns-renamed", updated.Name)
	s.Equal("marketing", updated.Namespace)
	disabled := false
	updated, err = svc.UpdateChannel(marketing.ID, &UpdateChannelRequest{Namespace: &blank, Enabled: &disabled})
	s.Require().NoError(err)
	s.False(updated.Enabled)
	s.Equal("marketing", updated.Namespace, "blank namespace on update means no change")
}

func (s *NotificationServiceSuite) TestPolicyNamespaceCreateOnly() {
	svc := s.svc()
	shared := s.channelIn("ns-shared-chan", models.ClusterNamespace)

	omitted, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "ns-omitted", ChannelID: shared.ID, EventTypes: []string{"run_failed"}})
	s.Require().NoError(err)
	s.Equal(models.DefaultNamespace, omitted.Namespace, "a policy never inherits its channel's namespace")

	policy, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "ns-marketing", Namespace: "marketing", ChannelID: shared.ID, EventTypes: []string{"run_failed"}})
	s.Require().NoError(err)
	s.Equal("marketing", policy.Namespace)

	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: "ns-bad", Namespace: "a_b", ChannelID: shared.ID, EventTypes: []string{"run_failed"}})
	s.Require().ErrorIs(err, ErrInvalidPolicy)

	finance, disabled := "finance", false
	_, err = svc.UpdatePolicy(policy.ID, &UpdatePolicyRequest{Namespace: &finance, Enabled: &disabled})
	s.Require().ErrorIs(err, models.ErrNamespaceImmutable)
	s.ErrorIs(err, ErrInvalidPolicy)
	fetched, err := svc.GetPolicy(policy.ID)
	s.Require().NoError(err)
	s.True(fetched.Enabled, "a rejected update must write nothing")
	s.Equal("marketing", fetched.Namespace)

	echo := "marketing"
	updated, err := svc.UpdatePolicy(policy.ID, &UpdatePolicyRequest{Namespace: &echo, Enabled: &disabled})
	s.Require().NoError(err)
	s.False(updated.Enabled)
	s.Equal("marketing", updated.Namespace)
}

func (s *NotificationServiceSuite) TestPolicyChannelMustBeVisible() {
	svc := s.svc()
	financeChan := s.channelIn("chan-finance", "finance")
	marketingChan := s.channelIn("chan-marketing", "marketing")
	sharedChan := s.channelIn("chan-shared", models.ClusterNamespace)
	defaultChan := s.channelIn("chan-default", "")

	_, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "mkt-to-fin", Namespace: "marketing", ChannelID: financeChan.ID, EventTypes: []string{"run_failed"}})
	s.Require().ErrorIs(err, ErrInvalidPolicy)
	s.Contains(err.Error(), `"finance"`)

	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: "default-to-fin", ChannelID: financeChan.ID, EventTypes: []string{"run_failed"}})
	s.Require().ErrorIs(err, ErrInvalidPolicy, "an omitted policy namespace is default and cannot reach a finance channel")

	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: "shared-to-mkt", Namespace: models.ClusterNamespace, ChannelID: marketingChan.ID, EventTypes: []string{"run_failed"}})
	s.Require().ErrorIs(err, ErrInvalidPolicy, "a shared policy may reference only shared channels")

	var count int64
	s.Require().NoError(s.db.Model(&models.NotificationPolicy{}).Count(&count).Error)
	s.Zero(count, "rejected creates must write nothing")

	viaShared, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "mkt-to-shared", Namespace: "marketing", ChannelID: sharedChan.ID, EventTypes: []string{"run_failed"}})
	s.Require().NoError(err)
	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: "mkt-to-mkt", Namespace: "marketing", ChannelID: marketingChan.ID, EventTypes: []string{"run_failed"}})
	s.Require().NoError(err)
	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: "default-to-default", ChannelID: defaultChan.ID, EventTypes: []string{"run_failed"}})
	s.Require().NoError(err)

	_, err = svc.UpdatePolicy(viaShared.ID, &UpdatePolicyRequest{ChannelID: &financeChan.ID})
	s.Require().ErrorIs(err, ErrInvalidPolicy, "a channel_id update to a foreign channel is rejected")
	fetched, err := svc.GetPolicy(viaShared.ID)
	s.Require().NoError(err)
	s.Equal(sharedChan.ID, fetched.ChannelID)

	updated, err := svc.UpdatePolicy(viaShared.ID, &UpdatePolicyRequest{ChannelID: &marketingChan.ID})
	s.Require().NoError(err)
	s.Equal(marketingChan.ID, updated.ChannelID)
}

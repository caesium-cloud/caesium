package agentprofile

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type AgentProfileSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestAgentProfileSuite(t *testing.T) {
	suite.Run(t, new(AgentProfileSuite))
}

func (s *AgentProfileSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *AgentProfileSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *AgentProfileSuite) svc() *service {
	return &service{ctx: context.Background(), db: s.db}
}

func (s *AgentProfileSuite) TestCreateGetListUpdateDelete() {
	svc := s.svc()

	created, err := svc.Create(&CreateRequest{
		Name:  "test-profile",
		Image: "example/agent:latest",
		Limits: map[string]interface{}{
			"cpu": "1",
		},
		SecretRefs: map[string]string{
			"model_api_key": "secret://env/AGENT_KEY",
		},
	})
	s.Require().NoError(err)
	s.Require().NotEqual(uuid.Nil, created.ID)
	s.Equal(models.AtomEngineDocker, created.Engine, "engine should default to docker")

	fetched, err := svc.Get(created.ID)
	s.Require().NoError(err)
	s.Equal("test-profile", fetched.Name)

	list, err := svc.List(nil)
	s.Require().NoError(err)
	s.Len(list, 1)

	newImage := "example/agent:v2"
	updated, err := svc.Update(created.ID, &UpdateRequest{Image: &newImage})
	s.Require().NoError(err)
	s.Equal(newImage, updated.Image)

	s.Require().NoError(svc.Delete(created.ID))
	_, err = svc.Get(created.ID)
	s.Error(err)
}

func (s *AgentProfileSuite) TestCreateRequiresName() {
	svc := s.svc()
	_, err := svc.Create(&CreateRequest{Image: "example/agent:latest"})
	s.Require().Error(err)
	s.ErrorIs(err, ErrInvalidProfile)
}

func (s *AgentProfileSuite) TestCreateRequiresImage() {
	svc := s.svc()
	_, err := svc.Create(&CreateRequest{Name: "no-image"})
	s.Require().Error(err)
	s.ErrorIs(err, ErrInvalidProfile)
}

func (s *AgentProfileSuite) TestCreateRejectsDuplicateName() {
	svc := s.svc()
	_, err := svc.Create(&CreateRequest{Name: "dup", Image: "example/agent:latest"})
	s.Require().NoError(err)

	_, err = svc.Create(&CreateRequest{Name: "dup", Image: "example/agent:latest"})
	s.Require().Error(err)
	s.ErrorIs(err, ErrProfileNameConflict)
}

func (s *AgentProfileSuite) TestCreateRejectsUnknownEngine() {
	svc := s.svc()
	_, err := svc.Create(&CreateRequest{Name: "bad-engine", Image: "img", Engine: "lxc"})
	s.Require().Error(err)
	s.ErrorIs(err, ErrInvalidProfile)
}

func (s *AgentProfileSuite) TestCreateRejectsMalformedSecretRef() {
	svc := s.svc()
	_, err := svc.Create(&CreateRequest{
		Name:       "bad-secret",
		Image:      "img",
		SecretRefs: map[string]string{"key": "plaintext-not-a-uri"},
	})
	s.Require().Error(err)
	s.ErrorIs(err, ErrInvalidProfile)
}

func (s *AgentProfileSuite) TestCreateRejectsUnsupportedSecretProvider() {
	svc := s.svc()
	_, err := svc.Create(&CreateRequest{
		Name:       "bad-provider",
		Image:      "img",
		SecretRefs: map[string]string{"key": "secret://ssh/host/key"},
	})
	s.Require().Error(err)
	s.ErrorIs(err, ErrInvalidProfile)
}

func (s *AgentProfileSuite) TestSeedDefaultsIsIdempotent() {
	s.Require().NoError(SeedDefaults(context.Background(), s.db))
	s.Require().NoError(SeedDefaults(context.Background(), s.db))

	var profiles []models.AgentProfile
	s.Require().NoError(s.db.Where("name = ?", DefaultTriageOnlyProfileName).Find(&profiles).Error)
	s.Len(profiles, 1)
}

func (s *AgentProfileSuite) TestDefaultTriageOnlyProfileNameMatchesModel() {
	s.Equal(models.DefaultTriageOnlyProfileName, DefaultTriageOnlyProfileName)
}

func (s *AgentProfileSuite) TestCreateTrimsSecretRefValue() {
	svc := s.svc()
	created, err := svc.Create(&CreateRequest{
		Name:       "trim-secret",
		Image:      "img",
		SecretRefs: map[string]string{"key": "  secret://env/AGENT_KEY  "},
	})
	s.Require().NoError(err)

	fetched, err := svc.Get(created.ID)
	s.Require().NoError(err)
	var refs map[string]string
	s.Require().NoError(json.Unmarshal(fetched.SecretRefs, &refs))
	s.Equal("secret://env/AGENT_KEY", refs["key"], "stored secret ref should be trimmed")
}

// TestEnsureSeededRetriesAfterFailure proves seeding is retryable rather than
// permanently swallowed on a transient failure: a nil DB fails to seed and
// leaves the done flag unset, so a later ensureSeeded with a live DB succeeds.
func (s *AgentProfileSuite) TestEnsureSeededRetriesAfterFailure() {
	seedMu.Lock()
	seededOK = false
	seedMu.Unlock()
	s.T().Cleanup(func() {
		seedMu.Lock()
		seededOK = false
		seedMu.Unlock()
	})

	// A failed seed (nil conn) must not set the done flag.
	ensureSeeded(nil)
	seedMu.RLock()
	doneAfterFail := seededOK
	seedMu.RUnlock()
	s.False(doneAfterFail, "a failed seed must leave the retry flag unset")

	// A subsequent seed against a live DB succeeds and materializes the row.
	ensureSeeded(s.db)
	seedMu.RLock()
	doneAfterSuccess := seededOK
	seedMu.RUnlock()
	s.True(doneAfterSuccess)

	var profiles []models.AgentProfile
	s.Require().NoError(s.db.Where("name = ?", DefaultTriageOnlyProfileName).Find(&profiles).Error)
	s.Len(profiles, 1)
}

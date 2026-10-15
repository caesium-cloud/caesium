package agentprofile

import (
	"context"
	"encoding/json"
	"testing"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
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
	s.db = jobdeftestutil.OpenTestDB(s.T())
}

func (s *AgentProfileSuite) TearDownTest() {
	jobdeftestutil.CloseDB(s.db)
}

func (s *AgentProfileSuite) svc() *service {
	return &service{ctx: context.Background(), db: s.db}
}

func (s *AgentProfileSuite) TestCreateGetListUpdateDelete() {
	svc := s.svc()

	created, err := svc.Create(&CreateRequest{
		Name:  "test-profile",
		Image: "example/agent:latest",
		Limits: map[string]any{
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

func (s *AgentProfileSuite) TestValidateEngineKeepsExactMembershipAndDefault() {
	for _, engine := range []models.AtomEngine{
		models.AtomEngineDocker,
		models.AtomEnginePodman,
		models.AtomEngineKubernetes,
	} {
		got, err := validateEngine(engine)
		s.Require().NoError(err)
		s.Equal(engine, got)
	}

	for _, blank := range []models.AtomEngine{"", " ", "\t"} {
		got, err := validateEngine(blank)
		s.Require().NoError(err)
		s.Equal(models.AtomEngineDocker, got)
	}

	for _, engine := range []models.AtomEngine{"Docker", " docker", "docker ", "lxc"} {
		got, err := validateEngine(engine)
		s.Empty(got)
		s.Require().ErrorIs(err, ErrInvalidProfile)
		s.Equal(`invalid agent profile: unsupported engine "`+string(engine)+`"`, err.Error())
	}
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
	s.Require().Len(profiles, 1)
	s.Equal(models.ClusterNamespace, profiles[0].Namespace, "the built-in default is cluster-shared")
}

func (s *AgentProfileSuite) TestCreateNamespaceDefaultsSharedAndRejectsInvalid() {
	svc := s.svc()

	omitted, err := svc.Create(&CreateRequest{Name: "ns-omitted", Image: "img"})
	s.Require().NoError(err)
	s.Equal(models.DefaultNamespace, omitted.Namespace, "omitted namespace means default, never shared")

	blank, err := svc.Create(&CreateRequest{Name: "ns-blank", Image: "img", Namespace: "   "})
	s.Require().NoError(err)
	s.Equal(models.DefaultNamespace, blank.Namespace)

	explicit, err := svc.Create(&CreateRequest{Name: "ns-marketing", Image: "img", Namespace: " marketing "})
	s.Require().NoError(err)
	s.Equal("marketing", explicit.Namespace)

	shared, err := svc.Create(&CreateRequest{Name: "ns-shared", Image: "img", Namespace: "*"})
	s.Require().NoError(err)
	s.Equal(models.ClusterNamespace, shared.Namespace)

	fetched, err := svc.Get(explicit.ID)
	s.Require().NoError(err)
	s.Equal("marketing", fetched.Namespace)

	for _, bad := range []string{"Bad_NS", "**", "ns/x"} {
		_, err := svc.Create(&CreateRequest{Name: "ns-bad", Image: "img", Namespace: bad})
		s.Require().Error(err, bad)
		s.ErrorIs(err, ErrInvalidProfile)
		s.NotErrorIs(err, models.ErrNamespaceImmutable)
	}
	var count int64
	s.Require().NoError(s.db.Model(&models.AgentProfile{}).Where("name = ?", "ns-bad").Count(&count).Error)
	s.Zero(count)
}

func (s *AgentProfileSuite) TestUpdateRejectsNamespaceChangeWithoutWriting() {
	svc := s.svc()
	created, err := svc.Create(&CreateRequest{Name: "ns-move", Image: "img:v1", Namespace: "marketing"})
	s.Require().NoError(err)

	finance, image := "finance", "img:v2"
	_, err = svc.Update(created.ID, &UpdateRequest{Namespace: &finance, Image: &image})
	s.Require().Error(err)
	s.ErrorIs(err, models.ErrNamespaceImmutable)
	s.ErrorIs(err, ErrInvalidProfile)

	shared := "*"
	_, err = svc.Update(created.ID, &UpdateRequest{Namespace: &shared, Image: &image})
	s.Require().ErrorIs(err, models.ErrNamespaceImmutable, "moving to the shared namespace is a move too")

	fetched, err := svc.Get(created.ID)
	s.Require().NoError(err)
	s.Equal("marketing", fetched.Namespace)
	s.Equal("img:v1", fetched.Image, "a rejected update must write nothing")
}

func (s *AgentProfileSuite) TestUpdateAcceptsEchoedOrEmptyNamespace() {
	svc := s.svc()
	created, err := svc.Create(&CreateRequest{Name: "ns-echo", Image: "img:v1", Namespace: "marketing"})
	s.Require().NoError(err)

	echo, image := "marketing", "img:v2"
	updated, err := svc.Update(created.ID, &UpdateRequest{Namespace: &echo, Image: &image})
	s.Require().NoError(err)
	s.Equal("img:v2", updated.Image)
	s.Equal("marketing", updated.Namespace)

	blank, image3 := "", "img:v3"
	updated, err = svc.Update(created.ID, &UpdateRequest{Namespace: &blank, Image: &image3})
	s.Require().NoError(err)
	s.Equal("img:v3", updated.Image)
	s.Equal("marketing", updated.Namespace, "blank namespace on update means no change")

	image4 := "img:v4"
	updated, err = svc.Update(created.ID, &UpdateRequest{Image: &image4})
	s.Require().NoError(err)
	s.Equal("marketing", updated.Namespace)
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

package atom

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type AtomSuite struct {
	suite.Suite
	db *gorm.DB
}

func TestAtomSuite(t *testing.T) {
	suite.Run(t, new(AtomSuite))
}

func (s *AtomSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
}

func (s *AtomSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

func (s *AtomSuite) svc() *atomService {
	return &atomService{ctx: context.Background(), db: s.db}
}

func (s *AtomSuite) createAtom(engine, image string, command []string) *models.Atom {
	svc := s.svc()
	atom, err := svc.Create(&CreateRequest{
		Engine:  engine,
		Image:   image,
		Command: command,
	})
	s.Require().NoError(err)
	return atom
}

// --- List ---

func (s *AtomSuite) TestListEmpty() {
	atoms, err := s.svc().List(&ListRequest{})
	s.Require().NoError(err)
	s.Empty(atoms)
}

func (s *AtomSuite) TestListWithEngineFilter() {
	s.createAtom("docker", "alpine:latest", []string{"echo", "hello"})
	s.createAtom("kubernetes", "nginx:latest", []string{"nginx"})

	atoms, err := s.svc().List(&ListRequest{Engine: "docker"})
	s.Require().NoError(err)
	s.Len(atoms, 1)
	s.Equal(models.AtomEngine("docker"), atoms[0].Engine)
}

func (s *AtomSuite) TestListWithPagination() {
	for i := 0; i < 5; i++ {
		s.createAtom("docker", "alpine:latest", []string{"echo"})
	}

	atoms, err := s.svc().List(&ListRequest{Limit: 2, Offset: 1})
	s.Require().NoError(err)
	s.Len(atoms, 2)
}

// --- Get ---

func (s *AtomSuite) TestGetFound() {
	created := s.createAtom("docker", "alpine:latest", []string{"echo", "hi"})

	atom, err := s.svc().Get(created.ID)
	s.Require().NoError(err)
	s.Equal(created.ID, atom.ID)
	s.Equal(models.AtomEngine("docker"), atom.Engine)
	s.Equal("alpine:latest", atom.Image)
}

func (s *AtomSuite) TestGetNotFound() {
	_, err := s.svc().Get(uuid.New())
	s.Error(err)
}

// --- Create ---

func (s *AtomSuite) TestCreateBasic() {
	atom, err := s.svc().Create(&CreateRequest{
		Engine:  "docker",
		Image:   "alpine:latest",
		Command: []string{"echo", "hello"},
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, atom.ID)
	s.Equal(models.AtomEngine("docker"), atom.Engine)
	s.Equal("alpine:latest", atom.Image)
}

func (s *AtomSuite) TestCreateWithSpec() {
	atom, err := s.svc().Create(&CreateRequest{
		Engine:  "docker",
		Image:   "alpine:latest",
		Command: []string{"echo"},
		Spec: container.Spec{
			Env:     map[string]string{"FOO": "bar"},
			WorkDir: "/app",
		},
	})
	s.Require().NoError(err)
	s.NotEqual(uuid.Nil, atom.ID)

	// Verify spec was stored
	fetched, err := s.svc().Get(atom.ID)
	s.Require().NoError(err)
	spec := fetched.ContainerSpec()
	s.Equal("/app", spec.WorkDir)
	s.Equal("bar", spec.Env["FOO"])
}

// --- Delete ---

func (s *AtomSuite) TestDelete() {
	atom := s.createAtom("docker", "alpine:latest", []string{"echo"})

	err := s.svc().Delete(atom.ID)
	s.Require().NoError(err)

	_, err = s.svc().Get(atom.ID)
	s.Error(err)
}

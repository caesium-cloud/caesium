package jobdef

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/suite"
	"gorm.io/gorm"
)

type ImporterTestSuite struct {
	suite.Suite
	db       *gorm.DB
	importer *Importer
}

func TestImporterSuite(t *testing.T) {
	suite.Run(t, new(ImporterTestSuite))
}

func (s *ImporterTestSuite) SetupTest() {
	s.db = testutil.OpenTestDB(s.T())
	s.importer = NewImporter(s.db)
}

func (s *ImporterTestSuite) TearDownTest() {
	testutil.CloseDB(s.db)
}

func (s *ImporterTestSuite) TestApplyCreatesRecords() {
	def, err := schema.Parse([]byte(testutil.SampleJob))
	s.Require().NoError(err)

	ctx := context.Background()
	job, err := s.importer.Apply(ctx, def)
	s.Require().NoError(err)
	s.Equal("csv-to-parquet", job.Alias)
	s.Equal("data", job.Labels["team"])
	s.Equal("etl", job.Annotations["owner"])

	testutil.AssertCount(s.T(), s.db, &models.Trigger{}, 1)
	testutil.AssertCount(s.T(), s.db, &models.Atom{}, 3)
	testutil.AssertCount(s.T(), s.db, &models.Task{}, 3)
	testutil.AssertCount(s.T(), s.db, &models.Callback{}, 1)

	var tasks []models.Task
	s.Require().NoError(s.db.Where("job_id = ?", job.ID).Find(&tasks).Error)

	withNext := 0
	for _, task := range tasks {
		if task.NextID != nil {
			withNext++
		}
	}
	s.Equal(2, withNext)
}

func (s *ImporterTestSuite) TestDuplicateAliasFails() {
	def, err := schema.Parse([]byte(testutil.SampleJob))
	s.Require().NoError(err)

	ctx := context.Background()
	_, err = s.importer.Apply(ctx, def)
	s.Require().NoError(err)

	_, err = s.importer.Apply(ctx, def)
	s.Error(err)
}

func (s *ImporterTestSuite) TestApplyWithProvenance() {
	def, err := schema.Parse([]byte(testutil.SampleJob))
	s.Require().NoError(err)

	ctx := context.Background()
	prov := &Provenance{
		SourceID: "git-sync",
		Repo:     "https://example.com/repo.git",
		Ref:      "refs/heads/main",
		Commit:   "abcdef123",
		Path:     "jobs/sample.yaml",
	}

	job, err := s.importer.ApplyWithOptions(ctx, def, &ApplyOptions{Provenance: prov})
	s.Require().NoError(err)
	s.Equal("git-sync", job.ProvenanceSourceID)
	s.Equal(prov.Repo, job.ProvenanceRepo)
	s.Equal(prov.Ref, job.ProvenanceRef)
	s.Equal(prov.Commit, job.ProvenanceCommit)
	s.Equal(prov.Path, job.ProvenancePath)
	s.Equal("data", job.Labels["team"])
	s.Equal("etl", job.Annotations["owner"])
}

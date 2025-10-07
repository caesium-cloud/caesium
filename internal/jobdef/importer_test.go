package jobdef

import (
	"context"
	"net/url"
	"strings"
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

	var trigger models.Trigger
	s.Require().NoError(s.db.First(&trigger).Error)
	s.Empty(trigger.ProvenanceSourceID)
	s.Empty(trigger.ProvenanceRepo)
	s.Empty(trigger.ProvenanceRef)
	s.Empty(trigger.ProvenanceCommit)
	s.Empty(trigger.ProvenancePath)

	var atoms []models.Atom
	s.Require().NoError(s.db.Find(&atoms).Error)
	s.Len(atoms, 3)
	for _, atom := range atoms {
		s.Empty(atom.ProvenanceSourceID)
		s.Empty(atom.ProvenanceRepo)
		s.Empty(atom.ProvenanceRef)
		s.Empty(atom.ProvenanceCommit)
		s.Empty(atom.ProvenancePath)
	}

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

	var trigger models.Trigger
	s.Require().NoError(s.db.Where("alias = ?", job.Alias).First(&trigger).Error)
	s.Equal(prov.SourceID, trigger.ProvenanceSourceID)
	s.Equal(prov.Repo, trigger.ProvenanceRepo)
	s.Equal(prov.Ref, trigger.ProvenanceRef)
	s.Equal(prov.Commit, trigger.ProvenanceCommit)
	s.Equal(prov.Path+"#trigger", trigger.ProvenancePath)

	var atoms []models.Atom
	s.Require().NoError(s.db.Find(&atoms).Error)
	s.Len(atoms, 3)
	seen := make(map[string]struct{}, len(atoms))
	for _, atom := range atoms {
		s.Equal(prov.SourceID, atom.ProvenanceSourceID)
		s.Equal(prov.Repo, atom.ProvenanceRepo)
		s.Equal(prov.Ref, atom.ProvenanceRef)
		s.Equal(prov.Commit, atom.ProvenanceCommit)
		s.True(strings.HasPrefix(atom.ProvenancePath, prov.Path+"#step/"))
		nameEnc := strings.TrimPrefix(atom.ProvenancePath, prov.Path+"#step/")
		name, err := url.PathUnescape(nameEnc)
		s.Require().NoError(err)
		seen[name] = struct{}{}
	}
	s.Equal(map[string]struct{}{"list": {}, "convert": {}, "publish": {}}, seen)
}

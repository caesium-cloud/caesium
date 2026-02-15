package jobdef

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/datatypes"
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
	testutil.AssertCount(s.T(), s.db, &models.TaskEdge{}, 2)
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

	var edges []models.TaskEdge
	s.Require().NoError(s.db.Where("job_id = ?", job.ID).Find(&edges).Error)
	s.Len(edges, 2)

	edgeTargets := make(map[uuid.UUID]uuid.UUID, len(edges))
	for _, edge := range edges {
		edgeTargets[edge.FromTaskID] = edge.ToTaskID
	}

	for _, task := range tasks {
		if task.NextID == nil {
			continue
		}
		s.Equal(*task.NextID, edgeTargets[task.ID])
	}
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

func (s *ImporterTestSuite) TestApplyCreatesDAGEdges() {
	const dagManifest = `
apiVersion: v1
kind: Job
metadata:
  alias: fanout-job
trigger:
  type: cron
  configuration:
    cron: "* * * * *"
steps:
  - name: start
    image: repo/start
  - name: branch-a
    image: repo/branch-a
    dependsOn: start
  - name: branch-b
    image: repo/branch-b
    dependsOn: start
  - name: join
    image: repo/join
    dependsOn:
      - branch-a
      - branch-b
`

	def, err := schema.Parse([]byte(dagManifest))
	s.Require().NoError(err)

	ctx := context.Background()
	job, err := s.importer.Apply(ctx, def)
	s.Require().NoError(err)

	testutil.AssertCount(s.T(), s.db, &models.Task{}, 4)
	testutil.AssertCount(s.T(), s.db, &models.TaskEdge{}, 4)

	var tasks []models.Task
	s.Require().NoError(s.db.Where("job_id = ?", job.ID).Find(&tasks).Error)
	s.Len(tasks, 4)

	imageByTask := make(map[uuid.UUID]string, len(tasks))
	for _, task := range tasks {
		var atom models.Atom
		s.Require().NoError(s.db.First(&atom, "id = ?", task.AtomID).Error)
		imageByTask[task.ID] = atom.Image
	}

	var edges []models.TaskEdge
	s.Require().NoError(s.db.Where("job_id = ?", job.ID).Find(&edges).Error)
	s.Len(edges, 4)

	adj := make(map[string][]string)
	for _, edge := range edges {
		from := imageByTask[edge.FromTaskID]
		to := imageByTask[edge.ToTaskID]
		adj[from] = append(adj[from], to)
	}

	s.ElementsMatch([]string{"repo/branch-a", "repo/branch-b"}, adj["repo/start"])
	s.ElementsMatch([]string{"repo/join"}, adj["repo/branch-a"])
	s.ElementsMatch([]string{"repo/join"}, adj["repo/branch-b"])

	for _, task := range tasks {
		switch imageByTask[task.ID] {
		case "repo/start":
			s.Nil(task.NextID)
		case "repo/branch-a", "repo/branch-b":
			s.NotNil(task.NextID)
		case "repo/join":
			s.Nil(task.NextID)
		}
	}
}

func (s *ImporterTestSuite) TestApplyPersistsStepNodeSelector() {
	const manifest = `
apiVersion: v1
kind: Job
metadata:
  alias: affinity-job
trigger:
  type: cron
  configuration:
    cron: "* * * * *"
steps:
  - name: build
    image: repo/build
    nodeSelector:
      zone: us-west-2
      disk: ssd
`

	def, err := schema.Parse([]byte(manifest))
	s.Require().NoError(err)

	job, err := s.importer.Apply(context.Background(), def)
	s.Require().NoError(err)

	var tasks []models.Task
	s.Require().NoError(s.db.Where("job_id = ?", job.ID).Find(&tasks).Error)
	s.Require().Len(tasks, 1)
	s.Equal(datatypes.JSONMap{
		"zone": "us-west-2",
		"disk": "ssd",
	}, tasks[0].NodeSelector)
}

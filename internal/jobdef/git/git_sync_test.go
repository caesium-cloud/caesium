package git

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	jobdiff "github.com/caesium-cloud/caesium/internal/jobdef/diff"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	sshauth "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/suite"
	"gorm.io/gorm"
)

type GitSyncSuite struct {
	suite.Suite
}

func TestGitSyncSuite(t *testing.T) {
	suite.Run(t, new(GitSyncSuite))
}

func (s *GitSyncSuite) TestSyncSingleDocument() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs"}

	s.Require().NoError(source.Sync(context.Background(), importer))
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)
	testutil.AssertCount(s.T(), db, &models.Trigger{}, 1)
	testutil.AssertCount(s.T(), db, &models.Atom{}, 3)
	testutil.AssertCount(s.T(), db, &models.Task{}, 3)
}

func (s *GitSyncSuite) TestSyncMultiDocument() {
	multi := testutil.SampleJob + "\n---\n" + strings.Replace(testutil.SampleJob, "csv-to-parquet", "csv-to-parquet-2", 1)
	repoDir := s.initRepo(map[string]string{
		"jobs/multi.yaml": multi,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs"}

	s.Require().NoError(source.Sync(context.Background(), importer))
	testutil.AssertCount(s.T(), db, &models.Job{}, 2)
}

func (s *GitSyncSuite) TestWatchOnce() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	opts := WatchOptions{
		Source: Source{URL: repoDir, Ref: "master", Path: "jobs"},
		Once:   true,
	}

	s.Require().NoError(Watch(context.Background(), importer, opts))
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)
}

func (s *GitSyncSuite) TestWatchDetectsNewCommit() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	opts := WatchOptions{
		Source:   Source{URL: repoDir, Ref: "master", Path: "jobs"},
		Interval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, importer, opts) }()

	s.Eventually(func() bool {
		var count int64
		if err := db.Model(&models.Job{}).Count(&count).Error; err != nil {
			return false
		}
		return count == 1
	}, 2*time.Second, 20*time.Millisecond, "initial sync did not complete")

	// update repo with new alias
	s.commit(repoDir, map[string]string{
		"jobs/sample.yaml": strings.Replace(testutil.SampleJob, "csv-to-parquet", "csv-to-parquet-2", 1),
	})

	s.Eventually(func() bool {
		var count int64
		if err := db.Model(&models.Job{}).Count(&count).Error; err != nil {
			return false
		}
		return count == 2
	}, 2*time.Second, 20*time.Millisecond, "watch did not observe new commit")
	cancel()

	err := <-done
	if err != nil && !errors.Is(err, context.Canceled) {
		s.FailNow("watch returned error", err)
	}
	testutil.AssertCount(s.T(), db, &models.Job{}, 2)
}

func (s *GitSyncSuite) TestWatchCancel() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	opts := WatchOptions{
		Source:   Source{URL: repoDir, Ref: "master", Path: "jobs"},
		Interval: 50 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, importer, opts) }()

	s.Eventually(func() bool {
		var count int64
		if err := db.Model(&models.Job{}).Count(&count).Error; err != nil {
			return false
		}
		return count == 1
	}, 2*time.Second, 20*time.Millisecond, "initial sync did not complete")
	cancel()

	err := <-done
	if err != nil && !errors.Is(err, context.Canceled) {
		s.FailNow("watch returned error", err)
	}
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)
}

func (s *GitSyncSuite) TestSyncRecordsProvenance() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	repo, err := git.PlainOpen(repoDir)
	s.Require().NoError(err)
	ref, err := repo.Head()
	s.Require().NoError(err)
	hash := ref.Hash().String()

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}

	s.Require().NoError(source.Sync(context.Background(), importer))

	var job models.Job
	s.Require().NoError(db.Where("alias = ?", "csv-to-parquet").First(&job).Error)
	s.Equal("git-sync", job.ProvenanceSourceID)
	s.Equal(repoDir, job.ProvenanceRepo)
	s.Equal("master", job.ProvenanceRef)
	s.Equal(hash, job.ProvenanceCommit)
	s.Equal("jobs/sample.yaml", job.ProvenancePath)

	var trigger models.Trigger
	s.Require().NoError(db.Where("alias = ?", "csv-to-parquet").First(&trigger).Error)
	s.Equal("git-sync", trigger.ProvenanceSourceID)
	s.Equal(repoDir, trigger.ProvenanceRepo)
	s.Equal("master", trigger.ProvenanceRef)
	s.Equal(hash, trigger.ProvenanceCommit)
	s.Equal("jobs/sample.yaml#trigger", trigger.ProvenancePath)

	var atoms []models.Atom
	s.Require().NoError(db.Find(&atoms).Error)
	s.Len(atoms, 3)
	names := make(map[string]struct{}, len(atoms))
	for _, atom := range atoms {
		s.Equal("git-sync", atom.ProvenanceSourceID)
		s.Equal(repoDir, atom.ProvenanceRepo)
		s.Equal("master", atom.ProvenanceRef)
		s.Equal(hash, atom.ProvenanceCommit)
		s.True(strings.HasPrefix(atom.ProvenancePath, "jobs/sample.yaml#step/"))
		nameEnc := strings.TrimPrefix(atom.ProvenancePath, "jobs/sample.yaml#step/")
		name, err := url.PathUnescape(nameEnc)
		s.Require().NoError(err)
		names[name] = struct{}{}
	}
	s.Equal(map[string]struct{}{"list": {}, "convert": {}, "publish": {}}, names)
}

func (s *GitSyncSuite) TestSyncDagEdgesSurviveDiff() {
	manifest := `
apiVersion: v1
kind: Job
metadata:
  alias: git-dag
trigger:
  type: cron
  configuration:
    cron: "0 * * * *"
steps:
  - name: start
    image: alpine:3.23
    command: ["sh", "-c", "echo start"]
    next:
      - branch-a
      - branch-b
  - name: branch-a
    image: alpine:3.23
    command: ["sh", "-c", "echo branch-a"]
    dependsOn: start
  - name: branch-b
    image: alpine:3.23
    command: ["sh", "-c", "echo branch-b"]
    dependsOn: start
  - name: join
    image: alpine:3.23
    command: ["sh", "-c", "echo join"]
    dependsOn:
      - branch-a
      - branch-b
`

	repoDir := s.initRepo(map[string]string{
		"jobs/dag.yaml": strings.TrimSpace(manifest) + "\n",
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}

	s.Require().NoError(source.Sync(context.Background(), importer))

	var edges []models.TaskEdge
	s.Require().NoError(db.Find(&edges).Error)
	s.Len(edges, 4)

	paths := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		s.Equal("git-sync", edge.ProvenanceSourceID)
		s.Equal(repoDir, edge.ProvenanceRepo)
		s.Equal("master", edge.ProvenanceRef)
		s.NotEmpty(edge.ProvenanceCommit)
		paths[edge.ProvenancePath] = struct{}{}
	}

	expectedPaths := map[string]struct{}{
		"jobs/dag.yaml#edge/start->branch-a": {},
		"jobs/dag.yaml#edge/start->branch-b": {},
		"jobs/dag.yaml#edge/branch-a->join":  {},
		"jobs/dag.yaml#edge/branch-b->join":  {},
	}
	s.Equal(expectedPaths, paths)

	desired, err := jobdiff.LoadDefinitions([]string{filepath.Join(repoDir, "jobs")})
	s.Require().NoError(err)
	actual, err := jobdiff.LoadDatabaseSpecs(context.Background(), db)
	s.Require().NoError(err)
	diff := jobdiff.Compare(desired, actual)
	s.True(diff.Empty(), "unexpected diff: %+v", diff)
}

func (s *GitSyncSuite) TestSyncWithSourceIDPrunesMissingJobs() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml": testutil.SampleJob,
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}

	s.Require().NoError(source.Sync(context.Background(), importer))
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)

	s.commit(repoDir, map[string]string{
		"jobs/sample.yaml": strings.Replace(testutil.SampleJob, "csv-to-parquet", "csv-to-parquet-2", 1),
	})

	s.Require().NoError(source.Sync(context.Background(), importer))
	testutil.AssertCount(s.T(), db, &models.Job{}, 1)

	var totalJobs int64
	s.Require().NoError(db.Unscoped().Model(&models.Job{}).Count(&totalJobs).Error)
	s.Equal(int64(2), totalJobs)
}

func (s *GitSyncSuite) TestSyncPathGlobs() {
	repoDir := s.initRepo(map[string]string{
		"jobs/sample.yaml":      testutil.SampleJob,
		"jobs/include.job.yaml": strings.Replace(testutil.SampleJob, "csv-to-parquet", "csv-to-parquet-2", 1),
	})

	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })

	importer := jobdef.NewImporter(db)
	source := Source{
		URL:   repoDir,
		Ref:   "master",
		Path:  "jobs",
		Globs: []string{"**/*.job.yaml"},
	}

	s.Require().NoError(source.Sync(context.Background(), importer))

	var jobs []models.Job
	s.Require().NoError(db.Find(&jobs).Error)
	s.Len(jobs, 1)
	s.Equal("csv-to-parquet-2", jobs[0].Alias)
}

func (s *GitSyncSuite) TestCloneOptionsBasicAuthSecrets() {
	resolver := staticResolver{
		"secret://env/GIT_USERNAME": "git-user",
		"secret://env/GIT_PASSWORD": "top-secret",
	}

	source := Source{
		URL: "https://example.com/repo.git",
		Auth: &BasicAuth{
			UsernameRef: "secret://env/GIT_USERNAME",
			PasswordRef: "secret://env/GIT_PASSWORD",
		},
		Resolver: resolver,
	}

	opts, cleanup, err := source.cloneOptions(context.Background())
	s.Require().NoError(err)
	s.Require().NotNil(cleanup)
	cleanup()

	auth, ok := opts.Auth.(*httpauth.BasicAuth)
	s.Require().True(ok)
	s.Equal("git-user", auth.Username)
	s.Equal("top-secret", auth.Password)
}

func (s *GitSyncSuite) TestCloneOptionsBasicAuthMissingResolver() {
	source := Source{
		URL:  "https://example.com/repo.git",
		Auth: &BasicAuth{UsernameRef: "secret://missing"},
	}

	_, _, err := source.cloneOptions(context.Background())
	s.Require().Error(err)
	s.Contains(err.Error(), "secret resolver")
}

func (s *GitSyncSuite) TestCloneOptionsSSHWithSecrets() {
	key := mustGeneratePrivateKey(s.T())
	resolver := staticResolver{
		"secret://env/SSH_USERNAME": "git",
		"secret://env/SSH_KEY":      key,
		"secret://env/SSH_KNOWN":    githubKnownHostEntry(),
	}

	source := Source{
		URL: "git@github.com:caesium/test.git",
		SSH: &SSHAuth{
			UsernameRef:   "secret://env/SSH_USERNAME",
			PrivateKeyRef: "secret://env/SSH_KEY",
			KnownHostsRef: "secret://env/SSH_KNOWN",
		},
		Resolver: resolver,
	}

	opts, cleanup, err := source.cloneOptions(context.Background())
	s.Require().NoError(err)
	s.Require().NotNil(cleanup)
	defer cleanup()

	pk, ok := opts.Auth.(*sshauth.PublicKeys)
	s.Require().True(ok)
	s.Equal("git", pk.User)
	s.NotNil(pk.Signer)
	s.NotNil(pk.HostKeyCallback)
}

func (s *GitSyncSuite) TestCloneOptionsSSHKnownHostsPath() {
	key := mustGeneratePrivateKey(s.T())
	knownPath := filepath.Join(s.T().TempDir(), "known_hosts")
	s.Require().NoError(os.WriteFile(knownPath, []byte(githubKnownHostEntry()+"\n"), 0o600))

	source := Source{
		URL: "git@github.com:caesium/test.git",
		SSH: &SSHAuth{
			PrivateKey:     key,
			KnownHostsPath: knownPath,
		},
	}

	opts, cleanup, err := source.cloneOptions(context.Background())
	s.Require().NoError(err)
	s.Require().NotNil(cleanup)
	cleanup()

	pk, ok := opts.Auth.(*sshauth.PublicKeys)
	s.Require().True(ok)
	s.NotNil(pk.HostKeyCallback)

	_, statErr := os.Stat(knownPath)
	s.NoError(statErr)
}

func (s *GitSyncSuite) TestCloneOptionsSSHWithoutKnownHosts() {
	source := Source{
		URL: "git@github.com:caesium/test.git",
		SSH: &SSHAuth{
			PrivateKey: mustGeneratePrivateKey(s.T()),
		},
	}

	_, _, err := source.cloneOptions(context.Background())
	s.Require().Error(err)
	s.Contains(err.Error(), "known hosts")
}

func (s *GitSyncSuite) TestResolveSecretSuccess() {
	source := Source{Resolver: staticResolver{"secret://env/USER": "robot"}}
	value, err := source.resolveSecret(context.Background(), "secret://env/USER")
	s.Require().NoError(err)
	s.Equal("robot", value)
}

func (s *GitSyncSuite) TestResolveSecretMissingResolver() {
	source := Source{}
	_, err := source.resolveSecret(context.Background(), "secret://env/USER")
	s.Require().Error(err)
	s.Contains(err.Error(), "secret resolver not configured")
}

func (s *GitSyncSuite) TestResolveSecretResolverError() {
	source := Source{Resolver: staticResolver{}}
	_, err := source.resolveSecret(context.Background(), "secret://env/MISSING")
	s.Require().Error(err)
	s.Contains(err.Error(), "secret://env/MISSING")
}

// allowlistManifest is SampleJob under alias, in namespace when non-empty.
func allowlistManifest(alias, namespace string) string {
	meta := "  alias: " + alias + "\n"
	if namespace != "" {
		meta += "  namespace: " + namespace + "\n"
	}
	return strings.Replace(testutil.SampleJob, "  alias: csv-to-parquet\n", meta, 1)
}

type allowlistAudit struct {
	models.AuditLog
	Meta struct {
		URL          string              `json:"url"`
		Ref          string              `json:"ref"`
		Commit       string              `json:"commit"`
		SourceID     string              `json:"source_id"`
		Namespaces   []string            `json:"namespaces"`
		Applied      int                 `json:"applied"`
		Violations   []ManifestViolation `json:"violations"`
		Pruned       int                 `json:"pruned"`
		PruneSkipped []jobdef.PruneSkip  `json:"prune_skipped"`
		Error        *string             `json:"error"`
	}
}

func (s *GitSyncSuite) allowlistAudits(db *gorm.DB) []allowlistAudit {
	var rows []models.AuditLog
	s.Require().NoError(db.Where("actor = ? AND action = ?", "system:git-sync", "jobdef.git_sync").Order("timestamp").Find(&rows).Error)
	out := make([]allowlistAudit, 0, len(rows))
	for _, row := range rows {
		entry := allowlistAudit{AuditLog: row}
		s.Require().NoError(json.Unmarshal(row.Metadata, &entry.Meta))
		var raw map[string]json.RawMessage
		s.Require().NoError(json.Unmarshal(row.Metadata, &raw))
		for _, key := range []string{"violations", "prune_skipped"} {
			s.True(strings.HasPrefix(string(raw[key]), "["), "%s must be a JSON array, got %s", key, raw[key])
		}
		out = append(out, entry)
	}
	return out
}

func (s *GitSyncSuite) allowlistJob(db *gorm.DB, alias string) models.Job {
	var job models.Job
	s.Require().NoError(db.Unscoped().Where("alias = ?", alias).First(&job).Error)
	return job
}

func (s *GitSyncSuite) headCommit(repoDir string) string {
	repo, err := git.PlainOpen(repoDir)
	s.Require().NoError(err)
	ref, err := repo.Head()
	s.Require().NoError(err)
	return ref.Hash().String()
}

func (s *GitSyncSuite) removeAndCommit(repoDir string, rels ...string) {
	repo, err := git.PlainOpen(repoDir)
	s.Require().NoError(err)
	wt, err := repo.Worktree()
	s.Require().NoError(err)
	for _, rel := range rels {
		_, err = wt.Remove(rel)
		s.Require().NoError(err)
	}
	_, err = wt.Commit("remove", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	s.Require().NoError(err)
}

func (s *GitSyncSuite) allowlistSetup(files map[string]string) (string, *gorm.DB, *jobdef.Importer) {
	files["jobs/keep.txt"] = "keeps jobs/ present after every manifest is removed\n"
	repoDir := s.initRepo(files)
	db := testutil.OpenTestDB(s.T())
	s.T().Cleanup(func() { testutil.CloseDB(db) })
	return repoDir, db, jobdef.NewImporter(db)
}

func narrowedSource(repoDir string) Source {
	return Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync", Namespaces: []string{"default"}}
}

func (s *GitSyncSuite) TestSyncAllowlistSkipsForeignManifest() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml":   allowlistManifest("ns-default", ""),
		"jobs/marketing.yaml": allowlistManifest("ns-marketing", "marketing"),
	})
	source := narrowedSource(repoDir)

	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal(1, report.Applied)
	s.Equal([]ManifestViolation{{Path: "jobs/marketing.yaml", Alias: "ns-marketing", Namespace: "marketing",
		Reason: jobdef.NamespaceReasonRequested}}, report.Violations)
	s.Equal("default", s.allowlistJob(db, "ns-default").Namespace)
	var count int64
	s.Require().NoError(db.Unscoped().Model(&models.Job{}).Where("alias = ?", "ns-marketing").Count(&count).Error)
	s.Zero(count)

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 1)
	s.Equal("denied", audits[0].Outcome)
	s.Equal("git-sync", audits[0].ResourceID)
	s.Equal("git_source", audits[0].ResourceType)
	s.Equal(repoDir, audits[0].Meta.URL)
	s.Equal(s.headCommit(repoDir), audits[0].Meta.Commit)
	s.Equal([]string{"default"}, audits[0].Meta.Namespaces)
	s.Equal(1, audits[0].Meta.Applied)
	s.Equal(report.Violations, audits[0].Meta.Violations)
	s.NotNil(audits[0].Meta.PruneSkipped)
	s.Empty(audits[0].Meta.PruneSkipped)
	s.Nil(audits[0].Meta.Error)
}

func (s *GitSyncSuite) TestSyncAllowlistNarrowingKeepsExistingJob() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml":   allowlistManifest("ns-default", ""),
		"jobs/marketing.yaml": allowlistManifest("ns-marketing", "marketing"),
	})
	unrestricted := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}
	s.Require().NoError(unrestricted.Sync(context.Background(), importer))
	before := s.allowlistJob(db, "ns-marketing")

	s.commit(repoDir, map[string]string{"jobs/marketing.yaml": strings.Replace(allowlistManifest("ns-marketing", "marketing"), "echo list", "echo changed", 1)})
	narrowed := narrowedSource(repoDir)
	report, err := narrowed.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal(0, report.Pruned)
	s.Require().Len(report.Violations, 1)
	s.Equal("ns-marketing", report.Violations[0].Alias)

	after := s.allowlistJob(db, "ns-marketing")
	s.False(after.DeletedAt.Valid, "a refused manifest's job must not be pruned")
	s.Equal(before.ID, after.ID)
	s.Equal(before.ProvenanceCommit, after.ProvenanceCommit)
	s.Equal("marketing", after.Namespace)
}

func (s *GitSyncSuite) TestSyncAllowlistForbiddenMoveLeavesJobActive() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml": allowlistManifest("ns-default", ""),
	})
	source := narrowedSource(repoDir)
	s.Require().NoError(source.Sync(context.Background(), importer))
	before := s.allowlistJob(db, "ns-default")

	s.commit(repoDir, map[string]string{"jobs/default.yaml": allowlistManifest("ns-default", "marketing")})
	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal([]ManifestViolation{{Path: "jobs/default.yaml", Alias: "ns-default", Namespace: "marketing",
		Reason: jobdef.NamespaceReasonRequested}}, report.Violations)
	s.Equal(0, report.Applied)
	s.Equal(0, report.Pruned)

	after := s.allowlistJob(db, "ns-default")
	s.False(after.DeletedAt.Valid)
	s.Equal(before.ID, after.ID)
	s.Equal("default", after.Namespace)
	s.Equal(before.ProvenanceCommit, after.ProvenanceCommit)
}

func (s *GitSyncSuite) TestSyncAllowlistStoredNamespaceSkipped() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/mover.yaml": allowlistManifest("ns-mover", "marketing"),
	})
	unrestricted := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}
	s.Require().NoError(unrestricted.Sync(context.Background(), importer))
	before := s.allowlistJob(db, "ns-mover")

	// The narrowed source cannot pull its old job into its allowlist.
	s.commit(repoDir, map[string]string{"jobs/mover.yaml": allowlistManifest("ns-mover", "")})
	narrowed := narrowedSource(repoDir)
	report, err := narrowed.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal([]ManifestViolation{{Path: "jobs/mover.yaml", Alias: "ns-mover", Namespace: "default",
		StoredNamespace: "marketing", Reason: jobdef.NamespaceReasonStored}}, report.Violations)

	after := s.allowlistJob(db, "ns-mover")
	s.False(after.DeletedAt.Valid)
	s.Equal("marketing", after.Namespace)
	s.Equal(before.ProvenanceCommit, after.ProvenanceCommit)
}

func (s *GitSyncSuite) TestSyncAllowlistStoredViolatorSkipsValidation() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml": allowlistManifest("ns-default", ""),
		"jobs/mover.yaml":   allowlistManifest("ns-mover", "marketing"),
	})
	unrestricted := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}
	s.Require().NoError(unrestricted.Sync(context.Background(), importer))

	invalid := strings.Replace(allowlistManifest("ns-mover", ""), "    image: busybox:1.36.1\n", "", 1)
	s.commit(repoDir, map[string]string{
		"jobs/mover.yaml":   invalid,
		"jobs/default.yaml": strings.Replace(allowlistManifest("ns-default", ""), "echo list", "echo changed", 1),
	})
	narrowed := narrowedSource(repoDir)
	report, err := narrowed.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err, "a refused proposal must never reach ValidateBatch")
	s.Equal(1, report.Applied)
	s.Require().Len(report.Violations, 1)
	s.Equal(jobdef.NamespaceReasonStored, report.Violations[0].Reason)
	s.Equal(s.headCommit(repoDir), s.allowlistJob(db, "ns-default").ProvenanceCommit)
}

// allowlistChainManifest is a cron job, or with upstream an event job that
// runs when upstream's run completes.
func allowlistChainManifest(alias, upstream string) string {
	trigger := "trigger:\n  type: cron\n  configuration:\n    cron: \"0 * * * *\"\n"
	if upstream != "" {
		trigger = "trigger:\n  type: event\n  configuration:\n    events:\n      - type: run_completed\n        source: caesium\n        filter:\n          job_alias: " + upstream + "\n"
	}
	return "apiVersion: v1\nkind: Job\nmetadata:\n  alias: " + alias + "\n" + trigger +
		"steps:\n  - name: run\n    image: busybox:1.36.1\n    command: [\"sh\", \"-c\", \"echo ok\"]\n"
}

// allowlistMoveBeforeBatch moves alias out of the allowlist after the
// stored-namespace pre-filter, just before the batch transaction, as a
// concurrent REST apply would.
func (s *GitSyncSuite) allowlistMoveBeforeBatch(db *gorm.DB, alias string) {
	applyHookForTest = func() {
		s.Require().NoError(db.Model(&models.Job{}).Where("alias = ?", alias).Update("namespace", "marketing").Error)
	}
	s.T().Cleanup(func() { applyHookForTest = nil })
}

func (s *GitSyncSuite) TestSyncAllowlistInTransactionRefusalIsSkip() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml": allowlistManifest("ns-default", ""),
	})
	source := narrowedSource(repoDir)
	s.Require().NoError(source.Sync(context.Background(), importer))
	before := s.allowlistJob(db, "ns-default")

	s.commit(repoDir, map[string]string{"jobs/default.yaml": strings.Replace(allowlistManifest("ns-default", ""), "echo list", "echo changed", 1)})
	s.allowlistMoveBeforeBatch(db, "ns-default")

	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal(0, report.Applied)
	s.Equal([]ManifestViolation{{Path: "jobs/default.yaml", Alias: "ns-default", Namespace: "default",
		StoredNamespace: "marketing", Reason: jobdef.NamespaceReasonStored}}, report.Violations)
	after := s.allowlistJob(db, "ns-default")
	s.Equal("marketing", after.Namespace)
	s.Equal(before.ProvenanceCommit, after.ProvenanceCommit)
}

// An in-transaction refusal that leaves a valid graph: the rest of the commit
// applies and the refusal is reported.
func (s *GitSyncSuite) TestSyncAllowlistInTransactionRefusalKeepsValidBatch() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/x.yaml": allowlistChainManifest("ns-x", ""),
		"jobs/y.yaml": allowlistChainManifest("ns-y", ""),
	})
	source := narrowedSource(repoDir)
	s.Require().NoError(source.Sync(context.Background(), importer))
	yBefore := s.allowlistJob(db, "ns-y")

	s.commit(repoDir, map[string]string{
		"jobs/x.yaml": allowlistChainManifest("ns-x", "ns-y"),
		"jobs/y.yaml": strings.Replace(allowlistChainManifest("ns-y", ""), "echo ok", "echo changed", 1),
	})
	s.allowlistMoveBeforeBatch(db, "ns-y")

	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal(1, report.Applied)
	s.Equal([]ManifestViolation{{Path: "jobs/y.yaml", Alias: "ns-y", Namespace: "default",
		StoredNamespace: "marketing", Reason: jobdef.NamespaceReasonStored}}, report.Violations)
	s.Equal(s.headCommit(repoDir), s.allowlistJob(db, "ns-x").ProvenanceCommit)
	yAfter := s.allowlistJob(db, "ns-y")
	s.Equal(yBefore.ProvenanceCommit, yAfter.ProvenanceCommit)
	s.Equal("marketing", yAfter.Namespace)

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 2)
	s.Equal("denied", audits[1].Outcome)
}

// requireBatchRolledBack asserts the stored jobs still hold their earlier
// definitions: nothing from the refused batch persisted.
func (s *GitSyncSuite) requireBatchRolledBack(db *gorm.DB, before map[string]models.Job) {
	for alias, job := range before {
		after := s.allowlistJob(db, alias)
		s.Equal(job.ProvenanceCommit, after.ProvenanceCommit, "%s must keep its stored definition", alias)
		s.Equal(job.TriggerID, after.TriggerID, alias)
	}
}

// A refusal of the first manifest whose live row makes the rest of the batch a
// cycle: nothing from the commit is applied.
func (s *GitSyncSuite) TestSyncAllowlistFirstManifestRefusalRollsBackInvalidBatch() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/a.yaml": allowlistChainManifest("ns-a", "ns-b"),
		"jobs/b.yaml": allowlistChainManifest("ns-b", ""),
	})
	source := narrowedSource(repoDir)
	s.Require().NoError(source.Sync(context.Background(), importer))
	before := map[string]models.Job{"ns-a": s.allowlistJob(db, "ns-a"), "ns-b": s.allowlistJob(db, "ns-b")}

	// The commit reverses the chain: valid as a batch, a cycle against a's
	// live row once a is refused.
	s.commit(repoDir, map[string]string{
		"jobs/a.yaml": allowlistChainManifest("ns-a", ""),
		"jobs/b.yaml": allowlistChainManifest("ns-b", "ns-a"),
	})
	s.allowlistMoveBeforeBatch(db, "ns-a")

	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().ErrorIs(err, jobdef.ErrTriggerChainCycle)
	s.Require().ErrorIs(err, jobdef.ErrBatchInvalid)
	s.Equal(0, report.Applied)
	s.Require().Len(report.Violations, 1)
	s.Equal("ns-a", report.Violations[0].Alias)
	s.requireBatchRolledBack(db, before)

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 2)
	s.Equal("error", audits[1].Outcome)
}

// A later manifest refused after an earlier one was written in the same
// batch: the batch rolls back, so the stored graph stays as it was (valid)
// and the earlier manifest is not committed either.
func (s *GitSyncSuite) TestSyncAllowlistLaterManifestRefusalRollsBackInvalidBatch() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/a.yaml": allowlistChainManifest("ns-a", ""),
		"jobs/b.yaml": allowlistChainManifest("ns-b", "ns-a"),
	})
	source := narrowedSource(repoDir)
	s.Require().NoError(source.Sync(context.Background(), importer))
	before := map[string]models.Job{"ns-a": s.allowlistJob(db, "ns-a"), "ns-b": s.allowlistJob(db, "ns-b")}

	// Valid as a batch: a now listens for b, and b becomes cron.
	s.commit(repoDir, map[string]string{
		"jobs/a.yaml": allowlistChainManifest("ns-a", "ns-b"),
		"jobs/b.yaml": allowlistChainManifest("ns-b", ""),
	})
	s.allowlistMoveBeforeBatch(db, "ns-b")

	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().ErrorIs(err, jobdef.ErrTriggerChainCycle)
	s.Contains(err.Error(), `namespace refusal of ["ns-b"] leaves the batch invalid, so nothing from this commit was applied`)
	s.Contains(err.Error(), "move the job back into the allowlist")
	s.Equal(0, report.Applied)
	s.requireBatchRolledBack(db, before)
	s.Equal("marketing", s.allowlistJob(db, "ns-b").Namespace)
	// The stored graph is still valid: a cron, b listening for a.
	s.Require().NoError(jobdef.ValidateTriggerChains(context.Background(), db, nil))

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 2)
	s.Equal("error", audits[1].Outcome)
	s.Equal(0, audits[1].Meta.Applied)
	s.Require().Len(audits[1].Meta.Violations, 1)
	s.Equal(jobdef.NamespaceReasonStored, audits[1].Meta.Violations[0].Reason)
}

// A non-namespace error later in the batch rolls back the earlier manifests
// of a restricted source too: all allowed manifests or none.
func (s *GitSyncSuite) TestSyncAllowlistBatchRollsBackOnApplyError() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/a.yaml": allowlistChainManifest("ns-a", ""),
		"jobs/b.yaml": allowlistChainManifest("ns-b", ""),
	})
	foreign, err := schema.Parse([]byte(allowlistChainManifest("ns-b", "")))
	s.Require().NoError(err)
	_, err = importer.ApplyWithOptions(context.Background(), foreign, &jobdef.ApplyOptions{Provenance: &jobdef.Provenance{SourceID: "git-other"}})
	s.Require().NoError(err)

	source := narrowedSource(repoDir)
	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().ErrorIs(err, jobdef.ErrProvenanceConflict)
	s.Equal(0, report.Applied)
	var count int64
	s.Require().NoError(db.Unscoped().Model(&models.Job{}).Where("alias = ?", "ns-a").Count(&count).Error)
	s.Zero(count, "a must roll back with the batch")

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 1)
	s.Equal("error", audits[0].Outcome)
}

// The same conflict without a race: b was moved out of the allowlist before
// the commit, so the pre-filter skips it and a's proposal is validated against
// b's live row. The sync fails instead of applying a into a cycle.
func (s *GitSyncSuite) TestSyncAllowlistSkippedStoredRowConflictFailsLoudly() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/a.yaml": allowlistChainManifest("ns-a", ""),
		"jobs/b.yaml": allowlistChainManifest("ns-b", "ns-a"),
	})
	source := narrowedSource(repoDir)
	s.Require().NoError(source.Sync(context.Background(), importer))
	aBefore := s.allowlistJob(db, "ns-a")
	s.Require().NoError(db.Model(&models.Job{}).Where("alias = ?", "ns-b").Update("namespace", "marketing").Error)

	s.commit(repoDir, map[string]string{"jobs/a.yaml": allowlistChainManifest("ns-a", "ns-b")})
	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().ErrorIs(err, jobdef.ErrTriggerChainCycle)
	s.Contains(err.Error(), `namespace-skipped manifests ["ns-b"] keep their stored jobs`)
	s.Equal(0, report.Applied)
	s.Equal(aBefore.ProvenanceCommit, s.allowlistJob(db, "ns-a").ProvenanceCommit, "a must not apply into a cycle")

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 2)
	s.Equal("error", audits[1].Outcome)
	s.Require().Len(audits[1].Meta.Violations, 1)
	s.Equal("ns-b", audits[1].Meta.Violations[0].Alias)
}

func (s *GitSyncSuite) TestSyncAllowlistDuplicateAliasStaysFatal() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/a.yaml": allowlistManifest("ns-dup", "marketing"),
		"jobs/b.yaml": allowlistManifest("ns-dup", ""),
	})
	source := narrowedSource(repoDir)
	_, err := source.SyncWithReport(context.Background(), importer)
	s.Require().ErrorIs(err, jobdef.ErrDuplicateJob)
	s.Contains(err.Error(), "jobs/a.yaml and jobs/b.yaml")
	testutil.AssertCount(s.T(), db, &models.Job{}, 0)

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 1)
	s.Equal("error", audits[0].Outcome)
	s.Require().NotNil(audits[0].Meta.Error)
	s.Contains(*audits[0].Meta.Error, "ns-dup")
	s.Empty(audits[0].Meta.Violations)
}

func (s *GitSyncSuite) TestSyncAllowlistPruneLeavesForeignJob() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml":   allowlistManifest("ns-default", ""),
		"jobs/marketing.yaml": allowlistManifest("ns-marketing", "marketing"),
	})
	unrestricted := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}
	s.Require().NoError(unrestricted.Sync(context.Background(), importer))

	s.removeAndCommit(repoDir, "jobs/default.yaml", "jobs/marketing.yaml")
	narrowed := narrowedSource(repoDir)
	report, err := narrowed.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal(1, report.Pruned)
	s.Equal([]jobdef.PruneSkip{{Alias: "ns-marketing", Namespace: "marketing"}}, report.PruneSkipped)
	s.Empty(report.Violations)
	s.True(s.allowlistJob(db, "ns-default").DeletedAt.Valid)
	s.False(s.allowlistJob(db, "ns-marketing").DeletedAt.Valid)

	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 1)
	s.Equal("denied", audits[0].Outcome)
	s.Equal(1, audits[0].Meta.Pruned)
	s.Equal(report.PruneSkipped, audits[0].Meta.PruneSkipped)
}

func (s *GitSyncSuite) TestSyncUnrestrictedSourceUnchanged() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml":   allowlistManifest("ns-default", ""),
		"jobs/marketing.yaml": allowlistManifest("ns-marketing", "marketing"),
	})
	source := Source{URL: repoDir, Ref: "master", Path: "jobs", SourceID: "git-sync"}
	report, err := source.SyncWithReport(context.Background(), importer)
	s.Require().NoError(err)
	s.Equal(2, report.Applied)
	s.Empty(report.Violations)
	s.Nil(report.Namespaces)
	testutil.AssertCount(s.T(), db, &models.Job{}, 2)
	// Only allowlisted sources are audited.
	testutil.AssertCount(s.T(), db, &models.AuditLog{}, 0)
}

func (s *GitSyncSuite) TestWatchNamespaceViolationKeepsWatching() {
	repoDir, db, importer := s.allowlistSetup(map[string]string{
		"jobs/default.yaml":   allowlistManifest("ns-default", ""),
		"jobs/marketing.yaml": allowlistManifest("ns-marketing", "marketing"),
	})
	opts := WatchOptions{Source: narrowedSource(repoDir), Interval: 50 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, importer, opts) }()

	auditCount := func(want int64) func() bool {
		return func() bool {
			var count int64
			return db.Model(&models.AuditLog{}).Count(&count).Error == nil && count == want
		}
	}
	s.Eventually(auditCount(1), 5*time.Second, 20*time.Millisecond, "initial restricted sync was not audited")
	s.commit(repoDir, map[string]string{"jobs/later.yaml": allowlistManifest("ns-later", "")})
	s.Eventually(auditCount(2), 5*time.Second, 20*time.Millisecond, "watcher stopped after a namespace violation")
	time.Sleep(200 * time.Millisecond) // several ticks on an unchanged HEAD
	cancel()
	s.Require().ErrorIs(<-done, context.Canceled)

	s.False(s.allowlistJob(db, "ns-later").DeletedAt.Valid)
	audits := s.allowlistAudits(db)
	s.Require().Len(audits, 2, "each commit is reported once, not every tick")
	s.Equal("denied", audits[1].Outcome)
	s.Equal(2, audits[1].Meta.Applied)
}

func (s *GitSyncSuite) TestRedactSourceURL() {
	s.Equal("https://h/r.git", redactSourceURL("https://u:p@h/r.git?t=x#frag"))
	s.Equal("git://host:9418/coverage.git", redactSourceURL("git://host:9418/coverage.git"))
	s.Equal("git@github.com:org/repo.git", redactSourceURL("git@github.com:org/repo.git"))
	s.Equal("/tmp/repo dir", redactSourceURL("/tmp/repo dir"))
}

func (s *GitSyncSuite) initRepo(files map[string]string) string {
	dir := s.T().TempDir()
	repo, err := git.PlainInit(dir, false)
	s.Require().NoError(err)

	for rel, content := range files {
		path := filepath.Join(dir, rel)
		s.Require().NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		s.Require().NoError(os.WriteFile(path, []byte(content), 0o644))
	}

	wt, err := repo.Worktree()
	s.Require().NoError(err)
	_, err = wt.Add(".")
	s.Require().NoError(err)

	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	s.Require().NoError(err)

	return dir
}

func (s *GitSyncSuite) commit(repoDir string, files map[string]string) {
	repo, err := git.PlainOpen(repoDir)
	s.Require().NoError(err)

	wt, err := repo.Worktree()
	s.Require().NoError(err)

	for rel, content := range files {
		path := filepath.Join(repoDir, rel)
		s.Require().NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		s.Require().NoError(os.WriteFile(path, []byte(content), 0o644))
		_, err = wt.Add(rel)
		s.Require().NoError(err)
	}

	_, err = wt.Commit("update", &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@example.com", When: time.Now()},
	})
	s.Require().NoError(err)
}

type staticResolver map[string]string

func (sr staticResolver) Resolve(_ context.Context, ref string) (string, error) {
	value, _, err := sr.ResolveWithIdentity(context.Background(), ref)
	return value, err
}

func (sr staticResolver) ResolveWithIdentity(_ context.Context, ref string) (string, secret.Identity, error) {
	if val, ok := sr[ref]; ok {
		return val, secret.Identity{Provider: "env", Ref: ref, Verifiable: false, UnverifiableReason: "test stub"}, nil
	}
	return "", secret.Identity{}, fmt.Errorf("secret %q not found", ref)
}

func (sr staticResolver) CanonicalTarget(string) (string, error) {
	return "", errors.New("canonical target unsupported by test stub")
}

func mustGeneratePrivateKey(tb testing.TB) string {
	tb.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		tb.Fatalf("generate private key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	blk := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(blk))
}

func githubKnownHostEntry() string {
	return "github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk="
}

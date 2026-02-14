package git

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
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
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	sshauth "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/suite"
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
    image: alpine:3.20
    command: ["sh", "-c", "echo start"]
    next:
      - branch-a
      - branch-b
  - name: branch-a
    image: alpine:3.20
    command: ["sh", "-c", "echo branch-a"]
    dependsOn: start
  - name: branch-b
    image: alpine:3.20
    command: ["sh", "-c", "echo branch-b"]
    dependsOn: start
  - name: join
    image: alpine:3.20
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
	if val, ok := sr[ref]; ok {
		return val, nil
	}
	return "", fmt.Errorf("secret %q not found", ref)
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
